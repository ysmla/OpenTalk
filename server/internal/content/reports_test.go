package content

import (
	"encoding/json"
	"testing"
)

// Reporting is small, so the tests concentrate on the three things it must not do: reveal the reporter,
// act as a verdict on the content, or become a way to ask whether a hidden id exists.

func decodeReport(t *testing.T, res response) map[string]any {
	t.Helper()
	var view map[string]any
	if err := json.Unmarshal(res.body, &view); err != nil {
		t.Fatalf("cannot decode %s: %v", string(res.body), err)
	}
	return view
}

func TestCreateReportAndReadOwnReports(t *testing.T) {
	env := newTopicEnv(t)
	topic := env.topic(env.boardID, "举报目标")
	reporter := env.newUser("reporter")
	cookie, csrf := env.sessionCookie(reporter)

	body := `{"content_id":"` + itoa(topic) + `","reason":"包含广告","details":"反复推广同一个链接"}`
	res := env.do("POST", "/api/v1/reports", body, cookie, csrf)
	expectStatus(t, res, 201)
	created := decodeReport(t, res)
	if created["reason"] != "包含广告" || created["details"] != "反复推广同一个链接" {
		t.Fatalf("the response does not echo the report: %v", created)
	}
	if created["status"] != reportPending {
		t.Fatalf("a new report is %v", created["status"])
	}
	if _, leaked := created["reporter_id"]; leaked {
		t.Fatal("the response exposes the reporter to a payload the author could read")
	}
	if _, leaked := created["resolution_note"]; leaked {
		// Omitted while there is nothing to say: an empty note is not the same as "handled".
		t.Fatalf("an unresolved report carries a resolution note: %v", created)
	}
	// The report must not have touched the content: a report is a claim, not a verdict.
	var status string
	if err := env.db.QueryRow(`SELECT moderation_status FROM contents WHERE id=?`, topic).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "visible" {
		t.Fatalf("reporting changed the content to %q", status)
	}

	// The reporter can read it back, newest first.
	page := decodeReadPage(t, env.getAs("/api/v1/me/reports", cookie))
	if len(page.Items) != 1 {
		t.Fatalf("own reports page has %d items", len(page.Items))
	}
	if idsOf(t, page)[0] != created["id"] {
		t.Fatalf("the list disagrees with the create response: %v", page.Items)
	}

	// Only the reporter sees it: another account's list is empty, and there is no path that shows
	// somebody else's report.
	otherCookie, _ := env.sessionCookie(env.newUser("bystander"))
	if other := decodeReadPage(t, env.getAs("/api/v1/me/reports", otherCookie)); len(other.Items) != 0 {
		t.Fatalf("another account can see the report: %v", other.Items)
	}
	// Session and CSRF are required.
	expectStatus(t, env.get("/api/v1/me/reports"), 401)
	expectStatus(t, env.do("POST", "/api/v1/reports", body, cookie, ""), 403)
}

