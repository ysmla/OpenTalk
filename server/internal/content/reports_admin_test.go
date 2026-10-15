package content

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// The administrative half of reporting. The tests concentrate on the two boundaries that make it safe to
// hand an administrator a queue: closing a report must not itself touch the content, and the queue must not
// become a way to see who wrote anonymous content.

type adminReportPage struct {
	Items      []map[string]any `json:"items"`
	HasMore    bool             `json:"has_more"`
	NextCursor string           `json:"next_cursor"`
}

func decodeAdminReports(t *testing.T, res response) adminReportPage {
	t.Helper()
	var page adminReportPage
	if err := json.Unmarshal(res.body, &page); err != nil {
		t.Fatalf("cannot decode %s: %v", string(res.body), err)
	}
	return page
}

func TestAdminReportQueueAndResolution(t *testing.T) {
	env := newTopicEnv(t)
	topic := env.topic(env.boardID, "被举报的主题")
	reporter := env.newUser("reporter")
	reporterCookie, reporterCSRF := env.sessionCookie(reporter)
	admin := env.newAdmin("moderator")
	adminCookie, adminCSRF := env.sessionCookie(admin)

	expectStatus(t, env.do("POST", "/api/v1/reports",
		`{"content_id":"`+itoa(topic)+`","reason":"包含广告","details":"补充说明"}`,
		reporterCookie, reporterCSRF), 201)

	// The queue is admin-only.
	expectStatus(t, env.get("/api/v1/admin/reports"), 401)
	memberCookie, _ := env.sessionCookie(env.newUser("member"))
	expectStatus(t, env.getAs("/api/v1/admin/reports", memberCookie), 403)

	page := decodeAdminReports(t, env.getAs("/api/v1/admin/reports", adminCookie))
	if len(page.Items) != 1 {
		t.Fatalf("queue has %d items: %s", len(page.Items), string(pageMustBody(t, env, adminCookie)))
	}
	entry := page.Items[0]
	report, _ := entry["report"].(map[string]any)
	reporterInfo, _ := entry["reporter"].(map[string]any)
	if report["reason"] != "包含广告" || report["details"] != "补充说明" {
		t.Fatalf("the queue does not carry the report: %v", entry)
	}
	if reporterInfo == nil || reporterInfo["internal_user_id"] == nil {
		t.Fatalf("the queue is expected to name the reporter to an administrator: %v", entry)
	}
	if reporterInfo["account_status"] != "active" || reporterInfo["role"] != "user" {
		t.Fatalf("the reporter's account state is wrong: %v", reporterInfo)
	}
	if _, present := entry["handler"]; present {
		t.Fatalf("an open report has a handler: %v", entry)
	}

	// Closing it. The response is the administrative shape, now with the handler recorded.
	closed := env.do("PATCH", "/api/v1/admin/reports/"+report["id"].(string),
		`{"status":"resolved","resolution_note":"已隐藏并通知作者"}`, adminCookie, adminCSRF)
	expectStatus(t, closed, 200)
	var resolved map[string]any
	if err := json.Unmarshal(closed.body, &resolved); err != nil {
		t.Fatal(err)
	}
	closedReport, _ := resolved["report"].(map[string]any)
	handler, _ := resolved["handler"].(map[string]any)
	if closedReport["status"] != reportResolved || closedReport["resolution_note"] != "已隐藏并通知作者" {
		t.Fatalf("the closed report does not carry its outcome: %v", closedReport)
	}
	if closedReport["resolved_at"] == nil {
		t.Fatalf("the closed report has no resolution time: %v", closedReport)
	}
	if handler == nil || handler["internal_user_id"] != formatID(admin) {
		t.Fatalf("the handler was not recorded: %v", resolved)
	}

	// The reporter sees the same outcome through their own endpoint: the two halves agree.
	own := decodeReadPage(t, env.getAs("/api/v1/me/reports", reporterCookie))
	var ownRecord map[string]any
	if err := json.Unmarshal(own.Items[0], &ownRecord); err != nil {
		t.Fatal(err)
	}
	if ownRecord["resolution_note"] != "已隐藏并通知作者" || ownRecord["status"] != reportResolved {
		t.Fatalf("the reporter cannot see the outcome: %v", ownRecord)
	}
	// And it is out of the queue: the queue is the work that is left.
	if after := decodeAdminReports(t, env.getAs("/api/v1/admin/reports", adminCookie)); len(after.Items) != 0 {
		t.Fatalf("a closed report stayed in the queue: %v", after.Items)
	}

	// Closing it a second time is a conflict: the transition is one-way.
	repeat := env.do("PATCH", "/api/v1/admin/reports/"+report["id"].(string),
		`{"status":"dismissed","resolution_note":"再结一次"}`, adminCookie, adminCSRF)
	expectStatus(t, repeat, 409)
	expectCode(t, repeat, "REPORT_ALREADY_CLOSED")
}

