package auth

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"opentalk/server/internal/account"
	"opentalk/server/internal/captcha"
	"opentalk/server/internal/platform/httpapi"
	"opentalk/server/internal/platform/secretbox"
	"opentalk/server/internal/platform/testdb"
	"opentalk/server/internal/settings"
)

// These tests exercise the real HTTP surface against a real MySQL schema and a real Redis. They skip
// unless both endpoints are provided, matching the migration test's convention, because a passing
// unit test must never be mistaken for verified database behaviour.
const (
	testOrigin     = "http://127.0.0.1:5173"
	testPassword   = "abc12345"
	adminPassword  = "admin12345"
	redisTestDBIdx = 15
	// A wrong answer must use a character the alphabet excludes, otherwise it could accidentally be
	// the real answer.
	wrongCaptchaAnswer = "0000"
)

var codeExtractor = regexp.MustCompile(`验证码是 ([0-9]{6})`)

type testEnv struct {
	t          *testing.T
	db         *sql.DB
	cache      *redis.Client
	deps       Deps
	handler    http.Handler
	sink       *smtpSink
	settings   *settings.Store
	challenges Challenges
}

func TestAccountFlows(t *testing.T) {
	env := newTestEnv(t)
	// Serial sub-tests keep the flow readable and the shared Redis state predictable.
	t.Run("captcha", env.testCaptcha)
	t.Run("registration", env.testRegistration)
	t.Run("concurrent registration", env.testConcurrentRegistration)
	t.Run("login and session lifecycle", env.testLoginSession)
	t.Run("profile", env.testProfile)
	t.Run("password reset", env.testResetPassword)
	t.Run("rate limits and replay", env.testRateLimits)
	t.Run("admin settings", env.testAdminSettings)
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	adminDSN := os.Getenv("OPENTALK_TEST_MYSQL_DSN")
	redisAddr := os.Getenv("OPENTALK_TEST_REDIS_ADDR")
	if adminDSN == "" || redisAddr == "" {
		t.Skip("set OPENTALK_TEST_MYSQL_DSN and OPENTALK_TEST_REDIS_ADDR to run account integration checks")
	}
	db, cleanup, err := testdb.Open(adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	// A dedicated Redis database keeps the test from touching the development instance's data.
	cache := redis.NewClient(&redis.Options{Addr: redisAddr, DB: redisTestDBIdx})
	t.Cleanup(func() { _ = cache.Close() })
	if err := cache.FlushDB(context.Background()).Err(); err != nil {
		t.Fatalf("cannot clear the test Redis database: %v", err)
	}

	secret := []byte("integration-test-secret-value-0123456789")
	csrfKey, err := secretbox.Derive(secret, "session-csrf")
	if err != nil {
		t.Fatal(err)
	}
	challengeKey, err := secretbox.Derive(secret, "challenge-digest")
	if err != nil {
		t.Fatal(err)
	}
	settingsKey, err := secretbox.Derive(secret, "settings-secret")
	if err != nil {
		t.Fatal(err)
	}
	env := &testEnv{
		t:          t,
		db:         db,
		cache:      cache,
		settings:   &settings.Store{DB: db, Key: settingsKey},
		challenges: Challenges{Redis: cache, HashKey: challengeKey},
	}
	deps := Deps{
		Store:      Store{DB: db},
		Sessions:   Sessions{DB: db, CSRFKey: csrfKey, Secure: false},
		Challenges: env.challenges,
		Settings:   env.settings,
		Logger:     slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}
	env.deps = deps
	env.sink = newSMTPSink(t)
	// Point SMTP at the sink through the same settings path an operator uses, which also proves a
	// settings change takes effect without a restart.
	host, port := env.sink.address()
	if _, err := env.settings.Update(context.Background(), 0, settings.Patch{
		SMTPHost:        &host,
		SMTPPort:        &port,
		Security:        stringPtr("none"),
		SMTPFromAddress: stringPtr("no-reply@example.test"),
		SMTPFromName:    stringPtr("OpenTalk 测试"),
	}, "integration test setup"); err != nil {
		t.Fatalf("cannot configure SMTP for the test: %v", err)
	}
	mux := http.NewServeMux()
	Register(mux, deps)
	env.handler = httpapi.Middleware(testOrigin, deps.Logger, mux)
	return env
}

// --- HTTP helpers ---------------------------------------------------------------------------

type response struct {
	status  int
	body    []byte
	headers http.Header
	cookie  string
}

func (e *testEnv) do(method, path string, payload any, cookie, csrf string) response {
	e.t.Helper()
	var reader *bytes.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			e.t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	} else {
		reader = bytes.NewReader(nil)
	}
	request := httptest.NewRequest(method, path, reader)
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if method != "GET" && method != "HEAD" {
		request.Header.Set("Origin", testOrigin)
	}
	if cookie != "" {
		request.Header.Set("Cookie", SessionCookieName+"="+cookie)
	}
	if csrf != "" {
		request.Header.Set("X-CSRF-Token", csrf)
	}
	recorder := httptest.NewRecorder()
	e.handler.ServeHTTP(recorder, request)
	result := response{status: recorder.Code, body: recorder.Body.Bytes(), headers: recorder.Header()}
	for _, c := range recorder.Result().Cookies() {
		if c.Name == SessionCookieName && c.Value != "" {
			result.cookie = c.Value
		}
	}
	return result
}