func TestReportRejectsSecondOpenReportAndBadTargets(t *testing.T) {
	env := newTopicEnv(t)
	topic := env.topic(env.boardID, "举报目标")
	reporter := env.newUser("reporter")
	cookie, csrf := env.sessionCookie(reporter)
	report := func(contentID uint64, reason string) response {
		return env.do("POST", "/api/v1/reports",
			`{"content_id":"`+itoa(contentID)+`","reason":"`+reason+`"}`, cookie, csrf)
	}

	expectStatus(t, report(topic, "第一次"), 201)
	// The table's unique key allows one open report per (reporter, content). A second one is a conflict the
	// client can understand, not a duplicate-key error.
	duplicate := report(topic, "第二次")
	expectStatus(t, duplicate, 409)
	expectCode(t, duplicate, "REPORT_ALREADY_ACTIVE")

	// Once a report is closed, the reader may report the same content again: `active_slot` is NULL for a
	// closed row, so it does not occupy the slot.
	if _, err := env.db.Exec(`UPDATE reports SET status='dismissed', handler_id=?, resolution_note='不成立',
 resolved_at=UTC_TIMESTAMP(6) WHERE reporter_id=? AND content_id=?`, env.authorID, reporter, topic); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, report(topic, "再次举报"), 201)
	// And the closed report now carries its outcome, which is how the reporter learns the result.
	page := decodeReadPage(t, env.getAs("/api/v1/me/reports", cookie))
	closed := 0
	for _, item := range page.Items {
		var record map[string]any
		if err := json.Unmarshal(item, &record); err != nil {
			t.Fatalf("cannot decode a report: %v", err)
		}
		if record["status"] == reportDismissed {
			closed++
			if record["resolution_note"] != "不成立" {
				t.Fatalf("the closed report lost its note: %v", record)
			}
			if record["resolved_at"] == nil {
				t.Fatalf("the closed report has no resolution time: %v", record)
			}
		}
	}
	if closed != 1 {
		t.Fatalf("expected one closed report, got %d", closed)
	}

	// A hidden target is reported as missing, so a report cannot be used to test whether a hidden id exists.
	hidden := env.topic(env.boardID, "隐藏的目标")
	env.hideContent(t, itoa(hidden))
	expectStatus(t, report(hidden, "隐藏内容"), 404)
	expectCode(t, report(hidden, "隐藏内容"), "CONTENT_NOT_FOUND")
	// A deleted target and an id that was never issued look the same.
	deleted := env.topic(env.boardID, "已删除的目标")
	env.deleteContent(deleted)
	expectStatus(t, report(deleted, "已删除"), 404)
	expectStatus(t, report(99999999, "不存在"), 404)

	// Input boundaries.
	expectStatus(t, env.do("POST", "/api/v1/reports", `{"content_id":"`+itoa(topic)+`"}`, cookie, csrf), 422)
	expectStatus(t, env.do("POST", "/api/v1/reports", `{"content_id":"`+itoa(topic)+`","reason":"   "}`, cookie, csrf), 422)
	expectStatus(t, env.do("POST", "/api/v1/reports", `{"content_id":"abc","reason":"原因"}`, cookie, csrf), 422)
	long := ""
	for index := 0; index < reportMaxReason+1; index++ {
		long += "字"
	}
	expectStatus(t, env.do("POST", "/api/v1/reports",
		`{"content_id":"`+itoa(topic)+`","reason":"`+long+`"}`, cookie, csrf), 422)
	// The rejected requests stored nothing beyond the two accepted ones.
	var rows int
	if err := env.db.QueryRow(`SELECT COUNT(*) FROM reports WHERE reporter_id=?`, reporter).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("stored %d reports, want the two accepted ones", rows)
	}
}

func TestMyReportsPaginationAndCursorScope(t *testing.T) {
	env := newTopicEnv(t)
	reporter := env.newUser("reporter")
	cookie, csrf := env.sessionCookie(reporter)
	// Five reports about five different topics: the uniqueness rule is per content, so this is allowed.
	for index := 0; index < 5; index++ {
		topic := env.topic(env.boardID, "举报目标 "+itoa(uint64(index)))
		expectStatus(t, env.do("POST", "/api/v1/reports",
			`{"content_id":"`+itoa(topic)+`","reason":"原因 `+itoa(uint64(index))+`"}`, cookie, csrf), 201)
	}

	all := decodeReadPage(t, env.getAs("/api/v1/me/reports?limit=100", cookie))
	if len(all.Items) != 5 {
		t.Fatalf("expected five reports, got %d", len(all.Items))
	}
	var collected []string
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
		path := "/api/v1/me/reports?limit=2"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		page := decodeReadPage(t, env.getAs(path, cookie))
		collected = append(collected, idsOf(t, page)...)
		if !page.HasMore {
			break
		}
		cursor = page.NextCursor
	}
	if len(collected) != len(idsOf(t, all)) {
		t.Fatalf("paged %v, single page %v", collected, idsOf(t, all))
	}
	// A cursor from another list is refused rather than silently mispaging this one.
	expectStatus(t, env.getAs("/api/v1/me/reports?cursor="+encodeCursor(boardCursorScope, 1, 1), cookie), 400)
}