func TestResolvingAReportDoesNotTouchTheContent(t *testing.T) {
	env := newTopicEnv(t)
	topic := env.topic(env.boardID, "被举报的主题")
	reporter := env.newUser("reporter")
	reporterCookie, reporterCSRF := env.sessionCookie(reporter)
	adminCookie, adminCSRF := env.sessionCookie(env.newAdmin("moderator"))
	expectStatus(t, env.do("POST", "/api/v1/reports",
		`{"content_id":"`+itoa(topic)+`","reason":"原因"}`, reporterCookie, reporterCSRF), 201)
	page := decodeAdminReports(t, env.getAs("/api/v1/admin/reports", adminCookie))
	reportID := page.Items[0]["report"].(map[string]any)["id"].(string)

	// The decision is "dismissed": nothing happened to the content, and that is a legitimate outcome. If
	// closing a report also hid the content, this assertion is the one that would fail.
	expectStatus(t, env.do("PATCH", "/api/v1/admin/reports/"+reportID,
		`{"status":"dismissed","resolution_note":"不成立"}`, adminCookie, adminCSRF), 200)
	var status string
	if err := env.db.QueryRow(`SELECT moderation_status FROM contents WHERE id=?`, topic).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "visible" {
		t.Fatalf("closing a report changed the content to %q", status)
	}
	expectStatus(t, env.get("/api/v1/contents/"+itoa(topic)), 200)
	// The decision is in the audit trail, linked to the report it closed.
	var auditRows int
	if err := env.db.QueryRow(`SELECT COUNT(*) FROM moderation_actions
 WHERE action='resolve_report' AND report_id=? AND actor_id=?`, reportID, 0).Scan(&auditRows); err != nil {
		t.Fatal(err)
	}
	var linked int
	if err := env.db.QueryRow(`SELECT COUNT(*) FROM moderation_actions WHERE action='resolve_report' AND report_id=?`, reportID).Scan(&linked); err != nil {
		t.Fatal(err)
	}
	if linked != 1 {
		t.Fatalf("closing a report wrote %d audit rows", linked)
	}
}

func TestAdminReportQueueNeverRevealsAnonymousAuthors(t *testing.T) {
	env := newTopicEnv(t)
	// An anonymous question, reported by a second account: the administrator must get the reporter's
	// identity and nothing about who wrote the reported content.
	anonymousAuthor := env.newUser("secretive")
	in := env.base()
	in.AuthorID = anonymousAuthor
	in.BoardID = env.qaBoardID
	in.IsAnonymous = true
	in.Title = "匿名提问"
	in.TagIDs = nil
	topic, err := env.store.CreateTopic(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	reporter := env.newUser("reporter")
	reporterCookie, reporterCSRF := env.sessionCookie(reporter)
	expectStatus(t, env.do("POST", "/api/v1/reports",
		`{"content_id":"`+itoa(topic)+`","reason":"匿名内容违规"}`, reporterCookie, reporterCSRF), 201)
	adminCookie, _ := env.sessionCookie(env.newAdmin("moderator"))

	body := string(pageMustBody(t, env, adminCookie))
	for _, secret := range []string{env.nicknameOf(anonymousAuthor), env.publicIDOf(anonymousAuthor)} {
		if strings.Contains(body, secret) {
			t.Fatalf("the administrative queue leaked the anonymous author (%s): %s", secret, body)
		}
	}
	// Sanity: the queue did return the report, so the assertion above is not passing on an empty page.
	if !strings.Contains(body, "匿名内容违规") {
		t.Fatalf("the queue did not return the report: %s", body)
	}
}

// pageMustBody reads the admin queue once and returns the raw body, so a test can assert both on the decoded
// shape and on the bytes that were actually sent.
func pageMustBody(t *testing.T, env *topicEnv, cookie string) []byte {
	t.Helper()
	res := env.getAs("/api/v1/admin/reports", cookie)
	expectStatus(t, res, 200)
	return res.body
}
