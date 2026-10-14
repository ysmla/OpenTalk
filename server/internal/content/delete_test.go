package content

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
)

func (e *topicEnv) delete(contentID, actorID uint64) (DeletionResult, error) {
	return e.store.DeleteContent(context.Background(), DeleteContentInput{ActorID: actorID, ContentID: contentID})
}

// deleted reports whether a content row is soft-deleted and whether its body was cleared.
func (e *topicEnv) deleted(contentID uint64) (bool, bool) {
	e.t.Helper()
	var deletedAt sql.NullTime
	var body sql.NullString
	if err := e.db.QueryRow(`SELECT deleted_at,body_md FROM contents WHERE id=?`, contentID).
		Scan(&deletedAt, &body); err != nil {
		e.t.Fatal(err)
	}
	return deletedAt.Valid, !body.Valid
}

func (e *topicEnv) archiveOf(contentID uint64) (title sql.NullString, body string, deletedBy, rootID uint64, purgeOK bool) {
	e.t.Helper()
	var purgeAfter, deletedAt = sql.NullTime{}, sql.NullTime{}
	err := e.db.QueryRow(`SELECT original_title,original_body_md,deleted_by,deletion_root_id,purge_after,deleted_at
 FROM deleted_content_archives WHERE content_id=?`, contentID).
		Scan(&title, &body, &deletedBy, &rootID, &purgeAfter, &deletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		e.t.Fatalf("content %d has no archive row", contentID)
	}
	if err != nil {
		e.t.Fatal(err)
	}
	// The table's CHECK demands exactly 30 days, so the code must derive it rather than guess.
	purgeOK = purgeAfter.Time.Sub(deletedAt.Time) == ArchiveRetention
	return title, body, deletedBy, rootID, purgeOK
}

