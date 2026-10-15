package content

import (
	"encoding/json"
	"strings"
	"testing"
)

// B5 covers the two operations that reveal something normally hidden, plus an administrator's ability to
// delete somebody else's content. The tests are the same shape for all three: what is disclosed, and what
// record of the disclosure is left behind.

func TestAdminDeletesAnotherUsersContent(t *testing.T) {
	env := newTopicEnv(t)
	author := env.newUser("author")
	topic := env.createTopicBy(author, false, "管理员要删的帖子")
	admin := env.newAdmin("moderator")
	adminCookie, adminCSRF := env.sessionCookie(admin)

	// The permission endpoint tells the administrator they may do it, and still refuses edit: an administrator
	// removes content through moderation, they do not rewrite somebody's words.
	permissions := env.getAs("/api/v1/contents/"+itoa(topic)+"/permissions", adminCookie)
	expectStatus(t, permissions, 200)
	var flags map[string]any
	if err := json.Unmarshal(permissions.body, &flags); err != nil {
		t.Fatal(err)
	}
	if flags["can_delete"] != true {
		t.Fatalf("an administrator cannot delete: %v", flags)
	}
	if flags["can_edit"] == true {
		t.Fatalf("an administrator may edit somebody else's content: %v", flags)
	}

	// The deletion goes through the normal path, so the archive keeps who did it.
	deleted := env.do("DELETE", "/api/v1/contents/"+itoa(topic), "", adminCookie, adminCSRF)
	expectStatus(t, deleted, 200)
	var deletedBy uint64
	var purgeAfter string
	if err := env.db.QueryRow(`SELECT deleted_by,purge_after FROM deleted_content_archives WHERE content_id=?`, topic).
		Scan(&deletedBy, &purgeAfter); err != nil {
		t.Fatal(err)
	}
	if deletedBy != admin {
		t.Fatalf("the archive credits %d, want the administrator %d", deletedBy, admin)
	}
	// And an audit row states what happened, for the same reason every other moderation action does.
	var action, reason string
	if err := env.db.QueryRow(`SELECT action,reason FROM moderation_actions WHERE target_content_id=? ORDER BY id DESC LIMIT 1`, topic).
		Scan(&action, &reason); err != nil {
		t.Fatal(err)
	}
	if action != actionAdminDelete || !strings.Contains(reason, itoa(topic)) {
		t.Fatalf("audit row is action=%q reason=%q", action, reason)
	}

	// An ordinary reader still cannot delete somebody else's content: the new path is admin-only.
	other := env.newUser("bystander")
	otherCookie, otherCSRF := env.sessionCookie(other)
	victim := env.createTopicBy(author, false, "别人的帖子")
	expectStatus(t, env.do("DELETE", "/api/v1/contents/"+itoa(victim), "", otherCookie, otherCSRF), 403)
	expectStatus(t, env.do("DELETE", "/api/v1/contents/"+itoa(victim), "", "", ""), 401)
}

func TestDeletedArchiveReadIsAuditedAndExpires(t *testing.T) {
	env := newTopicEnv(t)
	author := env.newUser("author")
	topic := env.createTopicBy(author, false, "会被删除的帖子")
	if _, err := env.db.Exec(`UPDATE contents SET body_md=? WHERE id=?`, "原文正文", topic); err != nil {
		t.Fatal(err)
	}
	authorCookie, authorCSRF := env.sessionCookie(author)
	expectStatus(t, env.do("DELETE", "/api/v1/contents/"+itoa(topic), "", authorCookie, authorCSRF), 200)

	adminCookie, adminCSRF := env.sessionCookie(env.newAdmin("moderator"))
	read := env.do("POST", "/api/v1/admin/deleted-archives/read",
		`{"content_id":"`+itoa(topic)+`","reason":"举报核查"}`, adminCookie, adminCSRF)
	expectStatus(t, read, 200)
	var archive map[string]any
	if err := json.Unmarshal(read.body, &archive); err != nil {
		t.Fatal(err)
	}
	if archive["original_body_md"] != "原文正文" {
		t.Fatalf("the archive did not return the original text: %v", archive)
	}
	if archive["original_title"] != "会被删除的帖子" {
		t.Fatalf("the archive lost the original title: %v", archive)
	}
	// Reading is itself an event, with the stated reason.
	var action, reason string
	if err := env.db.QueryRow(`SELECT action,reason FROM moderation_actions WHERE action=? ORDER BY id DESC LIMIT 1`, actionReadArchive).
		Scan(&action, &reason); err != nil {
		t.Fatal(err)
	}
	if reason != "举报核查" {
		t.Fatalf("the audit reason is %q", reason)
	}

	// A reason is mandatory: the accountability of this endpoint is the reason itself.
	expectStatus(t, env.do("POST", "/api/v1/admin/deleted-archives/read",
		`{"content_id":"`+itoa(topic)+`"}`, adminCookie, adminCSRF), 422)
	expectStatus(t, env.do("POST", "/api/v1/admin/deleted-archives/read",
		`{"content_id":"`+itoa(topic)+`","reason":"   "}`, adminCookie, adminCSRF), 422)
	memberCookie, memberCSRF := env.sessionCookie(env.newUser("member"))
	expectStatus(t, env.do("POST", "/api/v1/admin/deleted-archives/read",
		`{"content_id":"`+itoa(topic)+`","reason":"越权"}`, memberCookie, memberCSRF), 403)
	expectStatus(t, env.do("POST", "/api/v1/admin/deleted-archives/read",
		`{"content_id":"`+itoa(topic)+`","reason":"缺令牌"}`, adminCookie, ""), 403)
	// Content that was never deleted has no archive.
	expectStatus(t, env.do("POST", "/api/v1/admin/deleted-archives/read",
		`{"content_id":"`+itoa(env.topic(env.boardID, "从未删除"))+`","reason":"没有归档"}`, adminCookie, adminCSRF), 404)

	// Past the retention window the record must not be returned even though the row is still on disk: the
	// check is on the clock, not on whether the cleanup job has run.
	if _, err := env.db.Exec(`UPDATE deleted_content_archives
 SET deleted_at=DATE_SUB(UTC_TIMESTAMP(6), INTERVAL 31 DAY), purge_after=DATE_SUB(UTC_TIMESTAMP(6), INTERVAL 1 DAY)
 WHERE content_id=?`, topic); err != nil {
		t.Fatal(err)
	}
	expired := env.do("POST", "/api/v1/admin/deleted-archives/read",
		`{"content_id":"`+itoa(topic)+`","reason":"过期后仍想读"}`, adminCookie, adminCSRF)
	expectStatus(t, expired, 410)
	expectCode(t, expired, "ARCHIVE_EXPIRED")
	if strings.Contains(string(expired.body), "原文正文") {
		t.Fatal("an expired archive leaked its text in the error response")
	}
}

