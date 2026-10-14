package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"errors"
	"net/http"

	"opentalk/server/internal/platform/httpapi"
)

type Principal struct {
	UserID   uint64
	Role     string
	CSRFHash []byte
}
type principalKey struct{}
type SessionStore interface {
	Lookup(context.Context, [32]byte) (Principal, error)
}

var ErrNoSession = errors.New("session unavailable")

type SQLSessions struct{ DB *sql.DB }

func (s SQLSessions) Lookup(ctx context.Context, hash [32]byte) (Principal, error) {
	var p Principal
	err := s.DB.QueryRowContext(ctx, `SELECT s.user_id,u.role,s.csrf_token_hash
 FROM sessions s JOIN users u ON u.id=s.user_id
 WHERE s.token_hash=? AND s.revoked_at IS NULL AND s.expires_at>UTC_TIMESTAMP(6)
 AND NOT EXISTS (SELECT 1 FROM user_restrictions r WHERE r.user_id=u.id AND r.kind='ban'
 AND r.revoked_at IS NULL AND r.starts_at<=UTC_TIMESTAMP(6) AND (r.ends_at IS NULL OR r.ends_at>UTC_TIMESTAMP(6)))`, hash[:]).Scan(&p.UserID, &p.Role, &p.CSRFHash)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNoSession
	}
	return p, err
}
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}
func Require(store SessionStore, admin bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("opentalk_session")
		if err != nil || len(cookie.Value) < 32 || len(cookie.Value) > 256 {
			httpapi.Fail(w, r, 401, "UNAUTHENTICATED", "请先登录")
			return
		}
		p, err := store.Lookup(r.Context(), sha256.Sum256([]byte(cookie.Value)))
		if errors.Is(err, ErrNoSession) {
			httpapi.Fail(w, r, 401, "UNAUTHENTICATED", "请重新登录")
			return
		}
		if err != nil {
			httpapi.Fail(w, r, 503, "AUTH_UNAVAILABLE", "登录状态暂时无法验证")
			return
		}
		if admin && p.Role != "admin" {
			httpapi.Fail(w, r, 403, "FORBIDDEN", "无操作权限")
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" && r.Method != "OPTIONS" {
			token := r.Header.Get("X-CSRF-Token")
			hash := sha256.Sum256([]byte(token))
			if token == "" || len(token) > 256 || subtle.ConstantTimeCompare(hash[:], p.CSRFHash) != 1 {
				httpapi.Fail(w, r, 403, "CSRF_REJECTED", "请求验证失败")
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	})
}