func TestDeleteCascade(t *testing.T) {
	env := newTopicEnv(t)
	ctx := context.Background()

	t.Run("删除讨论帖连带全部评论并归档原文", func(t *testing.T) {
		topic, err := env.store.CreateTopic(ctx, env.base())
		if err != nil {
			t.Fatal(err)
		}
		first, err := env.comment(NewComment{AuthorID: env.authorID, TopicID: topic, BodyMD: "顶层评论。"})
		if err != nil {
			t.Fatal(err)
		}
		second, err := env.comment(NewComment{AuthorID: env.authorID, TopicID: topic, ReplyToCommentID: first, BodyMD: "回复。"})
		if err != nil {
			t.Fatal(err)
		}
		result, err := env.delete(topic, env.authorID)
		if err != nil {
			t.Fatal(err)
		}
		// The root plus both comments.
		if result.AffectedCount != 3 {
			t.Fatalf("affected_count=%d want 3", result.AffectedCount)
		}
		for _, id := range []uint64{topic, first, second} {
			deleted, bodyCleared := env.deleted(id)
			if !deleted || !bodyCleared {
				t.Fatalf("content %d: deleted=%v bodyCleared=%v", id, deleted, bodyCleared)
			}
			// The original text must survive in the archive, otherwise the 30-day review is impossible.
			if _, _, _, rootID, purgeOK := env.archiveOf(id); rootID != topic || !purgeOK {
				t.Fatalf("content %d archive: root=%d purgeOK=%v", id, rootID, purgeOK)
			}
		}
		title, body, deletedBy, _, _ := env.archiveOf(topic)
		if !title.Valid || title.String != "第一个帖子" {
			t.Fatalf("original title not archived: %v", title)
		}
		if body != "正文内容。" {
			t.Fatalf("original body not archived: %q", body)
		}
		if deletedBy != env.authorID {
			t.Fatalf("deleted_by=%d want %d", deletedBy, env.authorID)
		}
		// The live title is replaced so a listing cannot show the original.
		var liveTitle string
		if err := env.db.QueryRow(`SELECT title FROM topics WHERE content_id=?`, topic).Scan(&liveTitle); err != nil {
			t.Fatal(err)
		}
		if liveTitle != DeletedTopicTitle {
			t.Fatalf("live title=%q want %q", liveTitle, DeletedTopicTitle)
		}
		// And the content is immediately invisible, not eventually.
		if _, err := env.store.PublicContentByID(ctx, topic); !errors.Is(err, ErrContentNotFound) {
			t.Fatalf("deleted content must be unreadable at once, got %v", err)
		}
		if _, err := env.comment(NewComment{AuthorID: env.authorID, TopicID: topic}); !errors.Is(err, ErrTopicNotFound) {
			t.Fatalf("a deleted topic must refuse new interaction, got %v", err)
		}
	})

	t.Run("删除问题连带全部回答与其评论", func(t *testing.T) {
		question := env.newQuestion(false)
		answerA, err := env.answer(question, env.authorID, false)
		if err != nil {
			t.Fatal(err)
		}
		answerB, err := env.answer(question, env.authorID, false)
		if err != nil {
			t.Fatal(err)
		}
		onQuestion, err := env.comment(NewComment{AuthorID: env.authorID, TopicID: question})
		if err != nil {
			t.Fatal(err)
		}
		onAnswerA, err := env.comment(NewComment{AuthorID: env.authorID, TopicID: question, AnswerID: answerA})
		if err != nil {
			t.Fatal(err)
		}
		reply, err := env.comment(NewComment{AuthorID: env.authorID, TopicID: question, AnswerID: answerA, ReplyToCommentID: onAnswerA})
		if err != nil {
			t.Fatal(err)
		}
		result, err := env.delete(question, env.authorID)
		if err != nil {
			t.Fatal(err)
		}
		// root + 2 answers + question comment + answer comment + its reply
		if result.AffectedCount != 6 {
			t.Fatalf("affected_count=%d want 6", result.AffectedCount)
		}
		for _, id := range []uint64{question, answerA, answerB, onQuestion, onAnswerA, reply} {
			if deleted, _ := env.deleted(id); !deleted {
				t.Fatalf("content %d was not deleted", id)
			}
		}
	})

	t.Run("删除回答只连带其评论", func(t *testing.T) {
		question := env.newQuestion(false)
		keep, err := env.answer(question, env.authorID, false)
		if err != nil {
			t.Fatal(err)
		}
		drop, err := env.answer(question, env.authorID, false)
		if err != nil {
			t.Fatal(err)
		}
		onDrop, err := env.comment(NewComment{AuthorID: env.authorID, TopicID: question, AnswerID: drop})
		if err != nil {
			t.Fatal(err)
		}
		onKeep, err := env.comment(NewComment{AuthorID: env.authorID, TopicID: question, AnswerID: keep})
		if err != nil {
			t.Fatal(err)
		}
		onQuestion, err := env.comment(NewComment{AuthorID: env.authorID, TopicID: question})
		if err != nil {
			t.Fatal(err)
		}
		result, err := env.delete(drop, env.authorID)
		if err != nil {
			t.Fatal(err)
		}
		if result.AffectedCount != 2 {
			t.Fatalf("affected_count=%d want 2", result.AffectedCount)
		}
		for _, id := range []uint64{drop, onDrop} {
			if deleted, _ := env.deleted(id); !deleted {
				t.Fatalf("content %d should be deleted", id)
			}
		}
		// Everything outside the answer must survive untouched.
		for _, id := range []uint64{question, keep, onKeep, onQuestion} {
			if deleted, _ := env.deleted(id); deleted {
				t.Fatalf("content %d must not be deleted", id)
			}
		}
	})

	t.Run("删除评论递归删除回复但不动祖先与无关分支", func(t *testing.T) {
		topic, err := env.store.CreateTopic(ctx, env.base())
		if err != nil {
			t.Fatal(err)
		}
		// Two independent top-level threads, and a three-deep chain in the first.
		rootA, err := env.comment(NewComment{AuthorID: env.authorID, TopicID: topic, BodyMD: "线程A根。"})
		if err != nil {
			t.Fatal(err)
		}
		rootB, err := env.comment(NewComment{AuthorID: env.authorID, TopicID: topic, BodyMD: "线程B根。"})
		if err != nil {
			t.Fatal(err)
		}
		childA, err := env.comment(NewComment{AuthorID: env.authorID, TopicID: topic, ReplyToCommentID: rootA, BodyMD: "A的直接回复。"})
		if err != nil {
			t.Fatal(err)
		}
		grandChild, err := env.comment(NewComment{AuthorID: env.authorID, TopicID: topic, ReplyToCommentID: childA, BodyMD: "A的间接回复。"})
		if err != nil {
			t.Fatal(err)
		}
		childB, err := env.comment(NewComment{AuthorID: env.authorID, TopicID: topic, ReplyToCommentID: rootB, BodyMD: "B的回复。"})
		if err != nil {
			t.Fatal(err)
		}
		result, err := env.delete(childA, env.authorID)
		if err != nil {
			t.Fatal(err)
		}
		// childA and the grandchild, nothing else.
		if result.AffectedCount != 2 {
			t.Fatalf("affected_count=%d want 2", result.AffectedCount)
		}
		for _, id := range []uint64{childA, grandChild} {
			if deleted, _ := env.deleted(id); !deleted {
				t.Fatalf("content %d should be deleted", id)
			}
		}
		// The ancestor must survive: deleting a reply must not take its parent with it.
		for _, id := range []uint64{topic, rootA, rootB, childB} {
			if deleted, _ := env.deleted(id); deleted {
				t.Fatalf("content %d must survive", id)
			}
		}
	})

	t.Run("非作者被拒且管理员无捷径", func(t *testing.T) {
		topic, err := env.store.CreateTopic(ctx, env.base())
		if err != nil {
			t.Fatal(err)
		}
		other := env.createUser("deleter")
		if _, err := env.delete(topic, other); !errors.Is(err, ErrNotContentOwner) {
			t.Fatalf("want ErrNotContentOwner, got %v", err)
		}
		admin := env.createUser("deleteadmin")
		if _, err := env.db.Exec(`UPDATE users SET role='admin' WHERE id=?`, admin); err != nil {
			t.Fatal(err)
		}
		if _, err := env.delete(topic, admin); !errors.Is(err, ErrNotContentOwner) {
			t.Fatalf("an administrator must use the moderation route, got %v", err)
		}
		if deleted, _ := env.deleted(topic); deleted {
			t.Fatal("a refused delete must change nothing")
		}
	})

	t.Run("重复删除按不存在处理", func(t *testing.T) {
		topic, err := env.store.CreateTopic(ctx, env.base())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := env.delete(topic, env.authorID); err != nil {
			t.Fatal(err)
		}
		if _, err := env.delete(topic, env.authorID); !errors.Is(err, ErrContentNotFound) {
			t.Fatalf("want ErrContentNotFound, got %v", err)
		}
	})

	t.Run("禁言账号仍可删除自己的内容", func(t *testing.T) {
		topic, err := env.store.CreateTopic(ctx, env.base())
		if err != nil {
			t.Fatal(err)
		}
		// The permission table allows a muted user to delete their own content; muting only forbids new
		// or edited speech.
		if _, err := env.db.Exec(`INSERT INTO user_restrictions (user_id,kind,reason,starts_at,created_by)
 VALUES (?,'mute','测试',UTC_TIMESTAMP(6),?)`, env.authorID, env.authorID); err != nil {
			t.Fatal(err)
		}
		defer func() {
			_, _ = env.db.Exec(`UPDATE user_restrictions SET revoked_at=UTC_TIMESTAMP(6) WHERE user_id=?`, env.authorID)
		}()
		if _, err := env.delete(topic, env.authorID); err != nil {
			t.Fatalf("a muted author must still be able to delete their content: %v", err)
		}
	})
}

