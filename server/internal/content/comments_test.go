package content

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func (e *topicEnv) comment(in NewComment) (uint64, error) {
	in.MaxAttachments = 9
	if in.BodyMD == "" {
		in.BodyMD = "一条评论。"
	}
	return e.store.CreateComment(context.Background(), in)
}

// threadIDs reads the stored thread pointers, which is where the two-level display gets its shape.
func (e *topicEnv) threadIDs(contentID uint64) (answer, parent, replyTo sql.NullInt64) {
	e.t.Helper()
	if err := e.db.QueryRow(`SELECT answer_id,parent_comment_id,reply_to_comment_id FROM comments WHERE content_id=?`,
		contentID).Scan(&answer, &parent, &replyTo); err != nil {
		e.t.Fatal(err)
	}
	return answer, parent, replyTo
}

func TestCommentThreads(t *testing.T) {
	env := newTopicEnv(t)
	question := env.newQuestion(false)
	answerID, err := env.answer(question, env.authorID, false)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("主题评论的两个线程字段都为空", func(t *testing.T) {
		id, err := env.comment(NewComment{AuthorID: env.authorID, TopicID: question})
		if err != nil {
			t.Fatal(err)
		}
		answer, parent, replyTo := env.threadIDs(id)
		if answer.Valid || parent.Valid || replyTo.Valid {
			t.Fatalf("a topic-level comment must leave all three null: %v %v %v", answer, parent, replyTo)
		}
		// The schema documents that these are omitted at the top level.
		body, err := json.Marshal(toCommentResponse(mustComment(t, env, id)))
		if err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"answer_id", "parent_comment_id", "reply_to_comment_id"} {
			if strings.Contains(string(body), `"`+field+`"`) {
				t.Fatalf("top-level comment must omit %s: %s", field, body)
			}
		}
	})

	t.Run("回答下的评论带answer_id", func(t *testing.T) {
		id, err := env.comment(NewComment{AuthorID: env.authorID, TopicID: question, AnswerID: answerID})
		if err != nil {
			t.Fatal(err)
		}
		answer, parent, replyTo := env.threadIDs(id)
		if !answer.Valid || answer.Int64 != int64(answerID) {
			t.Fatalf("answer_id not stored: %v", answer)
		}
		if parent.Valid || replyTo.Valid {
			t.Fatal("a top-level answer comment must not set the thread pointers")
		}
	})

	t.Run("回复顶层评论时parent与reply_to相同", func(t *testing.T) {
		top, err := env.comment(NewComment{AuthorID: env.authorID, TopicID: question})
		if err != nil {
			t.Fatal(err)
		}
		reply, err := env.comment(NewComment{AuthorID: env.authorID, TopicID: question, ReplyToCommentID: top})
		if err != nil {
			t.Fatal(err)
		}
		_, parent, replyTo := env.threadIDs(reply)
		if !parent.Valid || !replyTo.Valid || parent.Int64 != int64(top) || replyTo.Int64 != int64(top) {
			t.Fatalf("replying to a top-level comment: parent=%v reply_to=%v want both %d", parent, replyTo, top)
		}
	})

	t.Run("回复回复时parent保持线程根", func(t *testing.T) {
		top, err := env.comment(NewComment{AuthorID: env.authorID, TopicID: question})
		if err != nil {
			t.Fatal(err)
		}
		first, err := env.comment(NewComment{AuthorID: env.authorID, TopicID: question, ReplyToCommentID: top})
		if err != nil {
			t.Fatal(err)
		}
		second, err := env.comment(NewComment{AuthorID: env.authorID, TopicID: question, ReplyToCommentID: first})
		if err != nil {
			t.Fatal(err)
		}
		_, parent, replyTo := env.threadIDs(second)
		// The two-level display needs the original root, not the immediate parent.
		if parent.Int64 != int64(top) {
			t.Fatalf("parent should stay the thread root %d, got %v", top, parent)
		}
		if replyTo.Int64 != int64(first) {
			t.Fatalf("reply_to should be the comment actually answered %d, got %v", first, replyTo)
		}
	})

	t.Run("不能跨目标回复", func(t *testing.T) {
		topicComment, err := env.comment(NewComment{AuthorID: env.authorID, TopicID: question})
		if err != nil {
			t.Fatal(err)
		}
		answerComment, err := env.comment(NewComment{AuthorID: env.authorID, TopicID: question, AnswerID: answerID})
		if err != nil {
			t.Fatal(err)
		}
		// A NULL answer_id escapes the composite foreign key, so these two comparisons are the only
		// thing standing between the client and a comment attached to the wrong target.
		if _, err := env.comment(NewComment{AuthorID: env.authorID, TopicID: question, AnswerID: answerID, ReplyToCommentID: topicComment}); !errors.Is(err, ErrReplyTarget) {
			t.Fatalf("answering a topic-level comment from an answer thread must be refused, got %v", err)
		}
		if _, err := env.comment(NewComment{AuthorID: env.authorID, TopicID: question, ReplyToCommentID: answerComment}); !errors.Is(err, ErrReplyTarget) {
			t.Fatalf("answering an answer-level comment from the topic thread must be refused, got %v", err)
		}
		// The matching pairing is accepted in both directions.
		if _, err := env.comment(NewComment{AuthorID: env.authorID, TopicID: question, AnswerID: answerID, ReplyToCommentID: answerComment}); err != nil {
			t.Fatalf("a matching answer-level reply must be accepted: %v", err)
		}
	})

	t.Run("回复目标必须属于同一主题", func(t *testing.T) {
		other := env.newQuestion(false)
		otherComment, err := env.comment(NewComment{AuthorID: env.authorID, TopicID: other})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := env.comment(NewComment{AuthorID: env.authorID, TopicID: question, ReplyToCommentID: otherComment}); !errors.Is(err, ErrReplyTarget) {
			t.Fatalf("a reply target in another discussion must be refused, got %v", err)
		}
	})

	t.Run("回答必须属于该主题", func(t *testing.T) {
		other := env.newQuestion(false)
		otherAnswer, err := env.answer(other, env.authorID, false)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := env.comment(NewComment{AuthorID: env.authorID, TopicID: question, AnswerID: otherAnswer}); !errors.Is(err, ErrCommentTarget) {
			t.Fatalf("an answer from another question must be refused, got %v", err)
		}
	})

	t.Run("已解决问题禁止评论", func(t *testing.T) {
		solved := env.newQuestion(false)
		solvedAnswer, err := env.answer(solved, env.authorID, false)
		if err != nil {
			t.Fatal(err)
		}
		env.setSolved(solved)
		if _, err := env.comment(NewComment{AuthorID: env.authorID, TopicID: solved}); !errors.Is(err, ErrTopicSolved) {
			t.Fatalf("topic comment on a solved question: want ErrTopicSolved, got %v", err)
		}
		if _, err := env.comment(NewComment{AuthorID: env.authorID, TopicID: solved, AnswerID: solvedAnswer}); !errors.Is(err, ErrTopicSolved) {
			t.Fatalf("answer comment on a solved question: want ErrTopicSolved, got %v", err)
		}
	})

	t.Run("评论推进主题活跃时间", func(t *testing.T) {
		target := env.newQuestion(false)
		var before string
		if err := env.db.QueryRow(`SELECT last_activity_at FROM topics WHERE content_id=?`, target).Scan(&before); err != nil {
			t.Fatal(err)
		}
		if _, err := env.comment(NewComment{AuthorID: env.authorID, TopicID: target}); err != nil {
			t.Fatal(err)
		}
		var after string
		if err := env.db.QueryRow(`SELECT last_activity_at FROM topics WHERE content_id=?`, target).Scan(&after); err != nil {
			t.Fatal(err)
		}
		if after <= before {
			t.Fatalf("a comment must move the topic: before=%s after=%s", before, after)
		}
	})
}

