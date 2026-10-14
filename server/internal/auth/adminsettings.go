package auth

import (
	"errors"
	"net/http"
	"strings"

	"opentalk/server/internal/platform/httpapi"
	"opentalk/server/internal/settings"
)

// adminSettingsView never carries the SMTP password, only whether one is stored. Unconfigured SMTP
// fields are omitted so the client can distinguish "not set" from an empty value.
type adminSettingsView struct {
	SMTPHost                       string `json:"smtp_host,omitempty"`
	SMTPPort                       int    `json:"smtp_port,omitempty"`
	Security                       string `json:"smtp_security"`
	SMTPUsername                   string `json:"smtp_username,omitempty"`
	SMTPPasswordSet                bool   `json:"smtp_password_set"`
	SMTPFromAddress                string `json:"smtp_from_address,omitempty"`
	SMTPFromName                   string `json:"smtp_from_name,omitempty"`
	CaptchaTTLSeconds              int    `json:"captcha_ttl_seconds"`
	CaptchaMaxAttempts             int    `json:"captcha_max_attempts"`
	EmailCodeTTLSeconds            int    `json:"email_code_ttl_seconds"`
	EmailCodeMaxAttempts           int    `json:"email_code_max_attempts"`
	EmailCodeResendIntervalSeconds int    `json:"email_code_resend_interval_seconds"`
	EmailCodeEmailHourlyLimit      int    `json:"email_code_email_hourly_limit"`
	EmailCodeIPHourlyLimit         int    `json:"email_code_ip_hourly_limit"`
	LoginCaptchaFailureThreshold   int    `json:"login_captcha_failure_threshold"`
	MediaMaxBytes                  int    `json:"media_max_bytes"`
	MediaMaxDimension              int    `json:"media_max_dimension"`
	MediaMaxPerContent             int    `json:"media_max_per_content"`
	MediaPendingTTLHours           int    `json:"media_pending_ttl_hours"`
	MediaOrphanCleanupDays         int    `json:"media_orphan_cleanup_days"`
	UpdatedAt                      string `json:"updated_at"`
}

func toAdminSettingsView(s settings.Settings) adminSettingsView {
	return adminSettingsView{
		SMTPHost:                       s.SMTPHost,
		SMTPPort:                       s.SMTPPort,
		Security:                       s.Security,
		SMTPUsername:                   s.SMTPUsername,
		SMTPPasswordSet:                s.SMTPPasswordSet,
		SMTPFromAddress:                s.SMTPFromAddress,
		SMTPFromName:                   s.SMTPFromName,
		CaptchaTTLSeconds:              s.CaptchaTTLSeconds,
		CaptchaMaxAttempts:             s.CaptchaMaxAttempts,
		EmailCodeTTLSeconds:            s.EmailCodeTTLSeconds,
		EmailCodeMaxAttempts:           s.EmailCodeMaxAttempts,
		EmailCodeResendIntervalSeconds: s.EmailCodeResendIntervalSeconds,
		EmailCodeEmailHourlyLimit:      s.EmailCodeEmailHourlyLimit,
		EmailCodeIPHourlyLimit:         s.EmailCodeIPHourlyLimit,
		LoginCaptchaFailureThreshold:   s.LoginCaptchaFailureThreshold,
		MediaMaxBytes:                  s.MediaMaxBytes,
		MediaMaxDimension:              s.MediaMaxDimension,
		MediaMaxPerContent:             s.MediaMaxPerContent,
		MediaPendingTTLHours:           s.MediaPendingTTLHours,
		MediaOrphanCleanupDays:         s.MediaOrphanCleanupDays,
		UpdatedAt:                      formatTime(s.UpdatedAt),
	}
}

