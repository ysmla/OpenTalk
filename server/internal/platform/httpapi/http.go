package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

type requestIDKey struct{}
type Error struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

func (e Error) Error() string { return e.Code }

// RequestID returns the correlation id Middleware assigned, so deeper layers can log the same id the
// client received without re-deriving it.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}
func JSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func Fail(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	id, _ := r.Context().Value(requestIDKey{}).(string)
	JSON(w, status, Error{code, message, id})
}

// Middleware requires exact-origin JSON writes, including future login endpoints.
// Cookie authentication additionally requires a session-bound CSRF token.
func Middleware(origin string, logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idBytes := make([]byte, 16)
		if _, err := rand.Read(idBytes); err != nil {
			Fail(w, r, 500, "INTERNAL_ERROR", "服务暂不可用")
			return
		}
		id := hex.EncodeToString(idBytes)
		r = r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id))
		w.Header().Set("X-Request-ID", id)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		start := time.Now()
		defer func() {
			if recover() != nil {
				logger.Error("handler panic", "request_id", id)
				Fail(w, r, 500, "INTERNAL_ERROR", "服务暂不可用")
			}
			// Do not log URLs, query strings, cookies, tokens, or request bodies.
			logger.Info("request", "request_id", id, "method", r.Method, "duration_ms", time.Since(start).Milliseconds())
		}()
		unsafe := r.Method != "GET" && r.Method != "HEAD" && r.Method != "OPTIONS"
		if unsafe && r.Header.Get("Origin") != origin {
			Fail(w, r, 403, "ORIGIN_REJECTED", "请求来源不受信任")
			return
		}
		// Same-origin reverse proxy deployment. Cross-origin requests get no CORS grants.
		next.ServeHTTP(w, r)
	})
}
