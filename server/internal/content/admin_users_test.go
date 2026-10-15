package content

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Restrictions are the sharpest thing an administrator can do to a person, so the tests are about the three
// promises the implementation makes: a ban takes effect immediately, a revocation is recorded rather than
// deleted, and every change leaves an audit row.

func decodeRestriction(t *testing.T, res response) map[string]any {
	t.Helper()
	var item map[string]any
	if err := json.Unmarshal(res.body, &item); err != nil {
		t.Fatalf("cannot decode %s: %v", string(res.body), err)
	}
	return item
}

func TestBanTakesEffectImmediatelyAndRevocationIsRecorded(t *testing.T) {
	env := newTopicEnv(t)
	member := env.newUser("member")
	memberCookie, memberCSRF := env.sessionCookie(member)
	admin := env.newAdmin("moderator")
	adminCookie, adminCSRF := env.sessionCookie(admin)

	// The account can act before the ban.
	expectStatus(t, env.do("POST", "/api/v1/topics",
		`{"board_id":"`+itoa(env.boardID)+`","title":"被封前","body_md":"正文"}`, memberCookie, memberCSRF), 201)

	ban := env.do("POST", "/api/v1/admin/restrictions",
		`{"user_id":"`+itoa(member)+`","kind":"ban","reason":"持续发布违规内容"}`, adminCookie, adminCSRF)
	expectStatus(t, ban, 201)
	created := decodeRestriction(t, ban)
	if created["kind"] != restrictionBan || created["user_id"] != itoa(member) {
		t.Fatalf("the response does not describe the ban: %v", created)
	}
	if created["starts_at"] == nil {
		t.Fatalf("the ban has no start time: %v", created)
	}
	if _, permanent := created["ends_at"]; permanent {
		t.Fatalf("a ban without an end was given one: %v", created)
	}
	restrictionID := created["id"].(string)

	// The session the member already held is gone: that is what makes the ban take effect now rather than at
	// the next login. The request is answered as unauthenticated, not as forbidden.
	after := env.do("POST", "/api/v1/topics",
		`{"board_id":"`+itoa(env.boardID)+`","title":"被封后","body_md":"正文"}`, memberCookie, memberCSRF)
	expectStatus(t, after, 401)
	expectCode(t, after, "UNAUTHENTICATED")
	var sessions int
	if err := env.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE user_id=?`, member).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions != 0 {
		t.Fatalf("the banned account still holds %d sessions", sessions)
	}

	// Revocation records a time instead of deleting the row, and writes its own audit entry.
	expectStatus(t, env.do("DELETE", "/api/v1/admin/restrictions/"+restrictionID, "", adminCookie, adminCSRF), 204)
	var revokedAt *time.Time
	var rows int
	if err := env.db.QueryRow(`SELECT revoked_at FROM user_restrictions WHERE id=?`, restrictionID).Scan(&revokedAt); err != nil {
		t.Fatal(err)
	}
	if revokedAt == nil {
		t.Fatal("revoking did not record a time")
	}
	if err := env.db.QueryRow(`SELECT COUNT(*) FROM user_restrictions WHERE id=?`, restrictionID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatal("revoking deleted the restriction instead of recording it")
	}
	// The record stays readable, now with its revocation visible.
	page := env.getAs("/api/v1/admin/restrictions", adminCookie)
	expectStatus(t, page, 200)
	if !strings.Contains(string(page.body), restrictionID) || !strings.Contains(string(page.body), "revoked_at") {
		t.Fatalf("the revoked restriction is not in the list with its revocation: %s", string(page.body))
	}
	// Revoking again is a no-op, not an error: the caller asked for a state that already holds.
	expectStatus(t, env.do("DELETE", "/api/v1/admin/restrictions/"+restrictionID, "", adminCookie, adminCSRF), 204)

	// Every change is in the audit trail: ban, then revoke.
	rows2, err := env.db.Query(`SELECT action FROM moderation_actions WHERE target_user_id=? ORDER BY id`, member)
	if err != nil {
		t.Fatal(err)
	}
	actions, err := scanRows(rows2, func(rows *sql.Rows) (string, error) {
		var action string
		return action, rows.Scan(&action)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 2 || actions[0] != actionBanUser || actions[1] != actionRevokeRestriction {
		t.Fatalf("audit actions are %v", actions)
	}

	// A muted account keeps its sessions: a mute limits speech, it does not sign anybody out.
	muted := env.newUser("muted")
	env.sessionCookie(muted)
	expectStatus(t, env.do("POST", "/api/v1/admin/restrictions",
		`{"user_id":"`+itoa(muted)+`","kind":"mute","reason":"人身攻击","ends_at":"`+time.Now().UTC().Add(24*time.Hour).Format(time.RFC3339)+`"}`,
		adminCookie, adminCSRF), 201)
	if err := env.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE user_id=?`, muted).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions == 0 {
		t.Fatal("muting signed the account out, which only a ban should do")
	}
}

