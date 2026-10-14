package content

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func (e *topicEnv) resolution(topicID uint64) (status string, resolvedAt *string) {
	e.t.Helper()
	var resolved sql.NullString
	if err := e.db.QueryRow(`SELECT resolution_status,resolved_at FROM questions WHERE topic_id=?`, topicID).
		Scan(&status, &resolved); err != nil {
		e.t.Fatal(err)
	}
	if resolved.Valid {
		value := resolved.String
		return status, &value
	}
	return status, nil
}

func TestSetResolution(t *testing.T) {
	env := newTopicEnv(t)
	ctx := context.Background()

	t.Run("提问者标记已解决并写入时间", func(t *testing.T) {
		question := env.newQuestion(false)
		if err := env.store.SetResolution(ctx, SetResolutionInput{ActorID: env.authorID, TopicID: question, Status: "solved"}); err != nil {
			t.Fatal(err)
		}
		status, resolvedAt := env.resolution(question)
		if status != "solved" {
			t.Fatalf("status=%s want solved", status)
		}
		// The CHECK constraint pairs solved with a non-null time, so both must be written together.
		if resolvedAt == nil {
			t.Fatal("solved must set resolved_at")
		}
	})

	t.Run("已解决后新增互动被拒且重新打开后恢复", func(t *testing.T) {
		question := env.newQuestion(false)
		answerID, err := env.answer(question, env.authorID, false)
		if err != nil {
			t.Fatal(err)
		}
		if err := env.store.SetResolution(ctx, SetResolutionInput{ActorID: env.authorID, TopicID: question, Status: "solved"}); err != nil {
			t.Fatal(err)
		}
		// This is the cross-operation guarantee: the switch must actually stop the writers, not just
		// change a column.
		if _, err := env.answer(question, env.authorID, false); !errors.Is(err, ErrTopicSolved) {
			t.Fatalf("answering a solved question: want ErrTopicSolved, got %v", err)
		}
		if _, err := env.comment(NewComment{AuthorID: env.authorID, TopicID: question}); !errors.Is(err, ErrTopicSolved) {
			t.Fatalf("commenting on a solved question: want ErrTopicSolved, got %v", err)
		}
		if _, err := env.comment(NewComment{AuthorID: env.authorID, TopicID: question, AnswerID: answerID}); !errors.Is(err, ErrTopicSolved) {
			t.Fatalf("commenting on an answer of a solved question: want ErrTopicSolved, got %v", err)
		}
		if err := env.store.SetResolution(ctx, SetResolutionInput{ActorID: env.authorID, TopicID: question, Status: "open"}); err != nil {
			t.Fatal(err)
		}
		status, resolvedAt := env.resolution(question)
		if status != "open" {
			t.Fatalf("status=%s want open", status)
		}
		// open must clear the time; leaving it would violate the CHECK constraint.
		if resolvedAt != nil {
			t.Fatalf("reopening must clear resolved_at, got %v", *resolvedAt)
		}
		if _, err := env.answer(question, env.authorID, false); err != nil {
			t.Fatalf("reopening must restore answering: %v", err)
		}
	})

	t.Run("重复设置同一状态是幂等的", func(t *testing.T) {
		question := env.newQuestion(false)
		for i := 0; i < 2; i++ {
			if err := env.store.SetResolution(ctx, SetResolutionInput{ActorID: env.authorID, TopicID: question, Status: "solved"}); err != nil {
				t.Fatalf("repeat %d: %v", i, err)
			}
		}
		if status, _ := env.resolution(question); status != "solved" {
			t.Fatalf("status=%s want solved", status)
		}
	})

	t.Run("只有提问者可以切换", func(t *testing.T) {
		question := env.newQuestion(false)
		other := env.createUser("nottheasker")
		if err := env.store.SetResolution(ctx, SetResolutionInput{ActorID: other, TopicID: question, Status: "solved"}); !errors.Is(err, ErrNotTopicOwner) {
			t.Fatalf("another user: want ErrNotTopicOwner, got %v", err)
		}
		// An administrator is not exempt: this is the asker's judgement, not a moderation action.
		admin := env.createUser("resadmin")
		if _, err := env.db.Exec(`UPDATE users SET role='admin' WHERE id=?`, admin); err != nil {
			t.Fatal(err)
		}
		if err := env.store.SetResolution(ctx, SetResolutionInput{ActorID: admin, TopicID: question, Status: "solved"}); !errors.Is(err, ErrNotTopicOwner) {
			t.Fatalf("an administrator: want ErrNotTopicOwner, got %v", err)
		}
		if status, _ := env.resolution(question); status != "open" {
			t.Fatalf("a refused switch must not change the state, got %s", status)
		}
	})

	t.Run("讨论帖不能设置解决状态", func(t *testing.T) {
		discussion, err := env.store.CreateTopic(ctx, env.base())
		if err != nil {
			t.Fatal(err)
		}
		if err := env.store.SetResolution(ctx, SetResolutionInput{ActorID: env.authorID, TopicID: discussion, Status: "solved"}); !errors.Is(err, ErrNotAQuestion) {
			t.Fatalf("want ErrNotAQuestion, got %v", err)
		}
	})

	t.Run("已删除问题按不存在处理", func(t *testing.T) {
		question := env.newQuestion(false)
		if _, err := env.db.Exec(`UPDATE contents SET deleted_at=UTC_TIMESTAMP(6),body_md=NULL WHERE id=?`, question); err != nil {
			t.Fatal(err)
		}
		if err := env.store.SetResolution(ctx, SetResolutionInput{ActorID: env.authorID, TopicID: question, Status: "solved"}); !errors.Is(err, ErrTopicNotFound) {
			t.Fatalf("want ErrTopicNotFound, got %v", err)
		}
	})

	t.Run("锁定不能阻止重新打开", func(t *testing.T) {
		question := env.newQuestion(false)
		if err := env.store.SetResolution(ctx, SetResolutionInput{ActorID: env.authorID, TopicID: question, Status: "solved"}); err != nil {
			t.Fatal(err)
		}
		// Locking stops new interaction; switching the state is not interaction, and treating it as such
		// would make a locked-and-solved question impossible to reopen.
		env.setTopicFlag(question, "is_locked", true)
		if err := env.store.SetResolution(ctx, SetResolutionInput{ActorID: env.authorID, TopicID: question, Status: "open"}); err != nil {
			t.Fatalf("a locked question must still be reopenable: %v", err)
		}
	})

	t.Run("非法取值被拒", func(t *testing.T) {
		question := env.newQuestion(false)
		if err := env.store.SetResolution(ctx, SetResolutionInput{ActorID: env.authorID, TopicID: question, Status: "maybe"}); !errors.Is(err, ErrResolutionStatus) {
			t.Fatalf("want ErrResolutionStatus, got %v", err)
		}
	})
}

