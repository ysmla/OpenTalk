package content

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// newQuestion publishes a question in the Q&A board and returns its content id.
func (e *topicEnv) newQuestion(anonymous bool) uint64 {
	e.t.Helper()
	in := e.base()
	in.BoardID = e.qaBoardID
	in.IsAnonymous = anonymous
	in.TagIDs = nil
	id, err := e.store.CreateTopic(context.Background(), in)
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *topicEnv) setTopicFlag(topicID uint64, column string, value bool) {
	e.t.Helper()
	// The column name is a test-controlled literal, never user input.
	if _, err := e.db.Exec(`UPDATE topics SET `+column+`=? WHERE content_id=?`, value, topicID); err != nil {
		e.t.Fatal(err)
	}
}

func (e *topicEnv) setSolved(topicID uint64) {
	e.t.Helper()
	if _, err := e.db.Exec(`UPDATE questions SET resolution_status='solved',resolved_at=UTC_TIMESTAMP(6) WHERE topic_id=?`, topicID); err != nil {
		e.t.Fatal(err)
	}
}

func (e *topicEnv) reopen(topicID uint64) {
	e.t.Helper()
	if _, err := e.db.Exec(`UPDATE questions SET resolution_status='open',resolved_at=NULL WHERE topic_id=?`, topicID); err != nil {
		e.t.Fatal(err)
	}
}

func (e *topicEnv) answer(topicID, authorID uint64, anonymous bool) (uint64, error) {
	return e.store.CreateAnswer(context.Background(), NewAnswer{
		AuthorID:           authorID,
		TopicID:            topicID,
		BodyMD:             "一段回答。",
		RequestedAnonymous: anonymous,
		MaxAttachments:     9,
	})
}

func (e *topicEnv) authorOf(contentID uint64) authorResponse {
	e.t.Helper()
	view, err := e.store.AnswerByContentID(context.Background(), contentID)
	if err != nil {
		e.t.Fatal(err)
	}
	return toAnswerResponse(view).Author
}

