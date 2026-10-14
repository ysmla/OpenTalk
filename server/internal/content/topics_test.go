package content

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"opentalk/server/internal/auth"
	"opentalk/server/internal/media"
	"opentalk/server/internal/platform/httpapi"
	"opentalk/server/internal/platform/secretbox"
	"opentalk/server/internal/platform/testdb"
	"opentalk/server/internal/settings"
)

// topicEnv is one throwaway schema plus the fixtures a topic test needs. The write path is exercised
// against real MySQL because the transaction, the composite foreign key to anonymous_identities and the
// unique keys are the parts most likely to be wrong, and none of them exist in a fake.
type topicEnv struct {
	t         *testing.T
	db        *sql.DB
	store     Store
	handler   http.Handler
	sessions  auth.Sessions
	boardID   uint64
	qaBoardID uint64
	tagID     uint64
	authorID  uint64
	seq       int
	mediaRoot string
}

func newTopicEnv(t *testing.T) *topicEnv {
	t.Helper()
	dsn := testdb.DSNFromEnv()
	if dsn == "" {
		t.Skip("set OPENTALK_TEST_MYSQL_DSN to run content integration checks")
	}
	db, cleanup, err := testdb.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)

	secret := []byte(strings.Repeat("k", 32))
	csrfKey, err := secretbox.Derive(secret, "session-csrf")
	if err != nil {
		t.Fatal(err)
	}
	settingsKey, err := secretbox.Derive(secret, "settings-secret")
	if err != nil {
		t.Fatal(err)
	}

	store := Store{DB: db}
	if _, err := store.SeedBoards(context.Background()); err != nil {
		t.Fatal(err)
	}
	env := &topicEnv{t: t, db: db, store: store, sessions: auth.Sessions{DB: db, CSRFKey: csrfKey}}
	env.boardID = env.boardIDOfKind("discussion")
	env.qaBoardID = env.boardIDOfKind("qa")
	env.authorID = env.createUser("author")
	env.tagID = env.createTag("公告")

	mux := http.NewServeMux()
	env.mediaRoot = t.TempDir()
	Register(mux, Deps{
		Store:    store,
		Logger:   slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Sessions: env.sessions,
		Settings: &settings.Store{DB: db, Key: settingsKey},
		Media:    &media.Store{Root: env.mediaRoot},
	})
	env.handler = httpapi.Middleware(testOrigin, slog.New(slog.NewJSONHandler(io.Discard, nil)), mux)
	return env
}

func (e *topicEnv) boardIDOfKind(kind string) uint64 {
	e.t.Helper()
	var id uint64
	if err := e.db.QueryRow(`SELECT id FROM boards WHERE kind=? ORDER BY id LIMIT 1`, kind).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *topicEnv) createUser(nickname string) uint64 {
	e.t.Helper()
	result, err := e.db.Exec(`INSERT INTO users (public_id,email_normalized,password_hash,nickname,bio)
 VALUES (UUID(),?,?,?,'')`, nickname+"@example.test", "$argon2id$v=19$m=65536,t=3,p=2$c2FsdHNhbHQ$hash", nickname)
	if err != nil {
		e.t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		e.t.Fatal(err)
	}
	return uint64(id)
}

// newUser creates an extra participant whose email is unique per environment. A fixed nickname cannot be
// reused within one schema because email_normalized is unique, and two tests in the same environment may
// both want "another account".
func (e *topicEnv) newUser(nickname string) uint64 {
	e.t.Helper()
	e.seq++
	return e.createUser(fmt.Sprintf("%s%d", nickname, e.seq))
}

func (e *topicEnv) createTag(name string) uint64 {
	e.t.Helper()
	result, err := e.db.Exec(`INSERT INTO tags (name,normalized_name,description) VALUES (?,?,?)`, name, name, name+"说明")
	if err != nil {
		e.t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		e.t.Fatal(err)
	}
	return uint64(id)
}

func (e *topicEnv) createAttachment(ownerID uint64, status string) uint64 {
	e.t.Helper()
	// storage_key is unique, so every fixture needs its own value.
	e.seq++
	result, err := e.db.Exec(`INSERT INTO attachments (owner_id,storage_key,mime_type,byte_size,status)
 VALUES (?,?,?,?,?)`, ownerID, fmt.Sprintf("key-%s-%d", status, e.seq), "image/png", 1024, status)
	if err != nil {
		e.t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		e.t.Fatal(err)
	}
	return uint64(id)
}

