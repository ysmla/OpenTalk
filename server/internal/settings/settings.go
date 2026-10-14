// Package settings loads and updates the single row of administrator-editable configuration.
//
// Settings are read from the database on every request rather than cached in the process: an
// administrator changing a limit expects the next request to honour it, and a one-row primary-key
// lookup is cheap enough for this service.
package settings

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"opentalk/server/internal/platform/secretbox"
)

// Bounds mirror the CHECK constraints in migrations 000002 and 000003 and the OpenAPI schema.
// Validating here as well turns a constraint violation from an opaque 500 into a field-level 422.
const (
	CaptchaTTLMin, CaptchaTTLMax               = 60, 1800
	CaptchaAttemptsMin, CaptchaAttemptsMax     = 1, 10
	EmailCodeTTLMin, EmailCodeTTLMax           = 60, 3600
	EmailCodeAttemptsMin, EmailCodeAttemptsMax = 1, 10
	EmailCodeResendMin, EmailCodeResendMax     = 10, 600
	EmailCodeEmailLimitMin, EmailCodeEmailMax  = 1, 100
	EmailCodeIPLimitMin, EmailCodeIPLimitMax   = 1, 1000
	LoginCaptchaThresholdMin, LoginCaptchaMax  = 1, 50
	MediaMaxBytesMin, MediaMaxBytesMax         = 65536, 104857600
	MediaDimensionMin, MediaDimensionMax       = 256, 8192
	MediaPerContentMin, MediaPerContentMax     = 1, 30
	MediaPendingTTLMin, MediaPendingTTLMax     = 1, 720
	MediaOrphanDaysMin, MediaOrphanDaysMax     = 1, 90
)

// SecurityModes are the accepted SMTP transport modes.
var SecurityModes = []string{"none", "starttls", "tls"}

// Settings is the resolved configuration. Empty SMTP strings mean "not configured"; the database
// stores them as NULL.
type Settings struct {
	SMTPHost                       string
	SMTPPort                       int
	Security                       string
	SMTPUsername                   string
	SMTPPassword                   string
	SMTPPasswordSet                bool
	SMTPFromAddress                string
	SMTPFromName                   string
	CaptchaTTLSeconds              int
	CaptchaMaxAttempts             int
	EmailCodeTTLSeconds            int
	EmailCodeMaxAttempts           int
	EmailCodeResendIntervalSeconds int
	EmailCodeEmailHourlyLimit      int
	EmailCodeIPHourlyLimit         int
	LoginCaptchaFailureThreshold   int
	MediaMaxBytes                  int
	MediaMaxDimension              int
	MediaMaxPerContent             int
	MediaPendingTTLHours           int
	MediaOrphanCleanupDays         int
	UpdatedAt                      time.Time
}

// MailerReady reports whether enough SMTP configuration exists to attempt a delivery. Callers must
// check it before building a mailer, so a missing host never becomes a connection attempt.
func (s Settings) MailerReady() bool {
	return s.SMTPHost != "" && s.SMTPPort > 0 && s.SMTPFromAddress != ""
}

// Patch carries only the fields an administrator submitted. A nil field means "leave unchanged".
// An empty SMTPPassword string clears the stored password, which is how an unauthenticated relay
// is configured.
type Patch struct {
	SMTPHost                       *string
	SMTPPort                       *int
	Security                       *string
	SMTPUsername                   *string
	SMTPPassword                   *string
	SMTPFromAddress                *string
	SMTPFromName                   *string
	CaptchaTTLSeconds              *int
	CaptchaMaxAttempts             *int
	EmailCodeTTLSeconds            *int
	EmailCodeMaxAttempts           *int
	EmailCodeResendIntervalSeconds *int
	EmailCodeEmailHourlyLimit      *int
	EmailCodeIPHourlyLimit         *int
	LoginCaptchaFailureThreshold   *int
	MediaMaxBytes                  *int
	MediaMaxDimension              *int
	MediaMaxPerContent             *int
	MediaPendingTTLHours           *int
	MediaOrphanCleanupDays         *int
	// ClearSMTPPort sets smtp_port to NULL. It needs an explicit flag because 0 is not a valid port,
	// and overloading 0 would make a typo indistinguishable from a deliberate clear.
	ClearSMTPPort bool
}