func (e *testEnv) decode(res response, target any) {
	e.t.Helper()
	if err := json.Unmarshal(res.body, target); err != nil {
		e.t.Fatalf("cannot decode %s: %v", string(res.body), err)
	}
}

func (e *testEnv) expectStatus(res response, want int) {
	e.t.Helper()
	if res.status != want {
		e.t.Fatalf("status=%d want=%d body=%s", res.status, want, string(res.body))
	}
}

func (e *testEnv) expectErrorCode(res response, wantStatus int, wantCode string) {
	e.t.Helper()
	e.expectStatus(res, wantStatus)
	var body struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	}
	e.decode(res, &body)
	if body.Code != wantCode {
		e.t.Fatalf("code=%q want=%q body=%s", body.Code, wantCode, string(res.body))
	}
	if body.RequestID == "" {
		e.t.Fatalf("error response has no request_id: %s", string(res.body))
	}
}

// clearEmailLimits removes the sending ceilings for an address so a test can request several codes
// without waiting out the resend interval. The ceilings themselves are asserted in testRateLimits.
func (e *testEnv) clearEmailLimits(email string) {
	e.t.Helper()
	keys := []string{
		"resend:email:" + e.challenges.opaque("email", email),
		"rl:email:" + e.challenges.opaque("email", email),
		"rl:ip:192.0.2.1",
	}
	if err := e.cache.Del(context.Background(), keys...).Err(); err != nil {
		e.t.Fatal(err)
	}
}

// solvedCaptcha issues a challenge and recovers its answer. Only the digest is stored, so the test
// searches the small challenge space, which also proves the digest really is bound to the answer.
func (e *testEnv) solvedCaptcha(purpose string) (string, string) {
	e.t.Helper()
	res := e.do("GET", "/api/v1/auth/captcha?purpose="+purpose, nil, "", "")
	e.expectStatus(res, 200)
	var challenge struct {
		ChallengeID string `json:"challenge_id"`
		ImageURL    string `json:"image_url"`
		ExpiresAt   string `json:"expires_at"`
	}
	e.decode(res, &challenge)
	if challenge.ChallengeID == "" || challenge.ImageURL == "" || challenge.ExpiresAt == "" {
		e.t.Fatalf("incomplete challenge: %s", string(res.body))
	}
	return challenge.ChallengeID, e.bruteForceAnswer(mustGet(e.t, e, e.challenges.captchaKey(challenge.ChallengeID, "a")))
}

func (e *testEnv) bruteForceAnswer(storedDigest string) string {
	e.t.Helper()
	alphabet := captcha.Alphabet
	candidate := make([]byte, 4)
	for i := 0; i < len(alphabet); i++ {
		candidate[0] = alphabet[i]
		for j := 0; j < len(alphabet); j++ {
			candidate[1] = alphabet[j]
			for k := 0; k < len(alphabet); k++ {
				candidate[2] = alphabet[k]
				for l := 0; l < len(alphabet); l++ {
					candidate[3] = alphabet[l]
					if e.challenges.digest("captcha", string(candidate)) == storedDigest {
						return string(candidate)
					}
				}
			}
		}
	}
	e.t.Fatal("stored captcha digest does not match any candidate")
	return ""
}

// requestEmailCode walks the captcha step and returns the delivered challenge id and code.
func (e *testEnv) requestEmailCode(email, purpose string) (string, string) {
	e.t.Helper()
	e.clearEmailLimits(email)
	challengeID, answer := e.solvedCaptcha(purpose)
	res := e.do("POST", "/api/v1/auth/email-codes", map[string]string{
		"email":                email,
		"purpose":              purpose,
		"captcha_challenge_id": challengeID,
		"captcha_answer":       answer,
	}, "", "")
	e.expectStatus(res, 202)
	var body struct {
		ChallengeID       string `json:"challenge_id"`
		ExpiresAt         string `json:"expires_at"`
		RetryAfterSeconds int    `json:"retry_after_seconds"`
	}
	e.decode(res, &body)
	if body.ChallengeID == "" || body.ExpiresAt == "" {
		e.t.Fatalf("incomplete email challenge: %s", string(res.body))
	}
	message := e.sink.next(e.t)
	match := codeExtractor.FindStringSubmatch(message)
	if match == nil {
		e.t.Fatalf("delivered message has no code: %s", message)
	}
	return body.ChallengeID, match[1]
}

func (e *testEnv) registerUser(email, nickname string) {
	e.t.Helper()
	challengeID, code := e.requestEmailCode(email, "register")
	res := e.do("POST", "/api/v1/auth/register", map[string]string{
		"email":        email,
		"challenge_id": challengeID,
		"email_code":   code,
		"password":     testPassword,
		"nickname":     nickname,
	}, "", "")
	e.expectStatus(res, 204)
}

func (e *testEnv) login(email, password string) response {
	e.t.Helper()
	return e.do("POST", "/api/v1/auth/login", map[string]string{"email": email, "password": password}, "", "")
}

func (e *testEnv) loginOK(email, password string) (string, string) {
	e.t.Helper()
	res := e.login(email, password)
	e.expectStatus(res, 200)
	var body struct {
		User struct {
			Email string `json:"email"`
		} `json:"user"`
		CSRFToken string `json:"csrf_token"`
	}
	e.decode(res, &body)
	if res.cookie == "" || body.CSRFToken == "" {
		e.t.Fatal("login did not return a session cookie and CSRF token")
	}
	return res.cookie, body.CSRFToken
}