func TestRestrictionAuthorizationAndBoundaries(t *testing.T) {
	env := newTopicEnv(t)
	member := env.newUser("member")
	adminCookie, adminCSRF := env.sessionCookie(env.newAdmin("moderator"))
	memberCookie, memberCSRF := env.sessionCookie(member)
	path := "/api/v1/admin/restrictions"
	body := `{"user_id":"` + itoa(member) + `","kind":"mute","reason":"原因"}`

	expectStatus(t, env.do("POST", path, body, "", ""), 401)
	expectStatus(t, env.do("POST", path, body, memberCookie, memberCSRF), 403)
	expectStatus(t, env.do("POST", path, body, adminCookie, ""), 403)
	expectStatus(t, env.get("/api/v1/admin/restrictions"), 401)
	expectStatus(t, env.getAs("/api/v1/admin/restrictions", memberCookie), 403)
	expectStatus(t, env.getAs("/api/v1/admin/users", memberCookie), 403)

	// Boundaries: an unknown kind, a missing or blank reason, a bad id, and an end time that is not in the
	// future (a restriction that is already over reads as a mistake, not as an intention).
	expectStatus(t, env.do("POST", path, `{"user_id":"`+itoa(member)+`","kind":"jail","reason":"原因"}`, adminCookie, adminCSRF), 422)
	expectStatus(t, env.do("POST", path, `{"user_id":"`+itoa(member)+`","kind":"mute"}`, adminCookie, adminCSRF), 422)
	expectStatus(t, env.do("POST", path, `{"user_id":"`+itoa(member)+`","kind":"mute","reason":"   "}`, adminCookie, adminCSRF), 422)
	expectStatus(t, env.do("POST", path, `{"user_id":"abc","kind":"mute","reason":"原因"}`, adminCookie, adminCSRF), 422)
	expectStatus(t, env.do("POST", path, `{"user_id":"99999999","kind":"mute","reason":"原因"}`, adminCookie, adminCSRF), 404)
	expectStatus(t, env.do("POST", path, `{"user_id":"`+itoa(member)+`","kind":"mute","reason":"原因","ends_at":"上月"}`,
		adminCookie, adminCSRF), 422)
	expectStatus(t, env.do("POST", path, `{"user_id":"`+itoa(member)+`","kind":"mute","reason":"原因","ends_at":"2020-01-01T00:00:00Z"}`,
		adminCookie, adminCSRF), 422)
	expectStatus(t, env.do("DELETE", "/api/v1/admin/restrictions/99999999", "", adminCookie, adminCSRF), 404)
	expectStatus(t, env.getAs("/api/v1/admin/restrictions?limit=101", adminCookie), 422)
	expectStatus(t, env.getAs("/api/v1/admin/restrictions?cursor="+encodeCursor(adminUserCursorScope, 1), adminCookie), 400)

	// Nothing above created a restriction or an audit row.
	var restrictions, actions int
	if err := env.db.QueryRow(`SELECT COUNT(*) FROM user_restrictions`).Scan(&restrictions); err != nil {
		t.Fatal(err)
	}
	if err := env.db.QueryRow(`SELECT COUNT(*) FROM moderation_actions`).Scan(&actions); err != nil {
		t.Fatal(err)
	}
	if restrictions != 0 || actions != 0 {
		t.Fatalf("a rejected request left %d restrictions and %d audit rows", restrictions, actions)
	}
}

func TestAdminUserListExposesNoAddresses(t *testing.T) {
	env := newTopicEnv(t)
	adminCookie, _ := env.sessionCookie(env.newAdmin("moderator"))
	member := env.newUser("member")

	page := env.getAs("/api/v1/admin/users", adminCookie)
	expectStatus(t, page, 200)
	var decoded struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(page.body, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Items) == 0 {
		t.Fatal("the user list is empty")
	}
	found := false
	for _, item := range decoded.Items {
		if item["internal_user_id"] == itoa(member) {
			found = true
			profile, _ := item["profile"].(map[string]any)
			if item["account_status"] != "active" || item["role"] != "user" {
				t.Fatalf("the member's state is wrong: %v", item)
			}
			if profile == nil || profile["nickname"] == nil {
				t.Fatalf("the member has no public profile in the list: %v", item)
			}
		}
	}
	if !found {
		t.Fatalf("the member is not in the list: %s", string(page.body))
	}
	// The list is a directory, not a mailing list: no address may appear, in any form.
	for _, forbidden := range []string{"@example.test", "email", "password"} {
		if strings.Contains(string(page.body), forbidden) {
			t.Fatalf("the user list exposes %q: %s", forbidden, string(page.body))
		}
	}

	// A ban in force is visible in the list, so an administrator can see the state they set. The check reads
	// the list fetched *after* the ban — reading the earlier one would only assert that nothing changed.
	admin2 := env.newAdmin("moderator2")
	admin2Cookie, admin2CSRF := env.sessionCookie(admin2)
	expectStatus(t, env.do("POST", "/api/v1/admin/restrictions",
		`{"user_id":"`+itoa(member)+`","kind":"ban","reason":"原因"}`, admin2Cookie, admin2CSRF), 201)
	banned := env.getAs("/api/v1/admin/users", admin2Cookie)
	if !strings.Contains(string(banned.body), `"account_status":"banned"`) {
		t.Fatalf("a ban in force is not visible in the user list: %s", string(banned.body))
	}
}
