package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"opentalk/server/internal/platform/httpapi"
)

type fakeStore struct {
	p   Principal
	err error
}

func (s fakeStore) Lookup(context.Context, [32]byte) (Principal, error) { return s.p, s.err }
func TestSessionAndCSRFBoundaries(t *testing.T) {
	token := "csrf-for-session"
	hash := sha256.Sum256([]byte(token))
	p := Principal{UserID: 7, Role: "user", CSRFHash: hash[:]}
	tests := []struct {
		name, method, cookie, origin, csrf string
		admin                              bool
		err                                error
		want                               int
	}{
		{name: "missing cookie", method: "GET", want: 401},
		{name: "expired", method: "GET", cookie: strings.Repeat("x", 32), err: ErrNoSession, want: 401},
		{name: "database fails closed", method: "GET", cookie: strings.Repeat("x", 32), err: errors.New("db"), want: 503},
		{name: "user cannot administer", method: "GET", cookie: strings.Repeat("x", 32), admin: true, want: 403},
		{name: "read", method: "GET", cookie: strings.Repeat("x", 32), want: 204},
		{name: "cross site", method: "POST", cookie: strings.Repeat("x", 32), origin: "https://evil.example", csrf: token, want: 403},
		{name: "no origin", method: "POST", cookie: strings.Repeat("x", 32), csrf: token, want: 403},
		{name: "no csrf", method: "POST", cookie: strings.Repeat("x", 32), origin: "http://localhost:5173", want: 403},
		{name: "wrong csrf", method: "POST", cookie: strings.Repeat("x", 32), origin: "http://localhost:5173", csrf: "other-session", want: 403},
		{name: "authorized write", method: "POST", cookie: strings.Repeat("x", 32), origin: "http://localhost:5173", csrf: token, want: 204},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				if got, ok := FromContext(r.Context()); !ok || got.UserID != 7 {
					t.Error("missing principal")
				}
				w.WriteHeader(204)
			})
			handler := httpapi.Middleware("http://localhost:5173", slog.New(slog.NewTextHandler(io.Discard, nil)), Require(fakeStore{p, tt.err}, tt.admin, next))
			req := httptest.NewRequest(tt.method, "http://localhost/api/test", nil)
			if tt.cookie != "" {
				req.AddCookie(&http.Cookie{Name: "opentalk_session", Value: tt.cookie})
			}
			req.Header.Set("Origin", tt.origin)
			req.Header.Set("X-CSRF-Token", tt.csrf)
			res := httptest.NewRecorder()
			handler.ServeHTTP(res, req)
			if res.Code != tt.want {
				t.Fatalf("status=%d want=%d", res.Code, tt.want)
			}
			if called != (tt.want == 204) {
				t.Fatal("protected handler invoked incorrectly")
			}
		})
	}
}