func (e *testEnv) createAdmin(email, nickname string) {
	e.t.Helper()
	hash, err := account.HashPassword(adminPassword)
	if err != nil {
		e.t.Fatal(err)
	}
	publicID, err := account.NewPublicID()
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.deps.Store.CreateUser(context.Background(), NewUser{
		Email: email, PasswordHash: hash, Nickname: nickname, PublicID: publicID, Role: "admin",
	}); err != nil {
		e.t.Fatal(err)
	}
}

// --- sub-tests ------------------------------------------------------------------------------

func (e *testEnv) testCaptcha(t *testing.T) {
	// The purpose is mandatory because the stored challenge is bound to it.
	e.expectErrorCode(e.do("GET", "/api/v1/auth/captcha", nil, "", ""), 422, "CAPTCHA_PURPOSE_INVALID")
	e.expectErrorCode(e.do("GET", "/api/v1/auth/captcha?purpose=other", nil, "", ""), 422, "CAPTCHA_PURPOSE_INVALID")

	res := e.do("GET", "/api/v1/auth/captcha?purpose=login", nil, "", "")
	e.expectStatus(res, 200)
	var challenge struct {
		ChallengeID string `json:"challenge_id"`
		ImageURL    string `json:"image_url"`
	}
	e.decode(res, &challenge)
	image := e.do("GET", challenge.ImageURL, nil, "", "")
	e.expectStatus(image, 200)
	if got := image.headers.Get("Content-Type"); got != "image/png" {
		t.Fatalf("image content-type=%q", got)
	}
	if got := image.headers.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("image must not be cacheable, got Cache-Control=%q", got)
	}
	if !bytes.HasPrefix(image.body, []byte{0x89, 'P', 'N', 'G'}) {
		t.Fatal("image body is not a PNG")
	}
	e.expectErrorCode(e.do("GET", "/api/v1/auth/captcha/does-not-exist/image", nil, "", ""), 404, "NOT_FOUND")

	// A wrong answer costs an attempt, and the correct answer still works afterwards.
	registerID, registerAnswer := e.solvedCaptcha("register")
	wrong := e.do("POST", "/api/v1/auth/email-codes", map[string]string{
		"email": "captcha@example.test", "purpose": "register",
		"captcha_challenge_id": registerID, "captcha_answer": wrongCaptchaAnswer,
	}, "", "")
	e.expectErrorCode(wrong, 422, "CAPTCHA_INVALID")
	e.clearEmailLimits("captcha@example.test")
	recovered := e.do("POST", "/api/v1/auth/email-codes", map[string]string{
		"email": "captcha@example.test", "purpose": "register",
		"captcha_challenge_id": registerID, "captcha_answer": registerAnswer,
	}, "", "")
	// The address is unregistered, so a code is issued; the point here is that the second attempt
	// succeeded where an exhausted challenge would have failed.
	e.expectStatus(recovered, 202)
	_ = e.sink.next(t)

	// Purpose binding: an answer solved for register cannot be spent on reset_password.
	purposeID, purposeAnswer := e.solvedCaptcha("register")
	mismatch := e.do("POST", "/api/v1/auth/email-codes", map[string]string{
		"email": "captcha2@example.test", "purpose": "reset_password",
		"captcha_challenge_id": purposeID, "captcha_answer": purposeAnswer,
	}, "", "")
	e.expectErrorCode(mismatch, 422, "CAPTCHA_PURPOSE_MISMATCH")

	// Attempts are finite: consuming them all makes even the correct answer unusable, which is the
	// replay and brute-force brake.
	attemptID, attemptAnswer := e.solvedCaptcha("login")
	for i := 0; i < 3; i++ {
		if err := e.challenges.VerifyCaptcha(context.Background(), attemptID, "login", wrongCaptchaAnswer); err == nil {
			t.Fatal("a wrong captcha answer was accepted")
		}
	}
	if err := e.challenges.VerifyCaptcha(context.Background(), attemptID, "login", attemptAnswer); err == nil {
		t.Fatal("an exhausted challenge still accepted the correct answer")
	}
}