type adminUpdateSettingsRequest struct {
	SMTPHost                       *string `json:"smtp_host"`
	SMTPPort                       *int    `json:"smtp_port"`
	Security                       *string `json:"smtp_security"`
	SMTPUsername                   *string `json:"smtp_username"`
	SMTPPassword                   *string `json:"smtp_password"`
	SMTPFromAddress                *string `json:"smtp_from_address"`
	SMTPFromName                   *string `json:"smtp_from_name"`
	CaptchaTTLSeconds              *int    `json:"captcha_ttl_seconds"`
	CaptchaMaxAttempts             *int    `json:"captcha_max_attempts"`
	EmailCodeTTLSeconds            *int    `json:"email_code_ttl_seconds"`
	EmailCodeMaxAttempts           *int    `json:"email_code_max_attempts"`
	EmailCodeResendIntervalSeconds *int    `json:"email_code_resend_interval_seconds"`
	EmailCodeEmailHourlyLimit      *int    `json:"email_code_email_hourly_limit"`
	EmailCodeIPHourlyLimit         *int    `json:"email_code_ip_hourly_limit"`
	LoginCaptchaFailureThreshold   *int    `json:"login_captcha_failure_threshold"`
	MediaMaxBytes                  *int    `json:"media_max_bytes"`
	MediaMaxDimension              *int    `json:"media_max_dimension"`
	MediaMaxPerContent             *int    `json:"media_max_per_content"`
	MediaPendingTTLHours           *int    `json:"media_pending_ttl_hours"`
	MediaOrphanCleanupDays         *int    `json:"media_orphan_cleanup_days"`
	Reason                         string  `json:"reason"`
}

// getAdminSettings returns the effective configuration. The password is replaced by a presence flag.
func (d Deps) getAdminSettings(w http.ResponseWriter, r *http.Request) {
	current, err := d.Settings.Load(r.Context())
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	httpapi.JSON(w, 200, toAdminSettingsView(current))
}

// updateAdminSettings applies only the submitted fields and writes an audit entry. The reason is
// mandatory because a settings change has no other trace, and the audit records field names rather
// than values so a password can never reach the log.
func (d Deps) updateAdminSettings(w http.ResponseWriter, r *http.Request) {
	var request adminUpdateSettingsRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	principal, ok := FromContext(r.Context())
	if !ok {
		httpapi.Fail(w, r, 401, "UNAUTHENTICATED", "请先登录")
		return
	}
	reason := strings.TrimSpace(request.Reason)
	if reason == "" {
		httpapi.Fail(w, r, 422, "REASON_REQUIRED", "请填写修改原因")
		return
	}
	patch := settings.Patch{
		SMTPHost:                       request.SMTPHost,
		SMTPPort:                       request.SMTPPort,
		Security:                       request.Security,
		SMTPUsername:                   request.SMTPUsername,
		SMTPPassword:                   request.SMTPPassword,
		SMTPFromAddress:                request.SMTPFromAddress,
		SMTPFromName:                   request.SMTPFromName,
		CaptchaTTLSeconds:              request.CaptchaTTLSeconds,
		CaptchaMaxAttempts:             request.CaptchaMaxAttempts,
		EmailCodeTTLSeconds:            request.EmailCodeTTLSeconds,
		EmailCodeMaxAttempts:           request.EmailCodeMaxAttempts,
		EmailCodeResendIntervalSeconds: request.EmailCodeResendIntervalSeconds,
		EmailCodeEmailHourlyLimit:      request.EmailCodeEmailHourlyLimit,
		EmailCodeIPHourlyLimit:         request.EmailCodeIPHourlyLimit,
		LoginCaptchaFailureThreshold:   request.LoginCaptchaFailureThreshold,
		MediaMaxBytes:                  request.MediaMaxBytes,
		MediaMaxDimension:              request.MediaMaxDimension,
		MediaMaxPerContent:             request.MediaMaxPerContent,
		MediaPendingTTLHours:           request.MediaPendingTTLHours,
		MediaOrphanCleanupDays:         request.MediaOrphanCleanupDays,
	}
	if patch.Empty() {
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "至少需要修改一项设置")
		return
	}
	updated, err := d.Settings.Update(r.Context(), principal.UserID, patch, reason)
	var invalid *settings.ValidationError
	if errors.As(err, &invalid) {
		httpapi.Fail(w, r, 422, "SETTING_INVALID", "设置项超出允许范围："+invalid.Field)
		return
	}
	if errors.Is(err, settings.ErrNoSettingsRow) {
		d.failDependency(w, r, err)
		return
	}
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	httpapi.JSON(w, 200, toAdminSettingsView(updated))
}