func TestSetResolutionEndpoint(t *testing.T) {
	env := newTopicEnv(t)
	question := env.newQuestion(false)
	path := "/api/v1/questions/" + itoa(question) + "/resolution"

	t.Run("未登录被拒", func(t *testing.T) {
		res := env.do("PATCH", path, `{"resolution_status":"solved"}`, "", "")
		expectStatus(t, res, 401)
	})

	t.Run("缺少CSRF被拒", func(t *testing.T) {
		cookie, _ := env.sessionCookie(env.authorID)
		res := env.do("PATCH", path, `{"resolution_status":"solved"}`, cookie, "")
		expectStatus(t, res, 403)
		expectCode(t, res, "CSRF_REJECTED")
	})

	t.Run("提问者切换成功并返回整个问题", func(t *testing.T) {
		cookie, csrf := env.sessionCookie(env.authorID)
		res := env.do("PATCH", path, `{"resolution_status":"solved"}`, cookie, csrf)
		expectStatus(t, res, 200)
		var body struct {
			ID               string  `json:"id"`
			Kind             string  `json:"kind"`
			ResolutionStatus *string `json:"resolution_status"`
			ResolvedAt       *string `json:"resolved_at"`
		}
		if err := json.Unmarshal(res.body, &body); err != nil {
			t.Fatalf("cannot decode %s: %v", res.body, err)
		}
		if body.Kind != "question" || body.ID != itoa(question) {
			t.Fatalf("wrong kind/id: %s/%s", body.Kind, body.ID)
		}
		if body.ResolutionStatus == nil || *body.ResolutionStatus != "solved" {
			t.Fatalf("resolution_status wrong: %v", body.ResolutionStatus)
		}
		if body.ResolvedAt == nil {
			t.Fatal("a solved question must report resolved_at")
		}
		// Reopen through the endpoint and confirm the field disappears again. A fresh struct is required:
		// json.Unmarshal leaves fields that are absent from the payload untouched, so reusing one would
		// report the previous value and the assertion would pass or fail for the wrong reason.
		res = env.do("PATCH", path, `{"resolution_status":"open"}`, cookie, csrf)
		expectStatus(t, res, 200)
		var reopened struct {
			ResolutionStatus *string `json:"resolution_status"`
			ResolvedAt       *string `json:"resolved_at"`
		}
		if err := json.Unmarshal(res.body, &reopened); err != nil {
			t.Fatal(err)
		}
		if reopened.ResolvedAt != nil {
			t.Fatalf("an open question must omit resolved_at: %s", res.body)
		}
		if reopened.ResolutionStatus == nil || *reopened.ResolutionStatus != "open" {
			t.Fatalf("resolution_status should be open: %s", res.body)
		}
	})

	t.Run("非提问者被拒", func(t *testing.T) {
		other := env.createUser("endpointother")
		cookie, csrf := env.sessionCookie(other)
		res := env.do("PATCH", path, `{"resolution_status":"solved"}`, cookie, csrf)
		expectStatus(t, res, 403)
		expectCode(t, res, "NOT_TOPIC_OWNER")
	})

	t.Run("非法取值与坏路径ID被拒", func(t *testing.T) {
		cookie, csrf := env.sessionCookie(env.authorID)
		res := env.do("PATCH", path, `{"resolution_status":"maybe"}`, cookie, csrf)
		expectStatus(t, res, 422)
		expectCode(t, res, "INVALID_REQUEST")
		res = env.do("PATCH", "/api/v1/questions/abc/resolution", `{"resolution_status":"open"}`, cookie, csrf)
		expectStatus(t, res, 422)
		expectCode(t, res, "INVALID_REQUEST")
	})

	t.Run("讨论帖返回409", func(t *testing.T) {
		discussion, err := env.store.CreateTopic(context.Background(), env.base())
		if err != nil {
			t.Fatal(err)
		}
		cookie, csrf := env.sessionCookie(env.authorID)
		res := env.do("PATCH", "/api/v1/questions/"+itoa(discussion)+"/resolution", `{"resolution_status":"solved"}`, cookie, csrf)
		expectStatus(t, res, 409)
		expectCode(t, res, "NOT_A_QUESTION")
	})

	t.Run("未知字段被拒", func(t *testing.T) {
		cookie, csrf := env.sessionCookie(env.authorID)
		res := env.do("PATCH", path, `{"resolution_status":"open","extra":1}`, cookie, csrf)
		expectStatus(t, res, 400)
		expectCode(t, res, "INVALID_JSON")
	})

	t.Run("响应不含任何身份泄漏", func(t *testing.T) {
		cookie, csrf := env.sessionCookie(env.authorID)
		res := env.do("PATCH", path, `{"resolution_status":"open"}`, cookie, csrf)
		if strings.Contains(string(res.body), "@example.test") || strings.Contains(string(res.body), "password") {
			t.Fatalf("response leaked identity: %s", res.body)
		}
	})
}