func (e *testEnv) testRegistration(t *testing.T) {
	const email = "newuser@example.test"
	challengeID, code := e.requestEmailCode(email, "register")

	// A wrong code is rejected and does not create an account.
	wrong := e.do("POST", "/api/v1/auth/register", map[string]string{
		"email": email, "challenge_id": challengeID, "email_code": "000000",
		"password": testPassword, "nickname": "测试用户",
	}, "", "")
	e.expectErrorCode(wrong, 422, "EMAIL_CODE_INVALID")
	if _, _, err := e.deps.Store.UserByEmail(context.Background(), email); err == nil {
		t.Fatal("a rejected registration created an account")
	}

	// Input rules come from the confirmed requirements. Validation runs before the code is consumed,
	// so these cases can reuse the same challenge.
	for name, payload := range map[string]map[string]string{
		"password too short":   {"email": "weak1@example.test", "challenge_id": challengeID, "email_code": code, "password": "abc123", "nickname": "测试用户"},
		"password digits only": {"email": "weak2@example.test", "challenge_id": challengeID, "email_code": code, "password": "12345678", "nickname": "测试用户"},
		"password too long":    {"email": "weak4@example.test", "challenge_id": challengeID, "email_code": code, "password": "abc12345678901234", "nickname": "测试用户"},
		"nickname with digit":  {"email": "weak3@example.test", "challenge_id": challengeID, "email_code": code, "password": testPassword, "nickname": "user123"},
		"nickname too long":    {"email": "weak5@example.test", "challenge_id": challengeID, "email_code": code, "password": testPassword, "nickname": "汉字测试昵称超过十个字符"},
		"invalid email":        {"email": "not-an-email", "challenge_id": challengeID, "email_code": code, "password": testPassword, "nickname": "测试用户"},
	} {
		t.Run(name, func(t *testing.T) {
			res := e.do("POST", "/api/v1/auth/register", payload, "", "")
			if res.status != 422 {
				t.Fatalf("status=%d want=422 body=%s", res.status, string(res.body))
			}
		})
	}

	// Register for real. The challenge from the wrong-code attempt still has attempts left, and the
	// correct code was never spent, so no new code is needed.
	res := e.do("POST", "/api/v1/auth/register", map[string]string{
		"email": email, "challenge_id": challengeID, "email_code": code,
		"password": testPassword, "nickname": "测试用户",
	}, "", "")
	e.expectStatus(res, 204)

	// Replaying the same request is answered by the duplicate check rather than by the code check: the
	// email really is taken, and saying so is more useful than blaming the code.
	replay := e.do("POST", "/api/v1/auth/register", map[string]string{
		"email": email, "challenge_id": challengeID, "email_code": code,
		"password": testPassword, "nickname": "测试用户",
	}, "", "")
	e.expectErrorCode(replay, 409, "EMAIL_ALREADY_REGISTERED")

	// The consumed code cannot be spent on a different address, which proves it is single use.
	reused := e.do("POST", "/api/v1/auth/register", map[string]string{
		"email": "reuse@example.test", "challenge_id": challengeID, "email_code": code,
		"password": testPassword, "nickname": "复用测试",
	}, "", "")
	e.expectErrorCode(reused, 422, "EMAIL_CODE_INVALID")

	// An email code for an already registered address is refused before sending.
	e.clearEmailLimits(email)
	duplicateID, duplicateAnswer := e.solvedCaptcha("register")
	duplicate := e.do("POST", "/api/v1/auth/email-codes", map[string]string{
		"email": email, "purpose": "register",
		"captcha_challenge_id": duplicateID, "captcha_answer": duplicateAnswer,
	}, "", "")
	e.expectErrorCode(duplicate, 409, "EMAIL_ALREADY_REGISTERED")

	// Two accounts may share a nickname.
	nicknameID, nicknameCode := e.requestEmailCode("samename@example.test", "register")
	shared := e.do("POST", "/api/v1/auth/register", map[string]string{
		"email": "samename@example.test", "challenge_id": nicknameID, "email_code": nicknameCode,
		"password": testPassword, "nickname": "测试用户",
	}, "", "")
	e.expectStatus(shared, 204)

	// Registration must not hand out a session.
	var sessions int
	if err := e.db.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions != 0 {
		t.Fatalf("registration created %d sessions, want 0", sessions)
	}
}

// testConcurrentRegistration proves the unique index, not the pre-check, is what stops two
// simultaneous registrations for one address from both succeeding. The goroutines build their own
// requests so no testing.T method is called off the test goroutine.
func (e *testEnv) testConcurrentRegistration(t *testing.T) {
	const email = "race@example.test"
	const workers = 8
	challengeID, code := e.requestEmailCode(email, "register")
	payload, err := json.Marshal(map[string]string{
		"email": email, "challenge_id": challengeID, "email_code": code,
		"password": testPassword, "nickname": "并发测试",
	})
	if err != nil {
		t.Fatal(err)
	}
	statuses := make(chan int, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			request := httptest.NewRequest("POST", "/api/v1/auth/register", bytes.NewReader(payload))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Origin", testOrigin)
			recorder := httptest.NewRecorder()
			e.handler.ServeHTTP(recorder, request)
			statuses <- recorder.Code
		}()
	}
	wg.Wait()
	close(statuses)
	created, rejected := 0, 0
	for status := range statuses {
		switch status {
		case 204:
			created++
		case 409, 422:
			rejected++
		default:
			t.Fatalf("unexpected status %d", status)
		}
	}
	if created != 1 || rejected != workers-1 {
		t.Fatalf("created=%d rejected=%d want 1 and %d", created, rejected, workers-1)
	}
	var accounts int
	if err := e.db.QueryRow("SELECT COUNT(*) FROM users WHERE email_normalized=?", email).Scan(&accounts); err != nil {
		t.Fatal(err)
	}
	if accounts != 1 {
		t.Fatalf("concurrent registration created %d accounts", accounts)
	}
}