func TestAnonymousCommentIdentity(t *testing.T) {
	env := newTopicEnv(t)

	t.Run("匿名楼主的评论强制匿名并沿用编号", func(t *testing.T) {
		question := env.newQuestion(true)
		id, err := env.comment(NewComment{AuthorID: env.authorID, TopicID: question, RequestedAnonymous: false})
		if err != nil {
			t.Fatal(err)
		}
		var isAnonymous bool
		if err := env.db.QueryRow(`SELECT is_anonymous FROM contents WHERE id=?`, id).Scan(&isAnonymous); err != nil {
			t.Fatal(err)
		}
		if !isAnonymous {
			t.Fatal("the anonymous topic owner's comment was stored as named")
		}
		author := toCommentResponse(mustComment(t, env, id)).Author
		if author.Mode != "anonymous" || author.DisplayName != "楼主" || author.DisplayNumber != 1 {
			t.Fatalf("forced anonymous comment rendered wrong: %+v", author)
		}
	})

	t.Run("其他参与者用讨论内编号", func(t *testing.T) {
		question := env.newQuestion(false)
		other := env.createUser("commenter")
		id, err := env.comment(NewComment{AuthorID: other, TopicID: question, RequestedAnonymous: true})
		if err != nil {
			t.Fatal(err)
		}
		author := toCommentResponse(mustComment(t, env, id)).Author
		if author.DisplayName != "匿名#1" || author.IsTopicOwner {
			t.Fatalf("another participant rendered wrong: %+v", author)
		}
	})
}

