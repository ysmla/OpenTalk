package content

import (
	"encoding/json"
	"testing"
)

// Moderation is the one path in this package that can remove content from public view, so the tests are
// about what hiding reaches (descendants and attachments, through the shared ancestor rule), what it records
// (one audit row per decision, never per click), and who is allowed to do it (only an administrator).

// newAdmin creates an account and promotes it. The other moderation tests promote inline with the same
// UPDATE (delete_test.go, read_test.go); this only names the step.
func (e *topicEnv) newAdmin(nickname string) uint64 {
	e.t.Helper()
	id := e.newUser(nickname)
	if _, err := e.db.Exec(`UPDATE users SET role='admin' WHERE id=?`, id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func TestHidingContentReachesDescendants(t *testing.T) {
	env := newTopicEnv(t)
	question := env.newQuestion(false)
	answerer := env.newUser("answerer")
	answer, err := env.answer(question, answerer, false)
	if err != nil {
		t.Fatal(err)
	}
	commenter := env.newUser("commenter")
	comment, err := env.comment(NewComment{AuthorID: commenter, TopicID: question, AnswerID: answer, BodyMD: "回复正文"})
	if err != nil {
		t.Fatal(err)
	}
	adminCookie, adminCSRF := env.sessionCookie(env.newAdmin("moderator"))

	// Everything is readable before the decision.
	expectStatus(t, env.get("/api/v1/contents/"+itoa(question)), 200)
	expectStatus(t, env.get("/api/v1/contents/"+itoa(answer)), 200)
	expectStatus(t, env.get("/api/v1/contents/"+itoa(comment)), 200)

	hide := env.do("PATCH", "/api/v1/admin/contents/"+itoa(question)+"/visibility",
		`{"visibility":"hidden","reason":"包含违规内容"}`, adminCookie, adminCSRF)
	expectStatus(t, hide, 204)

	// The decision reaches the whole subtree through the shared rule, without this endpoint having to walk it:
	// the descendants' own rows are still visible and undeleted.
	for name, id := range map[string]uint64{"question": question, "answer": answer, "comment": comment} {
		expectStatus(t, env.get("/api/v1/contents/"+itoa(id)), 404)
		// And it is gone from the public list and from search too, not only from its own page.
		for _, found := range searchIDs(t, env.search("正文")) {
			if found == itoa(id) {
				t.Fatalf("hidden %s still appears in search", name)
			}
		}
	}
	// Hiding is not deleting: the row stays, with its body and title intact, and nothing is archived (there
	// is no deleted_content_archives row) — which is what makes restoring possible.
	var stillThere, archived int
	if err := env.db.QueryRow(`SELECT COUNT(*) FROM contents WHERE id=? AND deleted_at IS NULL AND moderation_status='hidden'`, question).Scan(&stillThere); err != nil {
		t.Fatal(err)
	}
	if stillThere != 1 {
		t.Fatal("hiding did not leave the row in place with its status set to hidden")
	}
	if err := env.db.QueryRow(`SELECT COUNT(*) FROM deleted_content_archives WHERE content_id=?`, question).Scan(&archived); err != nil {
		t.Fatal(err)
	}
	if archived != 0 {
		t.Fatal("hiding archived the content, which is what deletion does and would make restoring impossible")
	}
	// A hidden answer can no longer be edited or deleted by its author, and no reply can be added under it:
	// the same rule closes the write paths.
	answererCookie, answererCSRF := env.sessionCookie(answerer)
	expectStatus(t, env.do("PATCH", "/api/v1/contents/"+itoa(answer),
		`{"body_md":"改一改"}`, answererCookie, answererCSRF), 404)
	expectStatus(t, env.do("POST", "/api/v1/comments",
		`{"topic_id":"`+itoa(question)+`","answer_id":"`+itoa(answer)+`","body_md":"再回一条"}`, answererCookie, answererCSRF), 404)

	// Restoring brings the subtree back, because restoration only flips the one column the rule reads.
	expectStatus(t, env.do("PATCH", "/api/v1/admin/contents/"+itoa(question)+"/visibility",
		`{"visibility":"visible","reason":"复核后恢复"}`, adminCookie, adminCSRF), 204)
	expectStatus(t, env.get("/api/v1/contents/"+itoa(question)), 200)
	expectStatus(t, env.get("/api/v1/contents/"+itoa(answer)), 200)
	expectStatus(t, env.get("/api/v1/contents/"+itoa(comment)), 200)
}

func TestModerationRecordsOneAuditRowPerDecision(t *testing.T) {
	env := newTopicEnv(t)
	topic := env.topic(env.boardID, "审核目标")
	adminCookie, adminCSRF := env.sessionCookie(env.newAdmin("moderator"))
	hide := func(reason string) response {
		return env.do("PATCH", "/api/v1/admin/contents/"+itoa(topic)+"/visibility",
			`{"visibility":"hidden","reason":"`+reason+`"}`, adminCookie, adminCSRF)
	}

	expectStatus(t, hide("第一次"), 204)
	// Pressing hide again is a no-op: it changes nothing and — more importantly — records nothing, so an
	// audit row always means "a decision was taken here".
	expectStatus(t, hide("第二次"), 204)
	var rootRows int
	if err := env.db.QueryRow(`SELECT COUNT(*) FROM moderation_actions WHERE target_content_id=? AND action='hide_content'`, topic).Scan(&rootRows); err != nil {
		t.Fatal(err)
	}
	if rootRows != 1 {
		t.Fatalf("a repeated decision wrote %d audit rows", rootRows)
	}

	// The audit page is admin-only and carries ids and the stated reason, never the content's text.
	expectStatus(t, env.get("/api/v1/admin/audit"), 401)
	memberCookie, _ := env.sessionCookie(env.newUser("member"))
	expectStatus(t, env.getAs("/api/v1/admin/audit", memberCookie), 403)
	page := env.getAs("/api/v1/admin/audit", adminCookie)
	expectStatus(t, page, 200)
	var decoded struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(page.body, &decoded); err != nil {
		t.Fatalf("cannot decode the audit page: %v", err)
	}
	items := decoded.Items
	found := false
	for _, entry := range items {
		if entry["target_content_id"] == itoa(topic) {
			found = true
			if entry["action"] != "hide_content" || entry["reason"] != "第一次" {
				t.Fatalf("the audit entry does not describe the decision: %v", entry)
			}
			if entry["actor_user_id"] == "" {
				t.Fatalf("the audit entry has no actor: %v", entry)
			}
		}
	}
	if !found {
		t.Fatalf("no audit entry for the hidden topic: %v", items)
	}
	// Restoring is a second, separate decision.
	expectStatus(t, env.do("PATCH", "/api/v1/admin/contents/"+itoa(topic)+"/visibility",
		`{"visibility":"visible","reason":"恢复"}`, adminCookie, adminCSRF), 204)
	if err := env.db.QueryRow(`SELECT COUNT(*) FROM moderation_actions WHERE target_content_id=?`, topic).Scan(&rootRows); err != nil {
		t.Fatal(err)
	}
	if rootRows != 2 {
		t.Fatalf("hide then restore produced %d audit rows, want 2", rootRows)
	}

	// A cursor from another list is refused rather than mispaging this one.
	expectStatus(t, env.getAs("/api/v1/admin/audit?cursor="+encodeCursor(boardCursorScope, 1, 1), adminCookie), 400)
}

func TestModerationAuthorizationAndBoundaries(t *testing.T) {
	env := newTopicEnv(t)
	topic := env.topic(env.boardID, "审核目标")
	adminCookie, adminCSRF := env.sessionCookie(env.newAdmin("moderator"))
	memberCookie, memberCSRF := env.sessionCookie(env.newUser("member"))
	path := "/api/v1/admin/contents/" + itoa(topic) + "/visibility"

	// Only an administrator, and never anonymously.
	expectStatus(t, env.do("PATCH", path, `{"visibility":"hidden","reason":"越权"}`, "", ""), 401)
	expectStatus(t, env.do("PATCH", path, `{"visibility":"hidden","reason":"越权"}`, memberCookie, memberCSRF), 403)
	expectCode(t, env.do("PATCH", path, `{"visibility":"hidden","reason":"越权"}`, memberCookie, memberCSRF), "FORBIDDEN")
	// An admin without the CSRF token is still refused: the role is not a substitute for the token.
	expectStatus(t, env.do("PATCH", path, `{"visibility":"hidden","reason":"缺令牌"}`, adminCookie, ""), 403)

	// Input boundaries.
	expectStatus(t, env.do("PATCH", path, `{"visibility":"hidden"}`, adminCookie, adminCSRF), 422)
	expectStatus(t, env.do("PATCH", path, `{"visibility":"hidden","reason":"   "}`, adminCookie, adminCSRF), 422)
	expectStatus(t, env.do("PATCH", path, `{"visibility":"invisible","reason":"原因"}`, adminCookie, adminCSRF), 422)
	expectStatus(t, env.do("PATCH", path, `{"visibility":"hidden","reason":"原因","extra":1}`, adminCookie, adminCSRF), 400)
	expectStatus(t, env.do("PATCH", "/api/v1/admin/contents/abc/visibility",
		`{"visibility":"hidden","reason":"原因"}`, adminCookie, adminCSRF), 422)
	expectStatus(t, env.do("PATCH", "/api/v1/admin/contents/99999999/visibility",
		`{"visibility":"hidden","reason":"原因"}`, adminCookie, adminCSRF), 404)

	// Deleted content is not hidable: it is archived, and "deleted" must not blur into "hidden".
	deleted := env.topic(env.boardID, "已删除")
	env.deleteContent(deleted)
	expectStatus(t, env.do("PATCH", "/api/v1/admin/contents/"+itoa(deleted)+"/visibility",
		`{"visibility":"hidden","reason":"原因"}`, adminCookie, adminCSRF), 404)

	// Nothing above changed the content.
	var status string
	if err := env.db.QueryRow(`SELECT moderation_status FROM contents WHERE id=?`, topic).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "visible" {
		t.Fatalf("a rejected request changed the content to %q", status)
	}
	var rows int
	if err := env.db.QueryRow(`SELECT COUNT(*) FROM moderation_actions`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("a rejected request wrote %d audit rows", rows)
	}
}