func (e *testEnv) testLoginSession(t *testing.T) {
	const email = "login@example.test"
	e.registerUser(email, "登录测试")

	// A wrong password and an unknown address answer identically, so neither reveals registration.
	e.expectErrorCode(e.login(email, "wrong1234"), 401, "INVALID_CREDENTIALS")
	e.expectErrorCode(e.login("ghost@example.test", "wrong1234"), 401, "INVALID_CREDENTIALS")

	cookie, csrf := e.loginOK(email, testPassword)

	me := e.do("GET", "/api/v1/me", nil, cookie, "")
	e.expectStatus(me, 200)
	var profile struct {
		Profile struct {
			PublicID string `json:"public_id"`
			Nickname string `json:"nickname"`
		} `json:"profile"`
		Email         string `json:"email"`
		Role          string `json:"role"`
		AccountStatus string `json:"account_status"`
		IsMuted       bool   `json:"is_muted"`
	}
	e.decode(me, &profile)
	if profile.Email != email || profile.Role != "user" || profile.AccountStatus != "active" || profile.IsMuted {
		t.Fatalf("unexpected current user: %s", string(me.body))
	}
	if profile.Profile.PublicID == "" || profile.Profile.Nickname != "登录测试" {
		t.Fatalf("unexpected profile: %s", string(me.body))
	}
	for _, forbidden := range []string{"password", "hash", "csrf", "session", "email_normalized"} {
		if strings.Contains(strings.ToLower(string(me.body)), forbidden) {
			t.Fatalf("GET /me leaks %q: %s", forbidden, string(me.body))
		}
	}

	// The derived token is stable, so several tabs can read it without rotating it.
	refresh := e.do("GET", "/api/v1/auth/csrf", nil, cookie, "")
	e.expectStatus(refresh, 200)
	var token struct {
		CSRFToken string `json:"csrf_token"`
	}
	e.decode(refresh, &token)
	if token.CSRFToken != csrf {
		t.Fatal("CSRF token rotated on refresh")
	}

	// A write without the token, and with a forged one, is rejected.
	e.expectErrorCode(e.do("PATCH", "/api/v1/me", map[string]string{"nickname": "新昵称"}, cookie, ""), 403, "CSRF_REJECTED")
	e.expectErrorCode(e.do("PATCH", "/api/v1/me", map[string]string{"nickname": "新昵称"}, cookie, strings.Repeat("a", 43)), 403, "CSRF_REJECTED")

	// A write from another origin is rejected before CSRF is considered.
	request := httptest.NewRequest("PATCH", "/api/v1/me", strings.NewReader(`{"nickname":"跨站"}`))
	request.Header.Set("Origin", "http://evil.example.test")
	request.Header.Set("Cookie", SessionCookieName+"="+cookie)
	request.Header.Set("X-CSRF-Token", csrf)
	recorder := httptest.NewRecorder()
	e.handler.ServeHTTP(recorder, request)
	if recorder.Code != 403 {
		t.Fatalf("cross-origin write status=%d want=403", recorder.Code)
	}

	// Logout revokes the session immediately.
	logout := e.do("POST", "/api/v1/auth/logout", nil, cookie, csrf)
	e.expectStatus(logout, 204)
	e.expectErrorCode(e.do("GET", "/api/v1/me", nil, cookie, ""), 401, "UNAUTHENTICATED")
	e.expectErrorCode(e.do("GET", "/api/v1/me", nil, "", ""), 401, "UNAUTHENTICATED")
}

func (e *testEnv) testProfile(t *testing.T) {
	const email = "profile@example.test"
	e.registerUser(email, "资料测试")
	cookie, csrf := e.loginOK(email, testPassword)

	res := e.do("PATCH", "/api/v1/me", map[string]string{
		"nickname": "新名字", "bio": strings.Repeat("说明", 100),
	}, cookie, csrf)
	e.expectStatus(res, 200)
	var body struct {
		Profile struct {
			Nickname string `json:"nickname"`
			Bio      string `json:"bio"`
		} `json:"profile"`
	}
	e.decode(res, &body)
	if body.Profile.Nickname != "新名字" || body.Profile.Bio == "" {
		t.Fatalf("profile not updated: %s", string(res.body))
	}

	e.expectErrorCode(e.do("PATCH", "/api/v1/me", map[string]string{"bio": strings.Repeat("字", 301)}, cookie, csrf), 422, "BIO_TOO_LONG")
	e.expectErrorCode(e.do("PATCH", "/api/v1/me", map[string]string{"nickname": "abc123"}, cookie, csrf), 422, "NICKNAME_CHARACTERS")
	e.expectErrorCode(e.do("PATCH", "/api/v1/me", map[string]string{"nickname": strings.Repeat("汉", 11)}, cookie, csrf), 422, "NICKNAME_TOO_LONG")
	e.expectStatus(e.do("PATCH", "/api/v1/me", map[string]any{}, cookie, csrf), 422)
	// An avatar the caller does not own is refused without confirming whether it exists.
	e.expectErrorCode(e.do("PATCH", "/api/v1/me", map[string]string{"avatar_attachment_id": "999999"}, cookie, csrf), 404, "NOT_FOUND")
	e.expectErrorCode(e.do("PATCH", "/api/v1/me", map[string]string{"avatar_attachment_id": "not-a-number"}, cookie, csrf), 422, "AVATAR_INVALID")
}