func TestAnonymousTraceIsAuditedAndIdentifiesOnlyOnDemand(t *testing.T) {
	env := newTopicEnv(t)
	author := env.newUser("secretive")
	topic := env.createTopicBy(author, true, "匿名帖")
	adminCookie, adminCSRF := env.sessionCookie(env.newAdmin("moderator"))

	// Public reads show the anonymous author and nothing else — the point of the feature.
	public := string(env.get("/api/v1/contents/" + itoa(topic)).body)
	for _, secret := range []string{env.nicknameOf(author), env.publicIDOf(author)} {
		if strings.Contains(public, secret) {
			t.Fatalf("the public payload leaked %q: %s", secret, public)
		}
	}

	// The trace returns the account and the audit id of the record it just wrote.
	trace := env.do("POST", "/api/v1/admin/anonymous-traces",
		`{"content_id":"`+itoa(topic)+`","reason":"涉嫌违法内容，需要追溯"}`, adminCookie, adminCSRF)
	expectStatus(t, trace, 200)
	var result map[string]any
	if err := json.Unmarshal(trace.body, &result); err != nil {
		t.Fatal(err)
	}
	profile, _ := result["author"].(map[string]any)["profile"].(map[string]any)
	if profile == nil || profile["nickname"] != env.nicknameOf(author) {
		t.Fatalf("the trace did not identify the author: %v", result)
	}
	auditID, _ := result["audit_id"].(string)
	if auditID == "" {
		t.Fatalf("the trace carries no audit id: %v", result)
	}
	var action, reason string
	var target uint64
	if err := env.db.QueryRow(`SELECT action,reason,COALESCE(target_content_id,0) FROM moderation_actions WHERE id=?`, auditID).
		Scan(&action, &reason, &target); err != nil {
		t.Fatal(err)
	}
	if action != actionAnonymousTrace || reason != "涉嫌违法内容，需要追溯" || target != topic {
		t.Fatalf("audit row is action=%q reason=%q target=%d", action, reason, target)
	}

	// The reason is mandatory, and only administrators may trace.
	expectStatus(t, env.do("POST", "/api/v1/admin/anonymous-traces",
		`{"content_id":"`+itoa(topic)+`","reason":""}`, adminCookie, adminCSRF), 422)
	memberCookie, memberCSRF := env.sessionCookie(env.newUser("member"))
	expectStatus(t, env.do("POST", "/api/v1/admin/anonymous-traces",
		`{"content_id":"`+itoa(topic)+`","reason":"越权"}`, memberCookie, memberCSRF), 403)
	expectStatus(t, env.do("POST", "/api/v1/admin/anonymous-traces",
		`{"content_id":"`+itoa(topic)+`","reason":"匿名"}`, "", ""), 401)
	expectStatus(t, env.do("POST", "/api/v1/admin/anonymous-traces",
		`{"content_id":"99999999","reason":"不存在"}`, adminCookie, adminCSRF), 404)

	// No audit row was written by any of the refused attempts: only the successful trace is on record.
	var traces int
	if err := env.db.QueryRow(`SELECT COUNT(*) FROM moderation_actions WHERE action=?`, actionAnonymousTrace).Scan(&traces); err != nil {
		t.Fatal(err)
	}
	if traces != 1 {
		t.Fatalf("refused attempts left %d trace records", traces)
	}
}