func TestDeleteContentEndpoint(t *testing.T) {
	env := newTopicEnv(t)
	question := env.newQuestion(false)
	answerID, err := env.answer(question, env.authorID, false)
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/contents/" + itoa(answerID)

	t.Run("未登录被拒", func(t *testing.T) {
		res := env.do("DELETE", path, "", "", "")
		expectStatus(t, res, 401)
	})

	t.Run("缺少CSRF被拒", func(t *testing.T) {
		cookie, _ := env.sessionCookie(env.authorID)
		res := env.do("DELETE", path, "", cookie, "")
		expectStatus(t, res, 403)
		expectCode(t, res, "CSRF_REJECTED")
	})

	t.Run("作者删除返回契约形状", func(t *testing.T) {
		cookie, csrf := env.sessionCookie(env.authorID)
		res := env.do("DELETE", path, "", cookie, csrf)
		expectStatus(t, res, 200)
		var body struct {
			ContentID     string `json:"content_id"`
			AffectedCount int    `json:"affected_count"`
			DeletedAt     string `json:"deleted_at"`
		}
		if err := json.Unmarshal(res.body, &body); err != nil {
			t.Fatalf("cannot decode %s: %v", res.body, err)
		}
		if body.ContentID != itoa(answerID) || body.AffectedCount != 1 || body.DeletedAt == "" {
			t.Fatalf("unexpected result: %s", res.body)
		}
	})

	t.Run("非作者与坏ID被拒", func(t *testing.T) {
		other := env.createUser("deleteendpoint")
		cookie, csrf := env.sessionCookie(other)
		res := env.do("DELETE", "/api/v1/contents/"+itoa(question), "", cookie, csrf)
		expectStatus(t, res, 403)
		expectCode(t, res, "NOT_CONTENT_OWNER")
		res = env.do("DELETE", "/api/v1/contents/abc", "", cookie, csrf)
		expectStatus(t, res, 422)
		expectCode(t, res, "INVALID_REQUEST")
	})
}
