package auth

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"opentalk/server/internal/account"
	"opentalk/server/internal/platform/httpapi"
	"opentalk/server/internal/settings"
)

// maxJSONBody bounds every account request body. These payloads carry short strings; anything larger
// is a mistake or an attack, and answering 413 is cheaper than parsing it.
const maxJSONBody = 64 << 10

// Store reads and writes account rows.
type Store struct{ DB *sql.DB }

// Deps carries everything the account endpoints need. Passing them explicitly keeps the endpoints
// testable without a global.
type Deps struct {
	Store      Store
	Sessions   Sessions
	Challenges Challenges
	Settings   *settings.Store
	Logger     *slog.Logger
	// TrustProxy must be true only when a reverse proxy that overwrites X-Forwarded-For sits in front.
	TrustProxy bool
}

// Register mounts the account endpoints. Every write route runs behind Require, so an authenticated
// write without a matching CSRF token is rejected before the handler sees it.
func Register(mux *http.ServeMux, deps Deps) {
	member := func(h http.HandlerFunc) http.Handler { return Require(deps.Sessions, false, h) }
	admin := func(h http.HandlerFunc) http.Handler { return Require(deps.Sessions, true, h) }

	mux.HandleFunc("GET /api/v1/auth/captcha", deps.getCaptcha)
	mux.HandleFunc("GET /api/v1/auth/captcha/{challenge_id}/image", deps.getCaptchaImage)
	mux.HandleFunc("POST /api/v1/auth/email-codes", deps.sendEmailCode)
	mux.HandleFunc("POST /api/v1/auth/register", deps.register)
	mux.HandleFunc("POST /api/v1/auth/login", deps.login)
	mux.HandleFunc("POST /api/v1/auth/reset-password", deps.resetPassword)
	mux.Handle("POST /api/v1/auth/logout", member(deps.logout))

	mux.Handle("GET /api/v1/auth/csrf", member(deps.getCSRF))
	mux.Handle("GET /api/v1/me", member(deps.getMe))
	mux.Handle("PATCH /api/v1/me", member(deps.updateMe))

	mux.Handle("GET /api/v1/admin/settings", admin(deps.getAdminSettings))
	mux.Handle("PATCH /api/v1/admin/settings", admin(deps.updateAdminSettings))
}

// --- response views -------------------------------------------------------------------------
// Field names and presence mirror docs/api/openapi.json. Optional fields are omitted rather than
// sent as null, and every timestamp is UTC RFC 3339.

type publicUserView struct {
	PublicID  string `json:"public_id"`
	Nickname  string `json:"nickname"`
	AvatarURL string `json:"avatar_url,omitempty"`
	Bio       string `json:"bio,omitempty"`
	CreatedAt string `json:"created_at"`
}

type currentUserView struct {
	Profile       publicUserView `json:"profile"`
	Email         string         `json:"email"`
	Role          string         `json:"role"`
	AccountStatus string         `json:"account_status"`
	IsMuted       bool           `json:"is_muted"`
	MuteExpiresAt string         `json:"mute_expires_at,omitempty"`
}

type authSessionView struct {
	User      currentUserView `json:"user"`
	CSRFToken string          `json:"csrf_token"`
}

type captchaChallengeView struct {
	ChallengeID string `json:"challenge_id"`
	ImageURL    string `json:"image_url"`
	ExpiresAt   string `json:"expires_at"`
}

type emailCodeChallengeView struct {
	ChallengeID       string `json:"challenge_id"`
	ExpiresAt         string `json:"expires_at"`
	RetryAfterSeconds int    `json:"retry_after_seconds"`
}