func TestCreateCommentEndpoint(t *testing.T) {
	env := newTopicEnv(t)
	question := env.newQuestion(false)
	payload := `{"topic_id":"` + itoa(question) + `","body_md":"通过接口评论。","is_anonymous":false}`

	t.Run("未登录被拒", func(t *testing.T) {
		res := env.do("POST", "/api/v1/comments", payload, "", "")
		expectStatus(t, res, 401)
	})

	t.Run("缺少CSRF被拒", func(t *testing.T) {
		cookie, _ := env.sessionCookie(env.authorID)
		res := env.do("POST", "/api/v1/comments", payload, cookie, "")
		expectStatus(t, res, 403)
		expectCode(t, res, "CSRF_REJECTED")
	})

	t.Run("正常评论返回201与契约形状", func(t *testing.T) {
		cookie, csrf := env.sessionCookie(env.authorID)
		res := env.do("POST", "/api/v1/comments", payload, cookie, csrf)
		expectStatus(t, res, 201)
		var body struct {
			ID      string `json:"id"`
			State   string `json:"state"`
			Kind    string `json:"kind"`
			TopicID string `json:"topic_id"`
			BodyMD  string `json:"body_md"`
			Author  struct {
				Mode string `json:"mode"`
			} `json:"author"`
			Attachments []any `json:"attachments"`
		}
		if err := json.Unmarshal(res.body, &body); err != nil {
			t.Fatalf("cannot decode %s: %v", res.body, err)
		}
		if body.State != "published" || body.Kind != "comment" || body.TopicID != itoa(question) {
			t.Fatalf("wrong state/kind/topic: %s/%s/%s", body.State, body.Kind, body.TopicID)
		}
		if body.Attachments == nil {
			t.Fatal("attachments must serialise as [] rather than null")
		}
	})

	t.Run("跨目标回复返回422", func(t *testing.T) {
		cookie, csrf := env.sessionCookie(env.authorID)
		answerID, err := env.answer(question, env.authorID, false)
		if err != nil {
			t.Fatal(err)
		}
		answerComment, err := env.comment(NewComment{AuthorID: env.authorID, TopicID: question, AnswerID: answerID})
		if err != nil {
			t.Fatal(err)
		}
		body := `{"topic_id":"` + itoa(question) + `","reply_to_comment_id":"` + itoa(answerComment) + `","body_md":"回复。","is_anonymous":false}`
		res := env.do("POST", "/api/v1/comments", body, cookie, csrf)
		expectStatus(t, res, 422)
		expectCode(t, res, "INVALID_COMMENT_TARGET")
	})

	t.Run("危险正文与坏ID被拒", func(t *testing.T) {
		cookie, csrf := env.sessionCookie(env.authorID)
		body := `{"topic_id":"` + itoa(question) + `","body_md":"<b>原始HTML</b>","is_anonymous":false}`
		res := env.do("POST", "/api/v1/comments", body, cookie, csrf)
		expectStatus(t, res, 422)
		expectCode(t, res, "BODY_RAW_HTML")
		body = `{"topic_id":"` + itoa(question) + `","reply_to_comment_id":"abc","body_md":"回复。","is_anonymous":false}`
		res = env.do("POST", "/api/v1/comments", body, cookie, csrf)
		expectStatus(t, res, 422)
		expectCode(t, res, "INVALID_REQUEST")
	})
}

func mustComment(t *testing.T, env *topicEnv, contentID uint64) CommentView {
	t.Helper()
	view, err := env.store.CommentByContentID(context.Background(), contentID)
	if err != nil {
		t.Fatal(err)
	}
	return view
}