// Empty reports whether the patch would change nothing.
func (p Patch) Empty() bool {
	return p.SMTPHost == nil && p.SMTPPort == nil && p.Security == nil && p.SMTPUsername == nil &&
		p.SMTPPassword == nil && p.SMTPFromAddress == nil && p.SMTPFromName == nil &&
		p.CaptchaTTLSeconds == nil && p.CaptchaMaxAttempts == nil && p.EmailCodeTTLSeconds == nil &&
		p.EmailCodeMaxAttempts == nil && p.EmailCodeResendIntervalSeconds == nil &&
		p.EmailCodeEmailHourlyLimit == nil && p.EmailCodeIPHourlyLimit == nil &&
		p.LoginCaptchaFailureThreshold == nil && p.MediaMaxBytes == nil &&
		p.MediaMaxDimension == nil && p.MediaMaxPerContent == nil &&
		p.MediaPendingTTLHours == nil && p.MediaOrphanCleanupDays == nil && !p.ClearSMTPPort
}

// Fields lists the changed field names for the audit reason. It never includes values, so a
// password can never reach the audit log.
func (p Patch) Fields() []string {
	var names []string
	add := func(name string, set bool) {
		if set {
			names = append(names, name)
		}
	}
	add("smtp_host", p.SMTPHost != nil)
	add("smtp_port", p.SMTPPort != nil || p.ClearSMTPPort)
	add("smtp_security", p.Security != nil)
	add("smtp_username", p.SMTPUsername != nil)
	add("smtp_password", p.SMTPPassword != nil)
	add("smtp_from_address", p.SMTPFromAddress != nil)
	add("smtp_from_name", p.SMTPFromName != nil)
	add("captcha_ttl_seconds", p.CaptchaTTLSeconds != nil)
	add("captcha_max_attempts", p.CaptchaMaxAttempts != nil)
	add("email_code_ttl_seconds", p.EmailCodeTTLSeconds != nil)
	add("email_code_max_attempts", p.EmailCodeMaxAttempts != nil)
	add("email_code_resend_interval_seconds", p.EmailCodeResendIntervalSeconds != nil)
	add("email_code_email_hourly_limit", p.EmailCodeEmailHourlyLimit != nil)
	add("email_code_ip_hourly_limit", p.EmailCodeIPHourlyLimit != nil)
	add("login_captcha_failure_threshold", p.LoginCaptchaFailureThreshold != nil)
	add("media_max_bytes", p.MediaMaxBytes != nil)
	add("media_max_dimension", p.MediaMaxDimension != nil)
	add("media_max_per_content", p.MediaMaxPerContent != nil)
	add("media_pending_ttl_hours", p.MediaPendingTTLHours != nil)
	add("media_orphan_cleanup_days", p.MediaOrphanCleanupDays != nil)
	return names
}

// ValidationError names the rejected field so the HTTP layer can answer 422 without re-deriving
// the rule or echoing the submitted value.
type ValidationError struct{ Field string }

func (e *ValidationError) Error() string { return "invalid setting: " + e.Field }

// Store reads and writes the settings row and encrypts the SMTP password with a purpose-bound key.
type Store struct {
	DB  *sql.DB
	Key []byte
}

type scanner interface{ Scan(...any) error }