func (e *testEnv) testResetPassword(t *testing.T) {
	const email = "reset@example.test"
	e.registerUser(email, "找回测试")
	firstCookie, _ := e.loginOK(email, testPassword)
	secondCookie, _ := e.loginOK(email, testPassword)

	// An unknown address answers exactly like a known one and receives no mail.
	deliveredBefore := e.sink.count()
	e.clearEmailLimits("missing@example.test")
	unknownID, unknownAnswer := e.solvedCaptcha("reset_password")
	unknown := e.do("POST", "/api/v1/auth/email-codes", map[string]string{
		"email": "missing@example.test", "purpose": "reset_password",
		"captcha_challenge_id": unknownID, "captcha_answer": unknownAnswer,
	}, "", "")
	e.expectStatus(unknown, 202)
	var unknownBody struct {
		ChallengeID string `json:"challenge_id"`
		ExpiresAt   string `json:"expires_at"`
	}
	e.decode(unknown, &unknownBody)
	if unknownBody.ChallengeID == "" || unknownBody.ExpiresAt == "" {
		t.Fatalf("unknown address response is distinguishable: %s", string(unknown.body))
	}
	if got := e.sink.count(); got != deliveredBefore {
		t.Fatalf("a code was delivered for an unknown address (%d -> %d)", deliveredBefore, got)
	}
	// A code for that challenge can never verify.
	unknownReset := e.do("POST", "/api/v1/auth/reset-password", map[string]string{
		"email": "missing@example.test", "challenge_id": unknownBody.ChallengeID,
		"email_code": "123456", "new_password": "newpass123",
	}, "", "")
	e.expectErrorCode(unknownReset, 422, "EMAIL_CODE_INVALID")

	challengeID, code := e.requestEmailCode(email, "reset_password")
	reset := e.do("POST", "/api/v1/auth/reset-password", map[string]string{
		"email": email, "challenge_id": challengeID, "email_code": code, "new_password": "newpass123",
	}, "", "")
	e.expectStatus(reset, 204)

	// The consumed code cannot be replayed.
	replay := e.do("POST", "/api/v1/auth/reset-password", map[string]string{
		"email": email, "challenge_id": challengeID, "email_code": code, "new_password": "newpass456",
	}, "", "")
	e.expectErrorCode(replay, 422, "EMAIL_CODE_INVALID")

	// Every session issued before the reset is gone.
	for name, cookie := range map[string]string{"first": firstCookie, "second": secondCookie} {
		if revoked := e.do("GET", "/api/v1/me", nil, cookie, ""); revoked.status != 401 {
			t.Fatalf("%s session survived the password reset: status=%d", name, revoked.status)
		}
	}
	e.expectErrorCode(e.login(email, testPassword), 401, "INVALID_CREDENTIALS")
	e.loginOK(email, "newpass123")
}

func (e *testEnv) testRateLimits(t *testing.T) {
	const email = "limits@example.test"
	e.registerUser(email, "限流测试")

	// The resend interval rejects a second send for the same address.
	firstID, _ := e.requestEmailCode(email, "reset_password")
	if firstID == "" {
		t.Fatal("first challenge was not issued")
	}
	secondChallenge, secondAnswer := e.solvedCaptcha("reset_password")
	limited := e.do("POST", "/api/v1/auth/email-codes", map[string]string{
		"email": email, "purpose": "reset_password",
		"captcha_challenge_id": secondChallenge, "captcha_answer": secondAnswer,
	}, "", "")
	e.expectErrorCode(limited, 429, "RATE_LIMITED")
	if limited.headers.Get("Retry-After") == "" {
		t.Fatal("429 response is missing Retry-After")
	}

	// The per-address hourly ceiling applies even when the resend interval is cleared.
	e.clearEmailLimits(email)
	for i := 0; i < 5; i++ {
		if err := e.cache.Set(context.Background(), "rl:email:"+e.challenges.opaque("email", email), "5", time.Hour).Err(); err != nil {
			t.Fatal(err)
		}
		challenge, answer := e.solvedCaptcha("reset_password")
		res := e.do("POST", "/api/v1/auth/email-codes", map[string]string{
			"email": email, "purpose": "reset_password",
			"captcha_challenge_id": challenge, "captcha_answer": answer,
		}, "", "")
		e.expectErrorCode(res, 429, "RATE_LIMITED")
		break
	}

	// The login captcha threshold engages after five failures, and the counter does not depend on the
	// address existing.
	for i := 0; i < 5; i++ {
		e.expectStatus(e.login(email, "wrong1234"), 401)
	}
	e.expectErrorCode(e.login(email, testPassword), 422, "CAPTCHA_REQUIRED")

	// A valid image challenge lets the correct password through and clears the counter.
	challengeID, answer := e.solvedCaptcha("login")
	allowed := e.do("POST", "/api/v1/auth/login", map[string]string{
		"email": email, "password": testPassword,
		"captcha_challenge_id": challengeID, "captcha_answer": answer,
	}, "", "")
	e.expectStatus(allowed, 200)
	e.loginOK(email, testPassword)
}

