package content

import (
	"encoding/json"
	"strings"
	"testing"
)

// Boards, tags and topic flags are all "the administrator changes the shape of the site" operations. The
// tests are about the refusals that keep that shape meaningful, and about the audit row each change writes.

func TestBoardCreateAndUpdate(t *testing.T) {
	env := newTopicEnv(t)
	adminCookie, adminCSRF := env.sessionCookie(env.newAdmin("moderator"))

	created := env.do("POST", "/api/v1/admin/boards",
		`{"name":"公告","description":"站点公告","kind":"discussion","sort_order":1,"is_archived":false}`,
		adminCookie, adminCSRF)
	expectStatus(t, created, 201)
	var board map[string]any
	if err := json.Unmarshal(created.body, &board); err != nil {
		t.Fatal(err)
	}
	boardID := board["id"].(string)
	if board["name"] != "公告" || board["kind"] != "discussion" || board["sort_order"] != float64(1) {
		t.Fatalf("the created board is %v", board)
	}
	// It is in the public list immediately: this is site structure, not a draft.
	if !strings.Contains(string(env.get("/api/v1/boards").body), boardID) {
		t.Fatalf("the new board is not in the public list")
	}

	// Rename through PUT.
	renamed := env.do("PUT", "/api/v1/admin/boards/"+boardID,
		`{"name":"站点公告","description":"只读公告","kind":"discussion","sort_order":2,"is_archived":true}`,
		adminCookie, adminCSRF)
	expectStatus(t, renamed, 200)
	var updated map[string]any
	if err := json.Unmarshal(renamed.body, &updated); err != nil {
		t.Fatal(err)
	}
	if updated["name"] != "站点公告" || updated["is_archived"] != true {
		t.Fatalf("the board was not updated: %v", updated)
	}

	// A board with topics cannot change its kind: that would reinterpret everything already inside it.
	topic := env.createTopicBy(env.newUser("author"), false, "公告里的帖子")
	if _, err := env.db.Exec(`UPDATE topics SET board_id=? WHERE content_id=?`, boardID, topic); err != nil {
		t.Fatal(err)
	}
	locked := env.do("PUT", "/api/v1/admin/boards/"+boardID,
		`{"name":"站点公告","description":"只读公告","kind":"qa","sort_order":2,"is_archived":true}`,
		adminCookie, adminCSRF)
	expectStatus(t, locked, 409)
	expectCode(t, locked, "BOARD_KIND_LOCKED")
	// An empty board may still change its kind.
	empty := env.do("POST", "/api/v1/admin/boards",
		`{"name":"临时","description":"","kind":"discussion","sort_order":9,"is_archived":false}`, adminCookie, adminCSRF)
	expectStatus(t, empty, 201)
	var emptyBoard map[string]any
	if err := json.Unmarshal(empty.body, &emptyBoard); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, env.do("PUT", "/api/v1/admin/boards/"+emptyBoard["id"].(string),
		`{"name":"临时","description":"","kind":"qa","sort_order":9,"is_archived":false}`, adminCookie, adminCSRF), 200)

	// Boundaries and authorization.
	expectStatus(t, env.do("POST", "/api/v1/admin/boards",
		`{"name":"","description":"","kind":"discussion"}`, adminCookie, adminCSRF), 422)
	expectStatus(t, env.do("POST", "/api/v1/admin/boards",
		`{"name":"名字","description":"","kind":"unknown"}`, adminCookie, adminCSRF), 422)
	expectStatus(t, env.do("PUT", "/api/v1/admin/boards/99999999",
		`{"name":"名字","description":"","kind":"discussion"}`, adminCookie, adminCSRF), 404)
	memberCookie, memberCSRF := env.sessionCookie(env.newUser("member"))
	expectStatus(t, env.do("POST", "/api/v1/admin/boards",
		`{"name":"名字","description":"","kind":"discussion"}`, memberCookie, memberCSRF), 403)
	expectStatus(t, env.do("POST", "/api/v1/admin/boards",
		`{"name":"名字","description":"","kind":"discussion"}`, adminCookie, ""), 403)
	expectStatus(t, env.do("POST", "/api/v1/admin/boards",
		`{"name":"名字","description":"","kind":"discussion"}`, "", ""), 401)
}