func TestCreateAnswerRules(t *testing.T) {
	env := newTopicEnv(t)

	t.Run("讨论帖不能回答", func(t *testing.T) {
		discussion, err := env.store.CreateTopic(context.Background(), env.base())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := env.answer(discussion, env.authorID, false); !errors.Is(err, ErrNotAQuestion) {
			t.Fatalf("want ErrNotAQuestion, got %v", err)
		}
	})

	t.Run("锁定主题不能回答", func(t *testing.T) {
		question := env.newQuestion(false)
		env.setTopicFlag(question, "is_locked", true)
		if _, err := env.answer(question, env.authorID, false); !errors.Is(err, ErrTopicLocked) {
			t.Fatalf("want ErrTopicLocked, got %v", err)
		}
		env.setTopicFlag(question, "is_locked", false)
		if _, err := env.answer(question, env.authorID, false); err != nil {
			t.Fatalf("unlocking must restore answering: %v", err)
		}
	})

	t.Run("已解决问题对所有人禁止新增回答", func(t *testing.T) {
		question := env.newQuestion(false)
		env.setSolved(question)
		if _, err := env.answer(question, env.authorID, false); !errors.Is(err, ErrTopicSolved) {
			t.Fatalf("the asker must also be refused, got %v", err)
		}
		// An administrator is not exempt either: the rule is about the question's state, not the role.
		admin := env.createUser("admin2")
		if _, err := env.db.Exec(`UPDATE users SET role='admin' WHERE id=?`, admin); err != nil {
			t.Fatal(err)
		}
		if _, err := env.answer(question, admin, false); !errors.Is(err, ErrTopicSolved) {
			t.Fatalf("an administrator must also be refused, got %v", err)
		}
		// Reopening restores new interaction.
		env.reopen(question)
		if _, err := env.answer(question, env.authorID, false); err != nil {
			t.Fatalf("reopening must restore answering: %v", err)
		}
	})

	t.Run("已删除问题按不存在处理", func(t *testing.T) {
		question := env.newQuestion(false)
		if _, err := env.db.Exec(`UPDATE contents SET deleted_at=UTC_TIMESTAMP(6),body_md=NULL WHERE id=?`, question); err != nil {
			t.Fatal(err)
		}
		if _, err := env.answer(question, env.authorID, false); !errors.Is(err, ErrTopicNotFound) {
			t.Fatalf("want ErrTopicNotFound, got %v", err)
		}
	})

	t.Run("回答写入answers行并推进活跃时间", func(t *testing.T) {
		question := env.newQuestion(false)
		var before string
		if err := env.db.QueryRow(`SELECT last_activity_at FROM topics WHERE content_id=?`, question).Scan(&before); err != nil {
			t.Fatal(err)
		}
		id, err := env.answer(question, env.authorID, false)
		if err != nil {
			t.Fatal(err)
		}
		var questionID uint64
		if err := env.db.QueryRow(`SELECT question_id FROM answers WHERE content_id=?`, id).Scan(&questionID); err != nil {
			t.Fatal(err)
		}
		if questionID != question {
			t.Fatalf("answers.question_id must be the topic id: want %d got %d", question, questionID)
		}
		var after string
		if err := env.db.QueryRow(`SELECT last_activity_at FROM topics WHERE content_id=?`, question).Scan(&after); err != nil {
			t.Fatal(err)
		}
		if after <= before {
			t.Fatalf("last_activity_at was not advanced: before=%s after=%s", before, after)
		}
		var kind string
		if err := env.db.QueryRow(`SELECT kind FROM contents WHERE id=?`, id).Scan(&kind); err != nil {
			t.Fatal(err)
		}
		if kind != "answer" {
			t.Fatalf("contents.kind=%s want answer", kind)
		}
	})

	t.Run("禁言账号不能回答", func(t *testing.T) {
		question := env.newQuestion(false)
		muted := env.createUser("muted2")
		if _, err := env.db.Exec(`INSERT INTO user_restrictions (user_id,kind,reason,starts_at,created_by)
 VALUES (?,'mute','测试',UTC_TIMESTAMP(6),?)`, muted, env.authorID); err != nil {
			t.Fatal(err)
		}
		if _, err := env.answer(question, muted, false); !errors.Is(err, ErrAccountMuted) {
			t.Fatalf("want ErrAccountMuted, got %v", err)
		}
	})
}