func (e *testEnv) testAdminSettings(t *testing.T) {
	const email = "settings@example.test"
	e.registerUser(email, "设置测试")
	memberCookie, memberCSRF := e.loginOK(email, testPassword)
	e.createAdmin("root@example.test", "管理员")
	adminCookie, adminCSRF := e.loginOK("root@example.test", adminPassword)

	// A normal member must not read or write settings, and neither may an anonymous caller.
	e.expectErrorCode(e.do("GET", "/api/v1/admin/settings", nil, memberCookie, ""), 403, "FORBIDDEN")
	e.expectErrorCode(e.do("PATCH", "/api/v1/admin/settings", map[string]any{"email_code_max_attempts": 1, "reason": "越权"}, memberCookie, memberCSRF), 403, "FORBIDDEN")
	e.expectErrorCode(e.do("GET", "/api/v1/admin/settings", nil, "", ""), 401, "UNAUTHENTICATED")
	e.expectErrorCode(e.do("PATCH", "/api/v1/admin/settings", map[string]any{"email_code_max_attempts": 1, "reason": "匿名"}, adminCookie, ""), 403, "CSRF_REJECTED")

	adminRead := e.do("GET", "/api/v1/admin/settings", nil, adminCookie, "")
	e.expectStatus(adminRead, 200)
	var before map[string]any
	e.decode(adminRead, &before)
	if strings.Contains(string(adminRead.body), `"smtp_password"`) {
		t.Fatalf("settings response exposes smtp_password: %s", string(adminRead.body))
	}
	// The test sink is an unauthenticated relay, so no password is stored yet and the flag must say so.
	if set, ok := before["smtp_password_set"].(bool); !ok || set {
		t.Fatalf("expected smtp_password_set=false before a password is configured: %s", string(adminRead.body))
	}

	// A reason is mandatory, and out-of-range values are refused by the server rather than by MySQL.
	e.expectErrorCode(e.do("PATCH", "/api/v1/admin/settings", map[string]any{"email_code_max_attempts": 2}, adminCookie, adminCSRF), 422, "REASON_REQUIRED")
	e.expectErrorCode(e.do("PATCH", "/api/v1/admin/settings", map[string]any{"email_code_max_attempts": 999, "reason": "越界"}, adminCookie, adminCSRF), 422, "SETTING_INVALID")
	e.expectErrorCode(e.do("PATCH", "/api/v1/admin/settings", map[string]any{"smtp_security": "plain", "reason": "错误模式"}, adminCookie, adminCSRF), 422, "SETTING_INVALID")
	e.expectErrorCode(e.do("PATCH", "/api/v1/admin/settings", map[string]any{"reason": "没有字段"}, adminCookie, adminCSRF), 422, "INVALID_REQUEST")
	// 000003 image limits are admin-editable too, and their bounds match the SQL CHECK constraints.
	e.expectErrorCode(e.do("PATCH", "/api/v1/admin/settings", map[string]any{"media_max_bytes": 1024, "reason": "越界"}, adminCookie, adminCSRF), 422, "SETTING_INVALID")
	e.expectErrorCode(e.do("PATCH", "/api/v1/admin/settings", map[string]any{"media_max_dimension": 9000, "reason": "越界"}, adminCookie, adminCSRF), 422, "SETTING_INVALID")
	e.expectErrorCode(e.do("PATCH", "/api/v1/admin/settings", map[string]any{"media_max_per_content": 0, "reason": "越界"}, adminCookie, adminCSRF), 422, "SETTING_INVALID")
	e.expectErrorCode(e.do("PATCH", "/api/v1/admin/settings", map[string]any{"media_orphan_cleanup_days": 365, "reason": "越界"}, adminCookie, adminCSRF), 422, "SETTING_INVALID")

	updated := e.do("PATCH", "/api/v1/admin/settings", map[string]any{
		"email_code_max_attempts": 7, "media_max_bytes": 2097152, "media_max_per_content": 5,
		"reason": "集成测试",
	}, adminCookie, adminCSRF)
	e.expectStatus(updated, 200)
	var after struct {
		EmailCodeMaxAttempts int `json:"email_code_max_attempts"`
		MediaMaxBytes        int `json:"media_max_bytes"`
		MediaMaxDimension    int `json:"media_max_dimension"`
		MediaMaxPerContent   int `json:"media_max_per_content"`
		MediaPendingTTL      int `json:"media_pending_ttl_hours"`
		MediaOrphanDays      int `json:"media_orphan_cleanup_days"`
	}
	e.decode(updated, &after)
	if after.EmailCodeMaxAttempts != 7 {
		t.Fatalf("setting not applied: %s", string(updated.body))
	}
	if after.MediaMaxBytes != 2097152 || after.MediaMaxPerContent != 5 {
		t.Fatalf("image limits not applied: %s", string(updated.body))
	}
	// The untouched media settings keep their migration defaults.
	if after.MediaMaxDimension != 4096 || after.MediaPendingTTL != 24 || after.MediaOrphanDays != 7 {
		t.Fatalf("untouched image defaults changed: %s", string(updated.body))
	}
	reloaded := e.do("GET", "/api/v1/admin/settings", nil, adminCookie, "")
	e.decode(reloaded, &after)
	if after.EmailCodeMaxAttempts != 7 || after.MediaMaxBytes != 2097152 || after.MediaMaxPerContent != 5 {
		t.Fatalf("setting did not take effect immediately: %s", string(reloaded.body))
	}

	// The audit trail names the changed fields and contains no values.
	var reason string
	if err := e.db.QueryRow("SELECT reason FROM moderation_actions WHERE action='update_settings' ORDER BY id DESC LIMIT 1").Scan(&reason); err != nil {
		t.Fatalf("settings change was not audited: %v", err)
	}
	if !strings.Contains(reason, "email_code_max_attempts") {
		t.Fatalf("audit reason does not name the changed field: %q", reason)
	}
	if strings.Contains(reason, "7") {
		t.Fatalf("audit reason contains a value: %q", reason)
	}
	var auditRows int
	if err := e.db.QueryRow("SELECT COUNT(*) FROM moderation_actions").Scan(&auditRows); err != nil {
		t.Fatal(err)
	}
	if auditRows != 1 {
		t.Fatalf("admin API wrote %d audit rows, want 1 (test setup must not audit)", auditRows)
	}

	// Storing an SMTP password is write-only: the response carries a presence flag, never the value.
	const smtpPassword = "integration-test-secret-smtp-password"
	withPassword := e.do("PATCH", "/api/v1/admin/settings", map[string]any{"smtp_password": smtpPassword, "reason": "设置邮件密码"}, adminCookie, adminCSRF)
	e.expectStatus(withPassword, 200)
	if strings.Contains(string(withPassword.body), smtpPassword) {
		t.Fatal("settings response echoed the SMTP password")
	}
	var flagged struct {
		SMTPPasswordSet bool `json:"smtp_password_set"`
	}
	e.decode(withPassword, &flagged)
	if !flagged.SMTPPasswordSet {
		t.Fatalf("smtp_password_set did not become true: %s", string(withPassword.body))
	}
	// The column holds ciphertext, and the value survives a round trip through the store.
	var ciphertext []byte
	if err := e.db.QueryRow("SELECT smtp_password_ciphertext FROM settings WHERE id=1").Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	if len(ciphertext) == 0 {
		t.Fatal("SMTP password was not stored")
	}
	if bytes.Contains(ciphertext, []byte(smtpPassword)) {
		t.Fatal("stored SMTP password is not encrypted")
	}
	current, err := e.settings.Load(context.Background())
	if err != nil {
		t.Fatalf("stored SMTP password cannot be decrypted: %v", err)
	}
	if current.SMTPPassword != smtpPassword {
		t.Fatal("stored SMTP password did not round trip")
	}
	// Clearing the password is how an unauthenticated relay is configured.
	clearRes := e.do("PATCH", "/api/v1/admin/settings", map[string]any{"smtp_password": "", "reason": "清除密码"}, adminCookie, adminCSRF)
	e.expectStatus(clearRes, 200)
	var cleared struct {
		SMTPPasswordSet bool `json:"smtp_password_set"`
	}
	e.decode(clearRes, &cleared)
	if cleared.SMTPPasswordSet {
		t.Fatalf("password was not cleared: %s", string(clearRes.body))
	}
	if err := e.db.QueryRow("SELECT smtp_password_ciphertext FROM settings WHERE id=1").Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	if len(ciphertext) != 0 {
		t.Fatal("clearing the password left ciphertext behind")
	}

	// A banned account cannot log in, and an existing session is revoked at the next login attempt.
	if _, err := e.db.Exec(`INSERT INTO user_restrictions (user_id,kind,reason,starts_at,created_by)
 SELECT id,'ban','集成测试',UTC_TIMESTAMP(6),(SELECT id FROM users WHERE email_normalized='root@example.test')
 FROM users WHERE email_normalized=?`, email); err != nil {
		t.Fatal(err)
	}
	e.expectErrorCode(e.login(email, testPassword), 403, "ACCOUNT_BANNED")
	if survivor := e.do("GET", "/api/v1/me", nil, memberCookie, ""); survivor.status != 401 {
		t.Fatalf("session survived the ban: status=%d", survivor.status)
	}
}