func read(row scanner) (Settings, []byte, error) {
	var s Settings
	var host, security, username, fromAddress, fromName sql.NullString
	var port sql.NullInt64
	var password []byte
	err := row.Scan(&host, &port, &security, &username, &password, &fromAddress, &fromName,
		&s.CaptchaTTLSeconds, &s.CaptchaMaxAttempts, &s.EmailCodeTTLSeconds, &s.EmailCodeMaxAttempts,
		&s.EmailCodeResendIntervalSeconds, &s.EmailCodeEmailHourlyLimit, &s.EmailCodeIPHourlyLimit,
		&s.LoginCaptchaFailureThreshold, &s.MediaMaxBytes, &s.MediaMaxDimension,
		&s.MediaMaxPerContent, &s.MediaPendingTTLHours, &s.MediaOrphanCleanupDays, &s.UpdatedAt)
	if err != nil {
		return s, nil, err
	}
	s.SMTPHost, s.SMTPUsername = host.String, username.String
	s.SMTPPort = int(port.Int64)
	s.Security, s.SMTPFromAddress, s.SMTPFromName = security.String, fromAddress.String, fromName.String
	s.SMTPPasswordSet = len(password) > 0
	return s, password, nil
}

const selectColumns = `smtp_host,smtp_port,smtp_security,smtp_username,smtp_password_ciphertext,
 smtp_from_address,smtp_from_name,captcha_ttl_seconds,captcha_max_attempts,email_code_ttl_seconds,
 email_code_max_attempts,email_code_resend_interval_seconds,email_code_email_hourly_limit,
 email_code_ip_hourly_limit,login_captcha_failure_threshold,media_max_bytes,media_max_dimension,
 media_max_per_content,media_pending_ttl_hours,media_orphan_cleanup_days,updated_at`

// Load returns the current settings with the SMTP password decrypted. A decryption failure is
// reported rather than silently treated as "no password", so a rotated APP_SECRET surfaces clearly
// instead of quietly disabling authentication.
func (s Store) Load(ctx context.Context) (Settings, error) {
	current, ciphertext, err := s.loadRow(ctx, s.DB)
	if err != nil {
		return current, err
	}
	if len(ciphertext) > 0 {
		password, err := secretbox.Open(s.Key, ciphertext)
		if err != nil {
			return current, fmt.Errorf("cannot read stored SMTP password: %w", err)
		}
		current.SMTPPassword = string(password)
	}
	return current, nil
}