func TestAnonymousAnswerIdentity(t *testing.T) {
	env := newTopicEnv(t)

	t.Run("匿名楼主自答被强制匿名且沿用同一编号", func(t *testing.T) {
		question := env.newQuestion(true)
		// The client asks to be named. It must be ignored: otherwise a self-answer would expose the
		// anonymous asker.
		answerID, err := env.answer(question, env.authorID, false)
		if err != nil {
			t.Fatal(err)
		}
		var isAnonymous bool
		if err := env.db.QueryRow(`SELECT is_anonymous FROM contents WHERE id=?`, answerID).Scan(&isAnonymous); err != nil {
			t.Fatal(err)
		}
		if !isAnonymous {
			t.Fatal("the anonymous asker's own answer was stored as named")
		}
		author := env.authorOf(answerID)
		if author.Mode != "anonymous" || author.DisplayName != "楼主" || !author.IsTopicOwner {
			t.Fatalf("forced anonymous answer rendered wrong: %+v", author)
		}
		// The topic author already holds number 1 from the topic itself, and must keep it.
		if author.DisplayNumber != 1 {
			t.Fatalf("the topic author must reuse number 1, got %d", author.DisplayNumber)
		}
		var mappings int
		if err := env.db.QueryRow(`SELECT COUNT(*) FROM anonymous_identities WHERE topic_id=?`, question).Scan(&mappings); err != nil {
			t.Fatal(err)
		}
		if mappings != 1 {
			t.Fatalf("a second mapping was created for the same user and discussion: %d", mappings)
		}
	})

	t.Run("其他参与者匿名编号稳定且不显示楼主", func(t *testing.T) {
		question := env.newQuestion(false)
		other := env.createUser("other")
		first, err := env.answer(question, other, true)
		if err != nil {
			t.Fatal(err)
		}
		second, err := env.answer(question, other, true)
		if err != nil {
			t.Fatal(err)
		}
		authorA := env.authorOf(first)
		authorB := env.authorOf(second)
		if authorA.Mode != "anonymous" || authorA.IsTopicOwner {
			t.Fatalf("another participant is not the owner: %+v", authorA)
		}
		if authorA.DisplayName != "匿名#1" {
			t.Fatalf("first anonymous participant should be 匿名#1, got %q", authorA.DisplayName)
		}
		if authorB.DisplayNumber != authorA.DisplayNumber {
			t.Fatalf("the same user must keep a stable number: %d then %d", authorA.DisplayNumber, authorB.DisplayNumber)
		}
		// And no real identity may appear in the payload.
		encoded, err := json.Marshal(authorA)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), "public_id") || strings.Contains(string(encoded), "@example.test") {
			t.Fatalf("anonymous author leaked identity: %s", encoded)
		}
	})

	t.Run("不同参与者各自取得新编号", func(t *testing.T) {
		question := env.newQuestion(false)
		one := env.createUser("one")
		two := env.createUser("two")
		first, err := env.answer(question, one, true)
		if err != nil {
			t.Fatal(err)
		}
		second, err := env.answer(question, two, true)
		if err != nil {
			t.Fatal(err)
		}
		if env.authorOf(first).DisplayNumber != 1 || env.authorOf(second).DisplayNumber != 2 {
			t.Fatalf("numbers must be distinct and dense: %d and %d",
				env.authorOf(first).DisplayNumber, env.authorOf(second).DisplayNumber)
		}
	})

	t.Run("编号不跨讨论复用", func(t *testing.T) {
		one := env.newQuestion(false)
		two := env.newQuestion(false)
		other := env.createUser("crosser")
		a, err := env.answer(one, other, true)
		if err != nil {
			t.Fatal(err)
		}
		b, err := env.answer(two, other, true)
		if err != nil {
			t.Fatal(err)
		}
		// Each discussion numbers independently, so both are the first participant there.
		if env.authorOf(a).DisplayNumber != 1 || env.authorOf(b).DisplayNumber != 1 {
			t.Fatalf("numbering must be per discussion: %d and %d",
				env.authorOf(a).DisplayNumber, env.authorOf(b).DisplayNumber)
		}
	})

	t.Run("非匿名主题作者以昵称回答时带楼主标识", func(t *testing.T) {
		question := env.newQuestion(false)
		answerID, err := env.answer(question, env.authorID, false)
		if err != nil {
			t.Fatal(err)
		}
		author := env.authorOf(answerID)
		if author.Mode != "named" || !author.IsTopicOwner {
			t.Fatalf("a named owner reply must be marked as the topic owner: %+v", author)
		}
		if author.User == nil || author.User.Nickname == "" {
			t.Fatalf("a named reply must carry the user: %+v", author)
		}
		// Another person's named answer must not claim the owner marker.
		other := env.createUser("plain")
		otherAnswer, err := env.answer(question, other, false)
		if err != nil {
			t.Fatal(err)
		}
		if env.authorOf(otherAnswer).IsTopicOwner {
			t.Fatal("a non-owner named answer claimed the topic owner marker")
		}
	})
}