func TestTagNormalisationAndDuplicates(t *testing.T) {
	env := newTopicEnv(t)
	adminCookie, adminCSRF := env.sessionCookie(env.newAdmin("moderator"))

	created := env.do("POST", "/api/v1/admin/tags", `{"name":"MySQL","description":"数据库"}`, adminCookie, adminCSRF)
	expectStatus(t, created, 201)
	var tag map[string]any
	if err := json.Unmarshal(created.body, &tag); err != nil {
		t.Fatal(err)
	}
	tagID := tag["id"].(string)

	// The uniqueness key is the normalised name, so a differently-cased duplicate is the same tag.
	duplicate := env.do("POST", "/api/v1/admin/tags", `{"name":"mysql","description":"另一个"}`, adminCookie, adminCSRF)
	expectStatus(t, duplicate, 409)
	expectCode(t, duplicate, "TAG_NAME_TAKEN")

	// Renaming to a name another tag already holds is refused the same way.
	other := env.do("POST", "/api/v1/admin/tags", `{"name":"Go","description":"语言"}`, adminCookie, adminCSRF)
	expectStatus(t, other, 201)
	var otherTag map[string]any
	if err := json.Unmarshal(other.body, &otherTag); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, env.do("PUT", "/api/v1/admin/tags/"+otherTag["id"].(string),
		`{"name":"MYSQL","description":"语言"}`, adminCookie, adminCSRF), 409)
	// Renaming to itself is fine, and so is a genuine rename.
	expectStatus(t, env.do("PUT", "/api/v1/admin/tags/"+otherTag["id"].(string),
		`{"name":"Go","description":"编程语言"}`, adminCookie, adminCSRF), 200)
	expectStatus(t, env.do("PUT", "/api/v1/admin/tags/"+tagID,
		`{"name":"MySQL 8","description":"数据库"}`, adminCookie, adminCSRF), 200)
	// The renamed tag is visible in the public list under its new name.
	if !strings.Contains(string(env.get("/api/v1/tags").body), "MySQL 8") {
		t.Fatal("the renamed tag is not in the public list")
	}

	expectStatus(t, env.do("PUT", "/api/v1/admin/tags/99999999", `{"name":"不存在","description":""}`, adminCookie, adminCSRF), 404)
	expectStatus(t, env.do("POST", "/api/v1/admin/tags", `{"name":"   ","description":""}`, adminCookie, adminCSRF), 422)
	expectStatus(t, env.do("POST", "/api/v1/admin/tags", `{"name":"标签"}`, adminCookie, adminCSRF), 422)
}

func TestTopicModerationRecordsTheReason(t *testing.T) {
	env := newTopicEnv(t)
	author := env.newUser("author")
	topic := env.createTopicBy(author, false, "被处置的帖子")
	adminCookie, adminCSRF := env.sessionCookie(env.newAdmin("moderator"))

	// Lock and pin in one decision, with the operator's own reason.
	locked := env.do("PATCH", "/api/v1/admin/topics/"+itoa(topic),
		`{"reason":"争议内容，先锁定","is_locked":true,"is_pinned":true}`, adminCookie, adminCSRF)
	expectStatus(t, locked, 200)
	var payload map[string]any
	if err := json.Unmarshal(locked.body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["is_locked"] != true || payload["is_pinned"] != true {
		t.Fatalf("the response does not show the new state: %v", payload)
	}
	// The public payload agrees, so the flag is not only in the response of the write.
	public := string(env.get("/api/v1/contents/" + itoa(topic)).body)
	if !strings.Contains(public, `"is_locked":true`) {
		t.Fatalf("locking did not reach the public payload: %s", public)
	}
	// The author can no longer edit a locked topic, which is what locking is for.
	authorCookie, authorCSRF := env.sessionCookie(author)
	expectStatus(t, env.do("PATCH", "/api/v1/contents/"+itoa(topic), `{"body_md":"改一改"}`, authorCookie, authorCSRF), 409)
	// The reason is in the audit trail, attached to the topic.
	var reason, action string
	if err := env.db.QueryRow(`SELECT action,reason FROM moderation_actions WHERE target_content_id=? ORDER BY id DESC LIMIT 1`, topic).
		Scan(&action, &reason); err != nil {
		t.Fatal(err)
	}
	if action != actionModerateTopic || reason != "争议内容，先锁定" {
		t.Fatalf("audit row is action=%q reason=%q", action, reason)
	}

	// Moving between boards of the same kind is allowed; crossing kinds is not, because the kind decides what
	// a topic means.
	sameKind := env.createTopicBy(env.newUser("author"), false, "另一个帖子")
	if _, err := env.db.Exec(`UPDATE contents SET kind='topic' WHERE id=?`, sameKind); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, env.do("PATCH", "/api/v1/admin/topics/"+itoa(topic),
		`{"reason":"移动同类板块","board_id":"`+itoa(env.boardID)+`"}`, adminCookie, adminCSRF), 200)
	crossed := env.do("PATCH", "/api/v1/admin/topics/"+itoa(topic),
		`{"reason":"移动到问答板","board_id":"`+itoa(env.qaBoardID)+`"}`, adminCookie, adminCSRF)
	expectStatus(t, crossed, 409)
	expectCode(t, crossed, "BOARD_KIND_MISMATCH")
	expectStatus(t, env.do("PATCH", "/api/v1/admin/topics/"+itoa(topic),
		`{"reason":"移动到不存在的板块","board_id":"99999999"}`, adminCookie, adminCSRF), 404)

	// Boundaries: a reason is required, and a request that changes nothing is not a decision.
	expectStatus(t, env.do("PATCH", "/api/v1/admin/topics/"+itoa(topic), `{"is_locked":false}`, adminCookie, adminCSRF), 422)
	expectStatus(t, env.do("PATCH", "/api/v1/admin/topics/"+itoa(topic), `{"reason":"   ","is_locked":false}`, adminCookie, adminCSRF), 422)
	expectStatus(t, env.do("PATCH", "/api/v1/admin/topics/"+itoa(topic), `{"reason":"没改什么"}`, adminCookie, adminCSRF), 422)
	expectStatus(t, env.do("PATCH", "/api/v1/admin/topics/99999999", `{"reason":"不存在","is_locked":true}`, adminCookie, adminCSRF), 404)
	memberCookie, memberCSRF := env.sessionCookie(env.newUser("member"))
	expectStatus(t, env.do("PATCH", "/api/v1/admin/topics/"+itoa(topic), `{"reason":"越权","is_locked":true}`, memberCookie, memberCSRF), 403)
	_ = sameKind
}