// --- helpers --------------------------------------------------------------------------------

func mustGet(t *testing.T, e *testEnv, key string) string {
	t.Helper()
	value, err := e.cache.Get(context.Background(), key).Result()
	if err != nil {
		t.Fatalf("redis key %s: %v", key, err)
	}
	return value
}

func stringPtr(value string) *string { return &value }

// smtpSink is a minimal SMTP server that records delivered messages, so the email path is exercised
// end to end without a real relay.
type smtpSink struct {
	listener net.Listener
	messages chan string
	mu       sync.Mutex
	total    int
}

func newSMTPSink(t *testing.T) *smtpSink {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("cannot start the SMTP sink: %v", err)
	}
	sink := &smtpSink{listener: listener, messages: make(chan string, 64)}
	go sink.serve()
	t.Cleanup(func() { _ = listener.Close() })
	return sink
}

func (s *smtpSink) address() (string, int) {
	return "127.0.0.1", s.listener.Addr().(*net.TCPAddr).Port
}

func (s *smtpSink) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *smtpSink) record(message string) {
	s.mu.Lock()
	s.total++
	s.mu.Unlock()
	select {
	case s.messages <- message:
	default:
	}
}

func (s *smtpSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.total
}

func (s *smtpSink) next(t *testing.T) string {
	t.Helper()
	select {
	case message := <-s.messages:
		return message
	case <-time.After(10 * time.Second):
		t.Fatal("no email was delivered")
		return ""
	}
}

func (s *smtpSink) handle(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)
	send := func(line string) {
		_, _ = writer.WriteString(line + "\r\n")
		_ = writer.Flush()
	}
	send("220 sink.invalid ESMTP")
	var data strings.Builder
	inData := false
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if inData {
			if line == "." {
				inData = false
				send("250 OK")
				s.record(data.String())
				data.Reset()
				continue
			}
			data.WriteString(line)
			data.WriteString("\n")
			continue
		}
		upper := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
			send("250-sink.invalid")
			send("250 8BITMIME")
		case strings.HasPrefix(upper, "MAIL FROM"), strings.HasPrefix(upper, "RCPT TO"):
			send("250 OK")
		case strings.HasPrefix(upper, "DATA"):
			send("354 End data with <CR><LF>.<CR><LF>")
			inData = true
		case strings.HasPrefix(upper, "QUIT"):
			send("221 Bye")
			return
		default:
			send("250 OK")
		}
	}
}
