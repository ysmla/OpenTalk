package content

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"opentalk/server/internal/media"
)

func (e *topicEnv) onDisk(key string) bool {
	e.t.Helper()
	_, err := os.Stat(filepath.Join(e.mediaRoot, key))
	return err == nil
}

func (e *topicEnv) cleanup() CleanupReport {
	e.t.Helper()
	report, err := e.store.Cleanup(context.Background(), CleanupOptions{
		PendingTTLHours:   24,
		OrphanCleanupDays: 7,
		Media:             &media.Store{Root: e.mediaRoot},
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return report
}

// uploadOne uploads a real PNG and returns its id and storage key.
func (e *topicEnv) uploadOne(cookie, csrf string) (uint64, string) {
	e.t.Helper()
	res := e.upload(pngOf(e.t, 12, 12), "x.png", cookie, csrf)
	expectStatus(e.t, res, 201)
	var body struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(res.body, &body); err != nil {
		e.t.Fatal(err)
	}
	id := parseUint(e.t, body.ID)
	_, _, key := e.attachmentRow(id)
	return id, key
}

func TestCleanup(t *testing.T) {
	env := newTopicEnv(t)
	cookie, csrf := env.sessionCookie(env.authorID)

	t.Run("过期待用附件被清除且未过期的保留", func(t *testing.T) {
		expired, expiredKey := env.uploadOne(cookie, csrf)
		fresh, freshKey := env.uploadOne(cookie, csrf)
		// Backdate only the first one past its window.
		if _, err := env.db.Exec(`UPDATE attachments SET delete_after=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 HOUR) WHERE id=?`, expired); err != nil {
			t.Fatal(err)
		}
		report := env.cleanup()
		if report.PendingRemoved != 1 {
			t.Fatalf("PendingRemoved=%d want 1", report.PendingRemoved)
		}
		if env.onDisk(expiredKey) {
			t.Fatal("the expired file is still on disk")
		}
		status, _, _ := env.attachmentRow(expired)
		if status != "deleted" {
			t.Fatalf("status=%s want deleted", status)
		}
		// The one still inside its window must be untouched.
		if !env.onDisk(freshKey) {
			t.Fatal("a pending image inside its window was removed")
		}
		if status, _, _ = env.attachmentRow(fresh); status != "pending" {
			t.Fatalf("fresh upload status=%s want pending", status)
		}
	})

	t.Run("已关联的图片即使过期也不清除", func(t *testing.T) {
		// This is the safety net for the promotion rule: even if a row were still pending with an expired
		// deadline, an image some content references must not be swept away.
		id, key := env.uploadOne(cookie, csrf)
		payload := `{"board_id":"` + itoa(env.boardID) + `","title":"引用了图片","body_md":"正文。","is_anonymous":false,"tag_ids":[],"attachment_ids":["` + itoa(id) + `"]}`
		expectStatus(t, env.do("POST", "/api/v1/topics", payload, cookie, csrf), 201)
		// Force the impossible state on purpose.
		if _, err := env.db.Exec(`UPDATE attachments SET status='pending',delete_after=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 HOUR) WHERE id=?`, id); err != nil {
			t.Fatal(err)
		}
		env.cleanup()
		if !env.onDisk(key) {
			t.Fatal("an image referenced by live content was removed")
		}
	})

	t.Run("删除内容后孤儿附件到期才清除", func(t *testing.T) {
		id, key := env.uploadOne(cookie, csrf)
		payload := `{"board_id":"` + itoa(env.boardID) + `","title":"将被删除","body_md":"正文。","is_anonymous":false,"tag_ids":[],"attachment_ids":["` + itoa(id) + `"]}`
		created := env.do("POST", "/api/v1/topics", payload, cookie, csrf)
		expectStatus(t, created, 201)
		var body struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(created.body, &body); err != nil {
			t.Fatal(err)
		}
		expectStatus(t, env.do("DELETE", "/api/v1/contents/"+body.ID, "", cookie, csrf), 200)
		// Inside the orphan window nothing happens yet.
		env.cleanup()
		if !env.onDisk(key) {
			t.Fatal("an image was removed before the orphan window elapsed")
		}
		// The content was deleted just now, but an un-expired archive still refers to it, so the image is
		// deliberately kept until the archive expires.
		// The CHECK requires purge_after to be exactly deleted_at + 30 days, so both move together: the
		// archive expired yesterday and was created 31 days ago.
		if _, err := env.db.Exec(`UPDATE deleted_content_archives
 SET deleted_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 31 DAY), purge_after=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 DAY)
 WHERE content_id=?`, parseUint(t, body.ID)); err != nil {
			t.Fatal(err)
		}
		if _, err := env.db.Exec(`UPDATE contents SET deleted_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 8 DAY) WHERE id=?`, parseUint(t, body.ID)); err != nil {
			t.Fatal(err)
		}
		report := env.cleanup()
		if report.OrphanRemoved != 1 {
			t.Fatalf("OrphanRemoved=%d want 1", report.OrphanRemoved)
		}
		if env.onDisk(key) {
			t.Fatal("the orphaned file is still on disk")
		}
	})

	t.Run("到期归档的原文被清空而标题被置空", func(t *testing.T) {
		topic, err := env.store.CreateTopic(context.Background(), env.base())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := env.delete(topic, env.authorID); err != nil {
			t.Fatal(err)
		}
		// Not yet due.
		if report := env.cleanup(); report.ArchivesPurged != 0 {
			t.Fatalf("ArchivesPurged=%d want 0 before the window", report.ArchivesPurged)
		}
		var body string
		if err := env.db.QueryRow(`SELECT original_body_md FROM deleted_content_archives WHERE content_id=?`, topic).Scan(&body); err != nil {
			t.Fatal(err)
		}
		if body == "" {
			t.Fatal("the archived text was purged too early")
		}
		if _, err := env.db.Exec(`UPDATE deleted_content_archives
 SET deleted_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 31 DAY), purge_after=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 DAY)
 WHERE content_id=?`, topic); err != nil {
			t.Fatal(err)
		}
		report := env.cleanup()
		if report.ArchivesPurged != 1 {
			t.Fatalf("ArchivesPurged=%d want 1", report.ArchivesPurged)
		}
		var title sql.NullString
		if err := env.db.QueryRow(`SELECT original_body_md,original_title FROM deleted_content_archives WHERE content_id=?`, topic).
			Scan(&body, &title); err != nil {
			t.Fatal(err)
		}
		if body != "" || title.Valid {
			t.Fatalf("expired archive still holds material: body=%q title=%v", body, title)
		}
		// Running again must be a no-op rather than double counting.
		if again := env.cleanup(); again.ArchivesPurged != 0 {
			t.Fatalf("a second run purged %d more", again.ArchivesPurged)
		}
	})

	t.Run("文件已缺失的待删除行仍被判为完成", func(t *testing.T) {
		id, key := env.uploadOne(cookie, csrf)
		if _, err := env.db.Exec(`UPDATE attachments SET status='pending_delete',delete_after=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 HOUR) WHERE id=?`, id); err != nil {
			t.Fatal(err)
		}
		// Simulate a crash between marking and unlinking, or a manual removal.
		if err := os.Remove(filepath.Join(env.mediaRoot, key)); err != nil {
			t.Fatal(err)
		}
		report := env.cleanup()
		if len(report.Failures) != 0 {
			t.Fatalf("a missing file is success, got failures: %v", report.Failures)
		}
		if status, _, _ := env.attachmentRow(id); status != "deleted" {
			t.Fatalf("status=%s want deleted", status)
		}
	})

	t.Run("重复运行是安全的", func(t *testing.T) {
		first := env.cleanup()
		second := env.cleanup()
		if second.PendingRemoved != 0 || second.OrphanRemoved != 0 || second.ArchivesPurged != 0 {
			t.Fatalf("a second run must find nothing: %+v", second)
		}
		if first.Failures == nil && second.Failures == nil {
			// Both clean; nothing further to assert beyond the counts above.
			return
		}
	})
}
