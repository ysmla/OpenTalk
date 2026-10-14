package content

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
)

func (e *topicEnv) edit(in EditContentInput) error {
	return e.store.EditContent(context.Background(), in)
}

func TestEditContent(t *testing.T) {
	env := newTopicEnv(t)
	ctx := context.Background()

	t.Run("作者在时限内编辑正文并写入edited_at", func(t *testing.T) {
		topic, err := env.store.CreateTopic(ctx, env.base())
		if err != nil {
			t.Fatal(err)
		}
		if err := env.edit(EditContentInput{ActorID: env.authorID, ContentID: topic, BodyMD: "改过的正文。"}); err != nil {
			t.Fatal(err)
		}
		var body string
		var editedAt sql.NullTime
		if err := env.db.QueryRow(`SELECT body_md,edited_at FROM contents WHERE id=?`, topic).Scan(&body, &editedAt); err != nil {
			t.Fatal(err)
		}
		if body != "改过的正文。" {
			t.Fatalf("body not updated: %q", body)
		}
		if !editedAt.Valid {
			t.Fatal("edited_at was not set")
		}
	})

	t.Run("非作者被拒", func(t *testing.T) {
		topic, err := env.store.CreateTopic(ctx, env.base())
		if err != nil {
			t.Fatal(err)
		}
		other := env.createUser("editor")
		if err := env.edit(EditContentInput{ActorID: other, ContentID: topic, BodyMD: "别人改的。"}); !errors.Is(err, ErrNotContentOwner) {
			t.Fatalf("want ErrNotContentOwner, got %v", err)
		}
		// An administrator gets no shortcut either: this is authorization, not a moderation action.
		admin := env.createUser("editadmin")
		if _, err := env.db.Exec(`UPDATE users SET role='admin' WHERE id=?`, admin); err != nil {
			t.Fatal(err)
		}
		if err := env.edit(EditContentInput{ActorID: admin, ContentID: topic, BodyMD: "管理员改的。"}); !errors.Is(err, ErrNotContentOwner) {
			t.Fatalf("an administrator: want ErrNotContentOwner, got %v", err)
		}
	})

	t.Run("超过30分钟时限被拒", func(t *testing.T) {
		topic, err := env.store.CreateTopic(ctx, env.base())
		if err != nil {
			t.Fatal(err)
		}
		// One minute past the window. The boundary matters more than the value, so it is set explicitly.
		if _, err := env.db.Exec(`UPDATE contents SET created_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 31 MINUTE) WHERE id=?`, topic); err != nil {
			t.Fatal(err)
		}
		if err := env.edit(EditContentInput{ActorID: env.authorID, ContentID: topic, BodyMD: "晚了。"}); !errors.Is(err, ErrEditWindowClosed) {
			t.Fatalf("want ErrEditWindowClosed, got %v", err)
		}
		// Just inside the window must still work, so the check is not simply always refusing.
		if _, err := env.db.Exec(`UPDATE contents SET created_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 29 MINUTE) WHERE id=?`, topic); err != nil {
			t.Fatal(err)
		}
		if err := env.edit(EditContentInput{ActorID: env.authorID, ContentID: topic, BodyMD: "还来得及。"}); err != nil {
			t.Fatalf("29 minutes must still be editable: %v", err)
		}
	})

	t.Run("编辑不延长时限", func(t *testing.T) {
		topic, err := env.store.CreateTopic(ctx, env.base())
		if err != nil {
			t.Fatal(err)
		}
		if err := env.edit(EditContentInput{ActorID: env.authorID, ContentID: topic, BodyMD: "第一次编辑。"}); err != nil {
			t.Fatal(err)
		}
		// The window runs from publication, so moving created_at back is enough to close it even though
		// edited_at is recent.
		if _, err := env.db.Exec(`UPDATE contents SET created_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 31 MINUTE) WHERE id=?`, topic); err != nil {
			t.Fatal(err)
		}
		if err := env.edit(EditContentInput{ActorID: env.authorID, ContentID: topic, BodyMD: "第二次编辑。"}); !errors.Is(err, ErrEditWindowClosed) {
			t.Fatalf("editing must not slide the window: %v", err)
		}
	})

	t.Run("锁定主题禁止编辑正文", func(t *testing.T) {
		topic, err := env.store.CreateTopic(ctx, env.base())
		if err != nil {
			t.Fatal(err)
		}
		env.setTopicFlag(topic, "is_locked", true)
		if err := env.edit(EditContentInput{ActorID: env.authorID, ContentID: topic, BodyMD: "锁定后改。"}); !errors.Is(err, ErrLockedEdit) {
			t.Fatalf("want ErrLockedEdit, got %v", err)
		}
	})

	t.Run("已解决不额外禁止编辑", func(t *testing.T) {
		question := env.newQuestion(false)
		env.setSolved(question)
		if err := env.edit(EditContentInput{ActorID: env.authorID, ContentID: question, BodyMD: "已解决但仍可编辑。"}); err != nil {
			t.Fatalf("solved must not block editing: %v", err)
		}
	})

	t.Run("禁言账号不能编辑", func(t *testing.T) {
		topic, err := env.store.CreateTopic(ctx, env.base())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := env.db.Exec(`INSERT INTO user_restrictions (user_id,kind,reason,starts_at,created_by)
 VALUES (?,'mute','测试',UTC_TIMESTAMP(6),?)`, env.authorID, env.authorID); err != nil {
			t.Fatal(err)
		}
		if err := env.edit(EditContentInput{ActorID: env.authorID, ContentID: topic, BodyMD: "禁言后改。"}); !errors.Is(err, ErrAccountMuted) {
			t.Fatalf("want ErrAccountMuted, got %v", err)
		}
		if _, err := env.db.Exec(`UPDATE user_restrictions SET revoked_at=UTC_TIMESTAMP(6) WHERE user_id=?`, env.authorID); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("已删除内容按不存在处理", func(t *testing.T) {
		topic, err := env.store.CreateTopic(ctx, env.base())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := env.db.Exec(`UPDATE contents SET deleted_at=UTC_TIMESTAMP(6),body_md=NULL WHERE id=?`, topic); err != nil {
			t.Fatal(err)
		}
		if err := env.edit(EditContentInput{ActorID: env.authorID, ContentID: topic, BodyMD: "已删除。"}); !errors.Is(err, ErrContentNotFound) {
			t.Fatalf("want ErrContentNotFound, got %v", err)
		}
	})

	t.Run("标题只能改主题", func(t *testing.T) {
		topic, err := env.store.CreateTopic(ctx, env.base())
		if err != nil {
			t.Fatal(err)
		}
		if err := env.edit(EditContentInput{ActorID: env.authorID, ContentID: topic, BodyMD: "正文。", Title: "新标题", NewTitle: true}); err != nil {
			t.Fatal(err)
		}
		var title string
		if err := env.db.QueryRow(`SELECT title FROM topics WHERE content_id=?`, topic).Scan(&title); err != nil {
			t.Fatal(err)
		}
		if title != "新标题" {
			t.Fatalf("title not updated: %q", title)
		}
		question := env.newQuestion(false)
		answerID, err := env.answer(question, env.authorID, false)
		if err != nil {
			t.Fatal(err)
		}
		if err := env.edit(EditContentInput{ActorID: env.authorID, ContentID: answerID, BodyMD: "正文。", Title: "回答不该有标题", NewTitle: true}); !errors.Is(err, ErrTitleOnNonTopic) {
			t.Fatalf("want ErrTitleOnNonTopic, got %v", err)
		}
	})

	t.Run("附件省略表示不改空数组表示清空", func(t *testing.T) {
		topic, err := env.store.CreateTopic(ctx, env.base())
		if err != nil {
			t.Fatal(err)
		}
		first := env.createAttachment(env.authorID, "pending")
		second := env.createAttachment(env.authorID, "pending")
		if err := env.edit(EditContentInput{ActorID: env.authorID, ContentID: topic, BodyMD: "正文。", Attachments: []uint64{first}, NewAttach: true, MaxAttach: 9}); err != nil {
			t.Fatal(err)
		}
		if got := env.attachmentsOf(topic); len(got) != 1 || got[0] != first {
			t.Fatalf("attachments not set: %v", got)
		}
		// Omitted: unchanged.
		if err := env.edit(EditContentInput{ActorID: env.authorID, ContentID: topic, BodyMD: "正文2。"}); err != nil {
			t.Fatal(err)
		}
		if got := env.attachmentsOf(topic); len(got) != 1 {
			t.Fatalf("an omitted attachment list must not change the images: %v", got)
		}
		// Empty array: clear them all.
		if err := env.edit(EditContentInput{ActorID: env.authorID, ContentID: topic, BodyMD: "正文3。", Attachments: nil, NewAttach: true, MaxAttach: 9}); err != nil {
			t.Fatal(err)
		}
		if got := env.attachmentsOf(topic); len(got) != 0 {
			t.Fatalf("an empty attachment list must clear the images: %v", got)
		}
		// Somebody else's attachment is refused.
		if err := env.edit(EditContentInput{ActorID: env.authorID, ContentID: topic, BodyMD: "正文4。", Attachments: []uint64{second + 9999}, NewAttach: true, MaxAttach: 9}); !errors.Is(err, ErrAttachmentUnavailable) {
			t.Fatalf("want ErrAttachmentUnavailable, got %v", err)
		}
	})
}

func (e *topicEnv) attachmentsOf(contentID uint64) []uint64 {
	e.t.Helper()
	rows, err := e.db.Query(`SELECT attachment_id FROM content_attachments WHERE content_id=? ORDER BY attachment_id`, contentID)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []uint64
	for rows.Next() {
		var id uint64
		if err := rows.Scan(&id); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, id)
	}
	return out
}

func TestEditContentEndpoint(t *testing.T) {
	env := newTopicEnv(t)
	question := env.newQuestion(false)
	answerID, err := env.answer(question, env.authorID, false)
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/contents/" + itoa(answerID)

	t.Run("未登录被拒", func(t *testing.T) {
		res := env.do("PATCH", path, `{"body_md":"改。"}`, "", "")
		expectStatus(t, res, 401)
	})

	t.Run("缺少CSRF被拒", func(t *testing.T) {
		cookie, _ := env.sessionCookie(env.authorID)
		res := env.do("PATCH", path, `{"body_md":"改。"}`, cookie, "")
		expectStatus(t, res, 403)
		expectCode(t, res, "CSRF_REJECTED")
	})

	t.Run("编辑回答返回Answer形状", func(t *testing.T) {
		cookie, csrf := env.sessionCookie(env.authorID)
		res := env.do("PATCH", path, `{"body_md":"改过的回答。"}`, cookie, csrf)
		expectStatus(t, res, 200)
		var body struct {
			Kind     string  `json:"kind"`
			BodyMD   string  `json:"body_md"`
			EditedAt *string `json:"edited_at"`
		}
		if err := json.Unmarshal(res.body, &body); err != nil {
			t.Fatalf("cannot decode %s: %v", res.body, err)
		}
		if body.Kind != "answer" || body.BodyMD != "改过的回答。" {
			t.Fatalf("wrong kind/body: %s/%s", body.Kind, body.BodyMD)
		}
		if body.EditedAt == nil {
			t.Fatalf("edited_at missing: %s", res.body)
		}
	})

	t.Run("编辑主题返回Topic形状", func(t *testing.T) {
		topic, err := env.store.CreateTopic(context.Background(), env.base())
		if err != nil {
			t.Fatal(err)
		}
		cookie, csrf := env.sessionCookie(env.authorID)
		res := env.do("PATCH", "/api/v1/contents/"+itoa(topic), `{"body_md":"改过的主题。","title":"新标题"}`, cookie, csrf)
		expectStatus(t, res, 200)
		var body struct {
			Kind  string `json:"kind"`
			Title string `json:"title"`
		}
		if err := json.Unmarshal(res.body, &body); err != nil {
			t.Fatal(err)
		}
		if body.Kind != "discussion" || body.Title != "新标题" {
			t.Fatalf("wrong kind/title: %s/%s", body.Kind, body.Title)
		}
	})

	t.Run("危险正文与非作者被拒", func(t *testing.T) {
		cookie, csrf := env.sessionCookie(env.authorID)
		res := env.do("PATCH", path, `{"body_md":"<script>x</script>"}`, cookie, csrf)
		expectStatus(t, res, 422)
		expectCode(t, res, "BODY_RAW_HTML")
		other := env.createUser("editendpoint")
		otherCookie, otherCSRF := env.sessionCookie(other)
		res = env.do("PATCH", path, `{"body_md":"别人改。"}`, otherCookie, otherCSRF)
		expectStatus(t, res, 403)
		expectCode(t, res, "NOT_CONTENT_OWNER")
	})

	t.Run("坏ID与未知字段被拒", func(t *testing.T) {
		cookie, csrf := env.sessionCookie(env.authorID)
		res := env.do("PATCH", "/api/v1/contents/0", `{"body_md":"改。"}`, cookie, csrf)
		expectStatus(t, res, 422)
		res = env.do("PATCH", path, `{"body_md":"改。","is_anonymous":true}`, cookie, csrf)
		expectStatus(t, res, 400)
		expectCode(t, res, "INVALID_JSON")
	})
}