type csrfTokenView struct {
	CSRFToken string `json:"csrf_token"`
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func toPublicUser(u User) publicUserView {
	return publicUserView{
		PublicID:  u.PublicID,
		Nickname:  u.Nickname,
		AvatarURL: u.AvatarURL(),
		Bio:       u.Bio,
		CreatedAt: formatTime(u.CreatedAt),
	}
}

func toCurrentUser(u User) currentUserView {
	view := currentUserView{
		Profile:       toPublicUser(u),
		Email:         u.Email,
		Role:          u.Role,
		AccountStatus: u.AccountStatus(),
		IsMuted:       u.Muted,
	}
	if u.MuteExpiresAt != nil {
		view.MuteExpiresAt = formatTime(*u.MuteExpiresAt)
	}
	return view
}

// --- request helpers ------------------------------------------------------------------------

// decodeJSON enforces a size limit and rejects unknown fields, so a typo in a client payload fails
// loudly instead of being ignored silently.
func decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			httpapi.Fail(w, r, 413, "PAYLOAD_TOO_LARGE", "请求体过大")
			return false
		}
		httpapi.Fail(w, r, 400, "INVALID_JSON", "请求格式无效")
		return false
	}
	return true
}

// fieldFailure maps a validation sentinel to a 422 with a stable code. Messages describe the rule,
// never the submitted value.
func fieldFailure(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, account.ErrEmailInvalid):
		httpapi.Fail(w, r, 422, "EMAIL_INVALID", "邮箱格式无效")
	case errors.Is(err, account.ErrPasswordLength):
		httpapi.Fail(w, r, 422, "PASSWORD_LENGTH", "密码长度必须为8到16个字符")
	case errors.Is(err, account.ErrPasswordComposition):
		httpapi.Fail(w, r, 422, "PASSWORD_COMPOSITION", "密码必须同时包含字母和数字")
	case errors.Is(err, account.ErrNicknameEmpty):
		httpapi.Fail(w, r, 422, "NICKNAME_REQUIRED", "请填写昵称")
	case errors.Is(err, account.ErrNicknameLength):
		httpapi.Fail(w, r, 422, "NICKNAME_TOO_LONG", "昵称不能超过10个字符")
	case errors.Is(err, account.ErrNicknameCharacters):
		httpapi.Fail(w, r, 422, "NICKNAME_CHARACTERS", "昵称只能包含字母和汉字")
	case errors.Is(err, account.ErrBioLength):
		httpapi.Fail(w, r, 422, "BIO_TOO_LONG", "简介不能超过300个字符")
	default:
		var validation *ValidationError
		var settingInvalid *settings.ValidationError
		switch {
		case errors.As(err, &validation), errors.As(err, &settingInvalid):
			httpapi.Fail(w, r, 422, "INVALID_REQUEST", "请求参数无效")
		default:
			httpapi.Fail(w, r, 500, "INTERNAL_ERROR", "服务暂不可用")
		}
	}
}

// clientIP is the address the per-IP ceilings count against. Behind a reverse proxy the transport
// peer is the proxy itself, which would make the ceiling a site-wide quota, so an operator who trusts
// their proxy can opt in to reading X-Forwarded-For. Only the first entry is used, and only when
// TRUST_PROXY is enabled, because a client that reaches the service directly can forge the header.
func (d Deps) clientIP(r *http.Request) string {
	if d.TrustProxy {
		if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
			if first := strings.TrimSpace(strings.Split(forwarded, ",")[0]); first != "" {
				return first
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// challengeFailure maps challenge errors to the 422 the client should act on.
func challengeFailure(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrChallengeUnknown):
		httpapi.Fail(w, r, 422, "CAPTCHA_EXPIRED", "验证码已过期，请重新获取")
	case errors.Is(err, ErrChallengePurpose):
		httpapi.Fail(w, r, 422, "CAPTCHA_PURPOSE_MISMATCH", "验证码用途不匹配，请重新获取")
	default:
		httpapi.Fail(w, r, 422, "CAPTCHA_INVALID", "验证码不正确")
	}
}

// emailCodeFailure maps email code errors to 422. The same answer is returned whether the address is
// registered or not, so a password reset cannot be used to enumerate accounts.
func emailCodeFailure(w http.ResponseWriter, r *http.Request, err error) {
	httpapi.Fail(w, r, 422, "EMAIL_CODE_INVALID", "验证码无效或已过期")
}
