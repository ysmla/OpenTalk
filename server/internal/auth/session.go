package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// SessionCookieName is the cookie the session middleware and this service share.
const SessionCookieName = "opentalk_session"

// SessionTTL is the absolute session lifetime. Sessions deliberately do not slide, so a stolen
// cookie cannot be kept alive indefinitely by continued use.
const SessionTTL = 30 * 24 * time.Hour

var ErrSessionUnknown = errors.New("session unknown")

// Sessions issues, derives and revokes session credentials. Only digests are stored, so a database
// dump does not hand over usable cookies or CSRF tokens.
type Sessions struct {
	DB      *sql.DB
	CSRFKey []byte
	Secure  bool
}

// Lookup implements SessionStore so the session middleware shares this configuration instead of
// building its own store.
func (s Sessions) Lookup(ctx context.Context, hash [32]byte) (Principal, error) {
	return SQLSessions{DB: s.DB}.Lookup(ctx, hash)
}

func tokenDigest(value string) [32]byte { return sha256.Sum256([]byte(value)) }

// CSRFToken derives the CSRF token from the raw session credential and a server key. Deriving
// instead of storing a random token lets several tabs read the same value without rotating it.
func (s Sessions) CSRFToken(rawToken string) string {
	mac := hmac.New(sha256.New, s.CSRFKey)
	mac.Write([]byte(rawToken))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func newSessionCredential() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("cannot read randomness: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// Create stores a new session and returns the raw cookie credential and its CSRF token. The caller
// is responsible for sending the cookie; the raw value must never be logged.
func (s Sessions) Create(ctx context.Context, userID uint64) (string, string, error) {
	raw, err := newSessionCredential()
	if err != nil {
		return "", "", err
	}
	csrf := s.CSRFToken(raw)
	rawDigest, csrfDigest := tokenDigest(raw), tokenDigest(csrf)
	_, err = s.DB.ExecContext(ctx, `INSERT INTO sessions (user_id,token_hash,csrf_token_hash,expires_at)
 VALUES (?,?,?,DATE_ADD(UTC_TIMESTAMP(6), INTERVAL ? SECOND))`,
		userID, rawDigest[:], csrfDigest[:], int(SessionTTL.Seconds()))
	if err != nil {
		return "", "", fmt.Errorf("cannot create session: %w", err)
	}
	return raw, csrf, nil
}

// CSRFFor returns the token for a live session, so a page reload can recover a token the browser
// kept only in memory. An unknown or revoked session reports ErrSessionUnknown.
func (s Sessions) CSRFFor(ctx context.Context, rawToken string) (string, error) {
	digest := tokenDigest(rawToken)
	var exists int
	err := s.DB.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE token_hash=? AND revoked_at IS NULL
 AND expires_at>UTC_TIMESTAMP(6)`, digest[:]).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrSessionUnknown
	}
	if err != nil {
		return "", err
	}
	return s.CSRFToken(rawToken), nil
}

// Revoke ends the presented session. A missing session is not an error: logging out twice must not
// look like a failure to the user.
func (s Sessions) Revoke(ctx context.Context, rawToken string) error {
	digest := tokenDigest(rawToken)
	_, err := s.DB.ExecContext(ctx, `UPDATE sessions SET revoked_at=UTC_TIMESTAMP(6)
 WHERE token_hash=? AND revoked_at IS NULL`, digest[:])
	return err
}

// RevokeAll ends every live session of a user. Password reset and banning both rely on this.
func (s Sessions) RevokeAll(ctx context.Context, userID uint64) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE sessions SET revoked_at=UTC_TIMESTAMP(6)
 WHERE user_id=? AND revoked_at IS NULL`, userID)
	return err
}

// SetCookie sends the session cookie. Secure follows the configured origin so a local http origin
// still works while production gets the attribute.
func (s Sessions) SetCookie(w http.ResponseWriter, rawToken string) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    rawToken,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.Secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(SessionTTL.Seconds()),
	})
}

// ClearCookie removes the session cookie. MaxAge<0 tells the browser to delete it immediately.
func (s Sessions) ClearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   s.Secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// RawFromRequest reads the session credential. The length check keeps obviously bogus values from
// reaching the database.
func RawFromRequest(r *http.Request) (string, bool) {
	cookie, err := r.Cookie(SessionCookieName)
	if err != nil || len(cookie.Value) < 32 || len(cookie.Value) > 256 {
		return "", false
	}
	return cookie.Value, true
}