// base returns the smallest valid create request; individual tests adjust one field so a failure points
// at that field rather than at unrelated setup.
func (e *topicEnv) base() NewTopic {
	return NewTopic{
		AuthorID:       e.authorID,
		BoardID:        e.boardID,
		Title:          "第一个帖子",
		BodyMD:         "正文内容。",
		TagIDs:         []uint64{e.tagID},
		MaxAttachments: 9,
	}
}

func TestCreateTopicStore(t *testing.T) {
	env := newTopicEnv(t)
	ctx := context.Background()

	t.Run("讨论帖写入全部相关行", func(t *testing.T) {
		id, err := env.store.CreateTopic(ctx, env.base())
		if err != nil {
			t.Fatal(err)
		}
		var kind, isAnonymous string
		var anonymousID sql.NullInt64
		if err := env.db.QueryRow(`SELECT kind,is_anonymous,anonymous_identity_id FROM contents WHERE id=?`, id).
			Scan(&kind, &isAnonymous, &anonymousID); err != nil {
			t.Fatal(err)
		}
		if kind != "topic" || isAnonymous != "0" || anonymousID.Valid {
			t.Fatalf("contents row wrong: kind=%s anonymous=%s anon_id=%v", kind, isAnonymous, anonymousID)
		}
		// A non-anonymous topic must not create a mapping.
		var mappings int
		if err := env.db.QueryRow(`SELECT COUNT(*) FROM anonymous_identities WHERE topic_id=?`, id).Scan(&mappings); err != nil {
			t.Fatal(err)
		}
		if mappings != 0 {
			t.Fatalf("non-anonymous topic created %d anonymous mappings", mappings)
		}
		// A discussion board must not create a question row.
		var questions int
		if err := env.db.QueryRow(`SELECT COUNT(*) FROM questions WHERE topic_id=?`, id).Scan(&questions); err != nil {
			t.Fatal(err)
		}
		if questions != 0 {
			t.Fatalf("discussion topic created %d question rows", questions)
		}
		var lastActivity sql.NullTime
		if err := env.db.QueryRow(`SELECT last_activity_at FROM topics WHERE content_id=?`, id).Scan(&lastActivity); err != nil {
			t.Fatal(err)
		}
		if !lastActivity.Valid {
			t.Fatal("last_activity_at was not set")
		}
		var tags int
		if err := env.db.QueryRow(`SELECT COUNT(*) FROM topic_tags WHERE topic_id=?`, id).Scan(&tags); err != nil {
			t.Fatal(err)
		}
		if tags != 1 {
			t.Fatalf("expected 1 tag link, got %d", tags)
		}
	})

	t.Run("问答板块同时建问题行", func(t *testing.T) {
		in := env.base()
		in.BoardID = env.qaBoardID
		id, err := env.store.CreateTopic(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		var status string
		var resolved sql.NullTime
		if err := env.db.QueryRow(`SELECT resolution_status,resolved_at FROM questions WHERE topic_id=?`, id).
			Scan(&status, &resolved); err != nil {
			t.Fatal(err)
		}
		if status != "open" || resolved.Valid {
			t.Fatalf("new question should be open and unresolved: status=%s resolved=%v", status, resolved)
		}
	})

	t.Run("匿名帖写入映射并回填", func(t *testing.T) {
		in := env.base()
		in.IsAnonymous = true
		id, err := env.store.CreateTopic(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		var number int
		var identityID, contentsIdentity uint64
		if err := env.db.QueryRow(`SELECT id,display_number FROM anonymous_identities WHERE topic_id=?`, id).
			Scan(&identityID, &number); err != nil {
			t.Fatal(err)
		}
		// The first anonymous participant in a new discussion is number 1.
		if number != 1 {
			t.Fatalf("first anonymous identity should be number 1, got %d", number)
		}
		if err := env.db.QueryRow(`SELECT anonymous_identity_id FROM contents WHERE id=?`, id).
			Scan(&contentsIdentity); err != nil {
			t.Fatal(err)
		}
		if contentsIdentity != identityID {
			t.Fatalf("contents was not backfilled: want %d got %d", identityID, contentsIdentity)
		}
		view, err := env.store.TopicByContentID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		response := toTopicResponse(view)
		if response.Author.Mode != "anonymous" || response.Author.DisplayName != "楼主" {
			t.Fatalf("anonymous author rendered wrong: %+v", response.Author)
		}
		// The anonymous branch must not carry any real identity.
		if response.Author.User != nil {
			t.Fatal("anonymous author leaked a user object")
		}
		encoded, err := json.Marshal(response)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"author", "example.test", "public_id"} {
			if forbidden == "author" {
				continue
			}
			if bytes.Contains(encoded, []byte(forbidden)) {
				t.Fatalf("anonymous response leaked %q: %s", forbidden, encoded)
			}
		}
	})

	t.Run("归档板块拒绝新主题", func(t *testing.T) {
		if _, err := env.db.Exec(`UPDATE boards SET is_archived=TRUE WHERE id=?`, env.boardID); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = env.db.Exec(`UPDATE boards SET is_archived=FALSE WHERE id=?`, env.boardID) })
		if _, err := env.store.CreateTopic(ctx, env.base()); !errors.Is(err, ErrBoardArchived) {
			t.Fatalf("want ErrBoardArchived, got %v", err)
		}
	})

	t.Run("未知板块", func(t *testing.T) {
		in := env.base()
		in.BoardID = 999999
		if _, err := env.store.CreateTopic(ctx, in); !errors.Is(err, ErrBoardNotFound) {
			t.Fatalf("want ErrBoardNotFound, got %v", err)
		}
	})

	t.Run("标签必须存在且不超过上限", func(t *testing.T) {
		in := env.base()
		in.TagIDs = []uint64{999999}
		if _, err := env.store.CreateTopic(ctx, in); !errors.Is(err, ErrTagNotFound) {
			t.Fatalf("want ErrTagNotFound, got %v", err)
		}
		in = env.base()
		in.TagIDs = []uint64{env.tagID, env.tagID, env.tagID, env.tagID, env.tagID, env.tagID}
		if _, err := env.store.CreateTopic(ctx, in); !errors.Is(err, ErrTooManyTags) {
			t.Fatalf("want ErrTooManyTags, got %v", err)
		}
	})

	t.Run("重复ID不会破坏唯一键", func(t *testing.T) {
		// The same tag twice must be deduplicated rather than violating the topic_tags primary key.
		in := env.base()
		in.TagIDs = []uint64{env.tagID, env.tagID, env.tagID}
		id, err := env.store.CreateTopic(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		var links int
		if err := env.db.QueryRow(`SELECT COUNT(*) FROM topic_tags WHERE topic_id=?`, id).Scan(&links); err != nil {
			t.Fatal(err)
		}
		if links != 1 {
			t.Fatalf("expected the duplicate tag to collapse to one link, got %d", links)
		}
	})

	t.Run("图片必须属于本人且未作废", func(t *testing.T) {
		stranger := env.createUser("stranger")
		foreign := env.createAttachment(stranger, "pending")
		in := env.base()
		in.AttachmentIDs = []uint64{foreign}
		if _, err := env.store.CreateTopic(ctx, in); !errors.Is(err, ErrAttachmentUnavailable) {
			t.Fatalf("someone else's attachment must be refused, got %v", err)
		}
		deleted := env.createAttachment(env.authorID, "deleted")
		in = env.base()
		in.AttachmentIDs = []uint64{deleted}
		if _, err := env.store.CreateTopic(ctx, in); !errors.Is(err, ErrAttachmentUnavailable) {
			t.Fatalf("a discarded attachment must be refused, got %v", err)
		}
		// A pending attachment owned by the author is accepted, because upload before publish is the
		// normal order.
		own := env.createAttachment(env.authorID, "pending")
		in = env.base()
		in.AttachmentIDs = []uint64{own}
		id, err := env.store.CreateTopic(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		var links int
		if err := env.db.QueryRow(`SELECT COUNT(*) FROM content_attachments WHERE content_id=?`, id).Scan(&links); err != nil {
			t.Fatal(err)
		}
		if links != 1 {
			t.Fatalf("expected 1 image link, got %d", links)
		}
	})

	t.Run("超过配置的图片张数上限", func(t *testing.T) {
		in := env.base()
		in.MaxAttachments = 2
		for i := 0; i < 3; i++ {
			in.AttachmentIDs = append(in.AttachmentIDs, env.createAttachment(env.authorID, "pending"))
		}
		if _, err := env.store.CreateTopic(ctx, in); !errors.Is(err, ErrTooManyAttachments) {
			t.Fatalf("want ErrTooManyAttachments, got %v", err)
		}
	})

	t.Run("禁言账号不能发帖", func(t *testing.T) {
		muted := env.createUser("muted")
		admin := env.createUser("admin")
		if _, err := env.db.Exec(`INSERT INTO user_restrictions (user_id,kind,reason,starts_at,created_by)
 VALUES (?,'mute','测试禁言',UTC_TIMESTAMP(6),?)`, muted, admin); err != nil {
			t.Fatal(err)
		}
		in := env.base()
		in.AuthorID = muted
		if _, err := env.store.CreateTopic(ctx, in); !errors.Is(err, ErrAccountMuted) {
			t.Fatalf("a muted account must be refused, got %v", err)
		}
		// An expired mute must not block. This is the half of the rule that is easy to get wrong.
		if _, err := env.db.Exec(`UPDATE user_restrictions SET starts_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 2 DAY),
 ends_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 DAY) WHERE user_id=?`, muted); err != nil {
			t.Fatal(err)
		}
		if _, err := env.store.CreateTopic(ctx, in); err != nil {
			t.Fatalf("an expired mute must not block: %v", err)
		}
	})

	t.Run("邮件覆盖只在显式设置时建行", func(t *testing.T) {
		id, err := env.store.CreateTopic(ctx, env.base())
		if err != nil {
			t.Fatal(err)
		}
		var rows int
		if err := env.db.QueryRow(`SELECT COUNT(*) FROM topic_subscriptions WHERE topic_id=?`, id).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if rows != 0 {
			t.Fatalf("inherit must not create a subscription row, got %d", rows)
		}
		in := env.base()
		in.EmailOverride = "disabled"
		id, err = env.store.CreateTopic(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		var override string
		if err := env.db.QueryRow(`SELECT email_override FROM topic_subscriptions WHERE topic_id=?`, id).Scan(&override); err != nil {
			t.Fatal(err)
		}
		if override != "disabled" {
			t.Fatalf("override not stored: %q", override)
		}
	})
}

// do issues one HTTP request against the content handler, optionally carrying a session cookie and a
// CSRF token exactly as the browser would.
func (e *topicEnv) do(method, path, body, cookie, csrf string) response {
	e.t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, path, reader)
	// Writes require a trusted Origin, so every request carries the app's own origin unless a test
	// overrides it deliberately.
	request.Header.Set("Origin", testOrigin)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if cookie != "" {
		request.Header.Set("Cookie", cookie)
	}
	if csrf != "" {
		request.Header.Set("X-CSRF-Token", csrf)
	}
	recorder := httptest.NewRecorder()
	e.handler.ServeHTTP(recorder, request)
	return response{status: recorder.Code, body: recorder.Body.Bytes()}
}

// sessionCookie creates a real session row and returns the cookie pair plus the matching CSRF token, so
// the write path is tested through the same middleware the browser goes through.
func (e *topicEnv) sessionCookie(userID uint64) (string, string) {
	e.t.Helper()
	raw, csrf, err := e.sessions.Create(context.Background(), userID)
	if err != nil {
		e.t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	e.sessions.SetCookie(recorder, raw)
	header := recorder.Header().Get("Set-Cookie")
	if header == "" {
		e.t.Fatal("SetCookie produced no header")
	}
	return strings.SplitN(header, ";", 2)[0], csrf
}

func TestCreateTopicEndpoint(t *testing.T) {
	env := newTopicEnv(t)
	valid := `{"board_id":"` + itoa(env.boardID) + `","title":"通过接口发布","body_md":"正文**加粗**。","is_anonymous":false,"tag_ids":["` + itoa(env.tagID) + `"]}`

	t.Run("未登录被拒", func(t *testing.T) {
		res := env.do("POST", "/api/v1/topics", valid, "", "")
		expectStatus(t, res, 401)
		expectCode(t, res, "UNAUTHENTICATED")
	})

	t.Run("缺少CSRF令牌被拒", func(t *testing.T) {
		cookie, _ := env.sessionCookie(env.authorID)
		res := env.do("POST", "/api/v1/topics", valid, cookie, "")
		expectStatus(t, res, 403)
		// Asserting the code matters: without it this test would also pass when the refusal came from
		// the Origin check rather than from the missing token.
		expectCode(t, res, "CSRF_REJECTED")
	})

	t.Run("越权令牌被拒", func(t *testing.T) {
		cookie, _ := env.sessionCookie(env.authorID)
		res := env.do("POST", "/api/v1/topics", valid, cookie, "not-a-real-token")
		expectStatus(t, res, 403)
		expectCode(t, res, "CSRF_REJECTED")
	})

	t.Run("正常发布返回201与契约形状", func(t *testing.T) {
		cookie, csrf := env.sessionCookie(env.authorID)
		res := env.do("POST", "/api/v1/topics", valid, cookie, csrf)
		expectStatus(t, res, 201)
		var body struct {
			ID        string `json:"id"`
			State     string `json:"state"`
			Kind      string `json:"kind"`
			Title     string `json:"title"`
			BoardID   string `json:"board_id"`
			BodyMD    string `json:"body_md"`
			CreatedAt string `json:"created_at"`
			IsLocked  bool   `json:"is_locked"`
			IsPinned  bool   `json:"is_pinned"`
			Tags      []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"tags"`
			Attachments []any `json:"attachments"`
			Author      struct {
				Mode         string `json:"mode"`
				IsTopicOwner bool   `json:"is_topic_owner"`
				User         *struct {
					PublicID string `json:"public_id"`
					Nickname string `json:"nickname"`
				} `json:"user"`
			} `json:"author"`
			// A discussion must not carry question-only fields.
			ResolutionStatus *string `json:"resolution_status"`
		}
		if err := json.Unmarshal(res.body, &body); err != nil {
			t.Fatalf("cannot decode %s: %v", res.body, err)
		}
		if body.State != "published" || body.Kind != "discussion" {
			t.Fatalf("wrong state/kind: %s/%s", body.State, body.Kind)
		}
		if body.Title != "通过接口发布" || body.BoardID != itoa(env.boardID) {
			t.Fatalf("wrong title/board: %q/%q", body.Title, body.BoardID)
		}
		if len(body.Tags) != 1 || body.Tags[0].Name != "公告" {
			t.Fatalf("tags not returned: %+v", body.Tags)
		}
		if body.Author.Mode != "named" || body.Author.User == nil || !body.Author.IsTopicOwner {
			t.Fatalf("named author wrong: %+v", body.Author)
		}
		if body.ResolutionStatus != nil {
			t.Fatalf("a discussion must not include resolution_status: %s", res.body)
		}
		if body.Attachments == nil {
			t.Fatal("attachments must serialise as [] rather than null")
		}
		if body.CreatedAt == "" {
			t.Fatal("created_at missing")
		}
	})

	t.Run("问答板块返回question", func(t *testing.T) {
		cookie, csrf := env.sessionCookie(env.authorID)
		payload := `{"board_id":"` + itoa(env.qaBoardID) + `","title":"这是个问题","body_md":"怎么用？","is_anonymous":false,"tag_ids":[]}`
		res := env.do("POST", "/api/v1/topics", payload, cookie, csrf)
		expectStatus(t, res, 201)
		var body struct {
			Kind             string  `json:"kind"`
			ResolutionStatus *string `json:"resolution_status"`
			ResolvedAt       *string `json:"resolved_at"`
		}
		if err := json.Unmarshal(res.body, &body); err != nil {
			t.Fatal(err)
		}
		if body.Kind != "question" {
			t.Fatalf("kind=%s want question", body.Kind)
		}
		if body.ResolutionStatus == nil || *body.ResolutionStatus != "open" {
			t.Fatalf("resolution_status wrong: %v", body.ResolutionStatus)
		}
		if body.ResolvedAt != nil {
			t.Fatal("an open question must omit resolved_at")
		}
	})

	t.Run("原始HTML被拒", func(t *testing.T) {
		cookie, csrf := env.sessionCookie(env.authorID)
		payload := `{"board_id":"` + itoa(env.boardID) + `","title":"危险","body_md":"正文<script>alert(1)</script>","is_anonymous":false,"tag_ids":[]}`
		res := env.do("POST", "/api/v1/topics", payload, cookie, csrf)
		expectStatus(t, res, 422)
		expectCode(t, res, "BODY_RAW_HTML")
	})

	t.Run("危险链接协议被拒", func(t *testing.T) {
		cookie, csrf := env.sessionCookie(env.authorID)
		payload := `{"board_id":"` + itoa(env.boardID) + `","title":"危险链接","body_md":"[点我](javascript:alert(1))","is_anonymous":false,"tag_ids":[]}`
		res := env.do("POST", "/api/v1/topics", payload, cookie, csrf)
		expectStatus(t, res, 422)
		expectCode(t, res, "BODY_UNSAFE_LINK")
	})

	t.Run("超长标题被拒", func(t *testing.T) {
		cookie, csrf := env.sessionCookie(env.authorID)
		payload := `{"board_id":"` + itoa(env.boardID) + `","title":"` + strings.Repeat("字", 121) + `","body_md":"正文","is_anonymous":false,"tag_ids":[]}`
		res := env.do("POST", "/api/v1/topics", payload, cookie, csrf)
		expectStatus(t, res, 422)
		expectCode(t, res, "TITLE_TOO_LONG")
	})

	t.Run("标签超限被拒", func(t *testing.T) {
		cookie, csrf := env.sessionCookie(env.authorID)
		ids := make([]string, 0, 6)
		for i := 0; i < 6; i++ {
			ids = append(ids, `"`+itoa(env.createTag("标签"+itoa(uint64(i))))+`"`)
		}
		payload := `{"board_id":"` + itoa(env.boardID) + `","title":"太多标签","body_md":"正文","is_anonymous":false,"tag_ids":[` + strings.Join(ids, ",") + `]}`
		res := env.do("POST", "/api/v1/topics", payload, cookie, csrf)
		expectStatus(t, res, 422)
		expectCode(t, res, "TOO_MANY_TAGS")
	})

	t.Run("归档板块返回409", func(t *testing.T) {
		cookie, csrf := env.sessionCookie(env.authorID)
		if _, err := env.db.Exec(`UPDATE boards SET is_archived=TRUE WHERE id=?`, env.boardID); err != nil {
			t.Fatal(err)
		}
		defer func() { _, _ = env.db.Exec(`UPDATE boards SET is_archived=FALSE WHERE id=?`, env.boardID) }()
		res := env.do("POST", "/api/v1/topics", valid, cookie, csrf)
		expectStatus(t, res, 409)
		expectCode(t, res, "BOARD_ARCHIVED")
	})

	t.Run("未知字段与坏ID被拒", func(t *testing.T) {
		cookie, csrf := env.sessionCookie(env.authorID)
		res := env.do("POST", "/api/v1/topics", `{"board_id":"1","title":"x","body_md":"y","is_anonymous":false,"tag_ids":[],"unexpected":1}`, cookie, csrf)
		expectStatus(t, res, 400)
		expectCode(t, res, "INVALID_JSON")
		res = env.do("POST", "/api/v1/topics", `{"board_id":"0","title":"x","body_md":"y","is_anonymous":false,"tag_ids":[]}`, cookie, csrf)
		expectStatus(t, res, 422)
		expectCode(t, res, "INVALID_REQUEST")
	})

	t.Run("跨站来源被拒", func(t *testing.T) {
		cookie, csrf := env.sessionCookie(env.authorID)
		request := httptest.NewRequest("POST", "/api/v1/topics", strings.NewReader(valid))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Cookie", cookie)
		request.Header.Set("X-CSRF-Token", csrf)
		request.Header.Set("Origin", "https://evil.test")
		recorder := httptest.NewRecorder()
		env.handler.ServeHTTP(recorder, request)
		if recorder.Code != 403 {
			t.Fatalf("a foreign Origin must be refused, got %d: %s", recorder.Code, recorder.Body.String())
		}
	})
}

func itoa(value uint64) string { return strconv.FormatUint(value, 10) }