func (s Store) loadRow(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (Settings, []byte, error) {
	return read(q.QueryRowContext(ctx, `SELECT `+selectColumns+` FROM settings WHERE id=1`))
}

var ErrNoSettingsRow = errors.New("settings row is missing")

// Update applies the patch, validates it, records an audit entry, and returns the new settings. The
// audit reason carries the administrator's text plus the changed field names only.
func (s Store) Update(ctx context.Context, actorID uint64, patch Patch, reason string) (Settings, error) {
	if patch.Empty() {
		return Settings{}, &ValidationError{Field: "patch"}
	}
	if err := patch.validate(); err != nil {
		return Settings{}, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Settings{}, err
	}
	defer func() { _ = tx.Rollback() }()

	// Lock the single row so two administrators cannot interleave read-modify-write.
	current, ciphertext, err := read(tx.QueryRowContext(ctx, `SELECT `+selectColumns+` FROM settings WHERE id=1 FOR UPDATE`))
	if errors.Is(err, sql.ErrNoRows) {
		return Settings{}, ErrNoSettingsRow
	}
	if err != nil {
		return Settings{}, err
	}

	assign := func(dst *string, src *string) {
		if src != nil {
			*dst = strings.TrimSpace(*src)
		}
	}
	assign(&current.SMTPHost, patch.SMTPHost)
	assign(&current.SMTPUsername, patch.SMTPUsername)
	assign(&current.SMTPFromAddress, patch.SMTPFromAddress)
	assign(&current.SMTPFromName, patch.SMTPFromName)
	if patch.Security != nil {
		current.Security = *patch.Security
	}
	if patch.SMTPPort != nil {
		current.SMTPPort = *patch.SMTPPort
	} else if patch.ClearSMTPPort {
		current.SMTPPort = 0
	}
	if patch.CaptchaTTLSeconds != nil {
		current.CaptchaTTLSeconds = *patch.CaptchaTTLSeconds
	}
	if patch.CaptchaMaxAttempts != nil {
		current.CaptchaMaxAttempts = *patch.CaptchaMaxAttempts
	}
	if patch.EmailCodeTTLSeconds != nil {
		current.EmailCodeTTLSeconds = *patch.EmailCodeTTLSeconds
	}
	if patch.EmailCodeMaxAttempts != nil {
		current.EmailCodeMaxAttempts = *patch.EmailCodeMaxAttempts
	}
	if patch.EmailCodeResendIntervalSeconds != nil {
		current.EmailCodeResendIntervalSeconds = *patch.EmailCodeResendIntervalSeconds
	}
	if patch.EmailCodeEmailHourlyLimit != nil {
		current.EmailCodeEmailHourlyLimit = *patch.EmailCodeEmailHourlyLimit
	}
	if patch.EmailCodeIPHourlyLimit != nil {
		current.EmailCodeIPHourlyLimit = *patch.EmailCodeIPHourlyLimit
	}
	if patch.LoginCaptchaFailureThreshold != nil {
		current.LoginCaptchaFailureThreshold = *patch.LoginCaptchaFailureThreshold
	}
	if patch.MediaMaxBytes != nil {
		current.MediaMaxBytes = *patch.MediaMaxBytes
	}
	if patch.MediaMaxDimension != nil {
		current.MediaMaxDimension = *patch.MediaMaxDimension
	}
	if patch.MediaMaxPerContent != nil {
		current.MediaMaxPerContent = *patch.MediaMaxPerContent
	}
	if patch.MediaPendingTTLHours != nil {
		current.MediaPendingTTLHours = *patch.MediaPendingTTLHours
	}
	if patch.MediaOrphanCleanupDays != nil {
		current.MediaOrphanCleanupDays = *patch.MediaOrphanCleanupDays
	}

	if patch.SMTPPassword != nil {
		if *patch.SMTPPassword == "" {
			ciphertext = nil
		} else {
			sealed, err := secretbox.Seal(s.Key, []byte(*patch.SMTPPassword))
			if err != nil {
				return Settings{}, err
			}
			ciphertext = sealed
		}
		current.SMTPPasswordSet = len(ciphertext) > 0
	}

	if _, err := tx.ExecContext(ctx, `UPDATE settings SET
 smtp_host=?,smtp_port=?,smtp_security=?,smtp_username=?,smtp_password_ciphertext=?,
 smtp_from_address=?,smtp_from_name=?,captcha_ttl_seconds=?,captcha_max_attempts=?,
 email_code_ttl_seconds=?,email_code_max_attempts=?,email_code_resend_interval_seconds=?,
 email_code_email_hourly_limit=?,email_code_ip_hourly_limit=?,login_captcha_failure_threshold=?,
 media_max_bytes=?,media_max_dimension=?,media_max_per_content=?,media_pending_ttl_hours=?,
 media_orphan_cleanup_days=?,
 updated_by=?
 WHERE id=1`,
		nullString(current.SMTPHost), nullInt(current.SMTPPort), current.Security,
		nullString(current.SMTPUsername), nullBytes(ciphertext),
		nullString(current.SMTPFromAddress), current.SMTPFromName,
		current.CaptchaTTLSeconds, current.CaptchaMaxAttempts, current.EmailCodeTTLSeconds,
		current.EmailCodeMaxAttempts, current.EmailCodeResendIntervalSeconds,
		current.EmailCodeEmailHourlyLimit, current.EmailCodeIPHourlyLimit,
		current.LoginCaptchaFailureThreshold, current.MediaMaxBytes, current.MediaMaxDimension,
		current.MediaMaxPerContent, current.MediaPendingTTLHours, current.MediaOrphanCleanupDays,
		actorIDArg(actorID)); err != nil {
		return Settings{}, err
	}

	// Audit reuses moderation_actions so administrators see settings changes in the same trail as
	// moderation, and the reason deliberately lists field names instead of values.
	//
	// A command-line change has no session and therefore no actor; moderation_actions.actor_id is a
	// required reference to a real account, so such a change is applied without an audit row and
	// updated_by stays NULL. The operator sees the change confirmed on the terminal instead.
	if actorID > 0 {
		auditReason := reason + "（变更字段：" + strings.Join(patch.Fields(), ", ") + "）"
		if _, err := tx.ExecContext(ctx, `INSERT INTO moderation_actions (actor_id,action,reason) VALUES (?,?,?)`,
			actorID, "update_settings", auditReason); err != nil {
			return Settings{}, err
		}
	}

	if err := tx.Commit(); err != nil {
		return Settings{}, err
	}
	return s.Load(ctx)
}

func (p Patch) validate() error {
	within := func(field string, value *int, min, max int) error {
		if value == nil {
			return nil
		}
		if *value < min || *value > max {
			return &ValidationError{Field: field}
		}
		return nil
	}
	if p.Security != nil {
		valid := false
		for _, mode := range SecurityModes {
			if *p.Security == mode {
				valid = true
			}
		}
		if !valid {
			return &ValidationError{Field: "smtp_security"}
		}
	}
	if p.SMTPPort != nil && p.ClearSMTPPort {
		// Setting and clearing the same field at once has no meaning.
		return &ValidationError{Field: "smtp_port"}
	}
	if p.SMTPPort != nil && (*p.SMTPPort < 1 || *p.SMTPPort > 65535) {
		return &ValidationError{Field: "smtp_port"}
	}
	if p.SMTPFromAddress != nil {
		address := strings.TrimSpace(*p.SMTPFromAddress)
		if address != "" {
			parsed, err := mail.ParseAddress(address)
			if err != nil || parsed.Address != address || strings.ContainsAny(address, "<>") {
				return &ValidationError{Field: "smtp_from_address"}
			}
		}
	}
	for _, check := range []struct {
		field string
		value *int
		min   int
		max   int
	}{
		{"captcha_ttl_seconds", p.CaptchaTTLSeconds, CaptchaTTLMin, CaptchaTTLMax},
		{"captcha_max_attempts", p.CaptchaMaxAttempts, CaptchaAttemptsMin, CaptchaAttemptsMax},
		{"email_code_ttl_seconds", p.EmailCodeTTLSeconds, EmailCodeTTLMin, EmailCodeTTLMax},
		{"email_code_max_attempts", p.EmailCodeMaxAttempts, EmailCodeAttemptsMin, EmailCodeAttemptsMax},
		{"email_code_resend_interval_seconds", p.EmailCodeResendIntervalSeconds, EmailCodeResendMin, EmailCodeResendMax},
		{"email_code_email_hourly_limit", p.EmailCodeEmailHourlyLimit, EmailCodeEmailLimitMin, EmailCodeEmailMax},
		{"email_code_ip_hourly_limit", p.EmailCodeIPHourlyLimit, EmailCodeIPLimitMin, EmailCodeIPLimitMax},
		{"login_captcha_failure_threshold", p.LoginCaptchaFailureThreshold, LoginCaptchaThresholdMin, LoginCaptchaMax},
		{"media_max_bytes", p.MediaMaxBytes, MediaMaxBytesMin, MediaMaxBytesMax},
		{"media_max_dimension", p.MediaMaxDimension, MediaDimensionMin, MediaDimensionMax},
		{"media_max_per_content", p.MediaMaxPerContent, MediaPerContentMin, MediaPerContentMax},
		{"media_pending_ttl_hours", p.MediaPendingTTLHours, MediaPendingTTLMin, MediaPendingTTLMax},
		{"media_orphan_cleanup_days", p.MediaOrphanCleanupDays, MediaOrphanDaysMin, MediaOrphanDaysMax},
	} {
		if err := within(check.field, check.value, check.min, check.max); err != nil {
			return err
		}
	}
	return nil
}

func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullInt(value int) any {
	if value <= 0 {
		return nil
	}
	return value
}

func nullBytes(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return value
}

// actorIDArg maps "no actor" to NULL, because updated_by references a real account.
func actorIDArg(actorID uint64) any {
	if actorID == 0 {
		return nil
	}
	return actorID
}