func TestCreateAnswerEndpoint(t *testing.T) {
	env := newTopicEnv(t)
	question := env.newQuestion(true)
	payload := `{"topic_id":"` + itoa(question) + `","body_md":"通过接口回答。","is_anonymous":false}`

	t.Run("未登录被拒", func(t *testing.T) {
		res := env.do("POST", "/api/v1/answers", payload, "", "")
		expectStatus(t, res, 401)
		expectCode(t, res, "UNAUTHENTICATED")
	})

	t.Run("缺少CSRF被拒", func(t *testing.T) {
		cookie, _ := env.sessionCookie(env.authorID)
		res := env.do("POST", "/api/v1/answers", payload, cookie, "")
		expectStatus(t, res, 403)
		expectCode(t, res, "CSRF_REJECTED")
	})

	t.Run("正常回答返回201与契约形状", func(t *testing.T) {
		cookie, csrf := env.sessionCookie(env.authorID)
		res := env.do("POST", "/api/v1/answers", payload, cookie, csrf)
		expectStatus(t, res, 201)
		var body struct {
			ID          string `json:"id"`
			State       string `json:"state"`
			Kind        string `json:"kind"`
			TopicID     string `json:"topic_id"`
			BodyMD      string `json:"body_md"`
			CreatedAt   string `json:"created_at"`
			Attachments []any  `json:"attachments"`
			Author      struct {
				Mode         string `json:"mode"`
				DisplayName  string `json:"display_name"`
				IsTopicOwner bool   `json:"is_topic_owner"`
				User         *struct {
					Nickname string `json:"nickname"`
				} `json:"user"`
			} `json:"author"`
		}
		if err := json.Unmarshal(res.body, &body); err != nil {
			t.Fatalf("cannot decode %s: %v", res.body, err)
		}
		if body.State != "published" || body.Kind != "answer" || body.TopicID != itoa(question) {
			t.Fatalf("wrong state/kind/topic: %s/%s/%s", body.State, body.Kind, body.TopicID)
		}
		// The payload asked for a named answer in an anonymous discussion: the server must override it.
		if body.Author.Mode != "anonymous" || body.Author.DisplayName != "楼主" {
			t.Fatalf("the anonymous owner's answer must be forced anonymous: %+v", body.Author)
		}
		if body.Author.User != nil {
			t.Fatal("the anonymous branch leaked a user object")
		}
		if strings.Contains(string(res.body), "admin@example.test") || strings.Contains(string(res.body), "public_id") {
			t.Fatalf("response leaked identity: %s", res.body)
		}
		if body.Attachments == nil {
			t.Fatal("attachments must serialise as [] rather than null")
		}
	})

	t.Run("已解决问题返回409", func(t *testing.T) {
		cookie, csrf := env.sessionCookie(env.authorID)
		solved := env.newQuestion(false)
		env.setSolved(solved)
		body := `{"topic_id":"` + itoa(solved) + `","body_md":"回答。","is_anonymous":false}`
		res := env.do("POST", "/api/v1/answers", body, cookie, csrf)
		expectStatus(t, res, 409)
		expectCode(t, res, "TOPIC_SOLVED")
	})

	t.Run("讨论帖返回409", func(t *testing.T) {
		cookie, csrf := env.sessionCookie(env.authorID)
		discussion, err := env.store.CreateTopic(context.Background(), env.base())
		if err != nil {
			t.Fatal(err)
		}
		body := `{"topic_id":"` + itoa(discussion) + `","body_md":"回答。","is_anonymous":false}`
		res := env.do("POST", "/api/v1/answers", body, cookie, csrf)
		expectStatus(t, res, 409)
		expectCode(t, res, "NOT_A_QUESTION")
	})

	t.Run("危险正文与坏ID被拒", func(t *testing.T) {
		cookie, csrf := env.sessionCookie(env.authorID)
		body := `{"topic_id":"` + itoa(question) + `","body_md":"<script>alert(1)</script>","is_anonymous":false}`
		res := env.do("POST", "/api/v1/answers", body, cookie, csrf)
		expectStatus(t, res, 422)
		expectCode(t, res, "BODY_RAW_HTML")
		body = `{"topic_id":"0","body_md":"回答。","is_anonymous":false}`
		res = env.do("POST", "/api/v1/answers", body, cookie, csrf)
		expectStatus(t, res, 422)
		expectCode(t, res, "INVALID_REQUEST")
	})
}
