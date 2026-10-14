package settings

import (
	"errors"
	"strings"
	"testing"
)

func intPtr(value int) *int       { return &value }
func strPtr(value string) *string { return &value }

func TestPatchEmptyAndFields(t *testing.T) {
	if !(Patch{}).Empty() {
		t.Fatal("an empty patch reported a change")
	}
	patch := Patch{CaptchaMaxAttempts: intPtr(4), SMTPHost: strPtr("smtp.example.test")}
	if patch.Empty() {
		t.Fatal("a populated patch reported no change")
	}
	// The field list feeds the audit reason, so it must name exactly what changed and nothing else.
	fields := strings.Join(patch.Fields(), ",")
	if fields != "smtp_host,captcha_max_attempts" {
		t.Fatalf("fields=%q", fields)
	}
	if strings.Contains(fields, "smtp_password") {
		t.Fatal("an unrelated password change was listed")
	}
	passwordPatch := Patch{SMTPPassword: strPtr("secret")}
	if got := strings.Join(passwordPatch.Fields(), ","); got != "smtp_password" {
		t.Fatalf("password field=%q", got)
	}
}

func TestPatchValidationRejectsOutOfRange(t *testing.T) {
	cases := map[string]struct {
		patch Patch
		field string
	}{
		"captcha ttl below minimum":      {Patch{CaptchaTTLSeconds: intPtr(CaptchaTTLMin - 1)}, "captcha_ttl_seconds"},
		"captcha ttl above maximum":      {Patch{CaptchaTTLSeconds: intPtr(CaptchaTTLMax + 1)}, "captcha_ttl_seconds"},
		"captcha attempts zero":          {Patch{CaptchaMaxAttempts: intPtr(0)}, "captcha_max_attempts"},
		"captcha attempts above maximum": {Patch{CaptchaMaxAttempts: intPtr(CaptchaAttemptsMax + 1)}, "captcha_max_attempts"},
		"email ttl out of range":         {Patch{EmailCodeTTLSeconds: intPtr(EmailCodeTTLMax + 1)}, "email_code_ttl_seconds"},
		"email attempts out of range":    {Patch{EmailCodeMaxAttempts: intPtr(EmailCodeAttemptsMax + 1)}, "email_code_max_attempts"},
		"resend interval too small":      {Patch{EmailCodeResendIntervalSeconds: intPtr(EmailCodeResendMin - 1)}, "email_code_resend_interval_seconds"},
		"email hourly limit too high":    {Patch{EmailCodeEmailHourlyLimit: intPtr(EmailCodeEmailMax + 1)}, "email_code_email_hourly_limit"},
		"ip hourly limit too high":       {Patch{EmailCodeIPHourlyLimit: intPtr(EmailCodeIPLimitMax + 1)}, "email_code_ip_hourly_limit"},
		"login threshold too low":        {Patch{LoginCaptchaFailureThreshold: intPtr(0)}, "login_captcha_failure_threshold"},
		"unknown security mode":          {Patch{Security: strPtr("plain")}, "smtp_security"},
		"empty security mode":            {Patch{Security: strPtr("")}, "smtp_security"},
		"port zero":                      {Patch{SMTPPort: intPtr(0)}, "smtp_port"},
		"port above range":               {Patch{SMTPPort: intPtr(65536)}, "smtp_port"},
		"malformed from address":         {Patch{SMTPFromAddress: strPtr("not-an-address")}, "smtp_from_address"},
		"display name form":              {Patch{SMTPFromAddress: strPtr("Name <a@example.test>")}, "smtp_from_address"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := tc.patch.validate()
			var invalid *ValidationError
			if !errors.As(err, &invalid) {
				t.Fatalf("error=%v want a ValidationError", err)
			}
			if invalid.Field != tc.field {
				t.Fatalf("field=%q want=%q", invalid.Field, tc.field)
			}
		})
	}
}

func TestPatchValidationAcceptsBoundaries(t *testing.T) {
	// Boundary values must be accepted: they are exactly what the CHECK constraints allow, so a
	// rejection here would make a documented setting impossible to save.
	patch := Patch{
		CaptchaTTLSeconds:              intPtr(CaptchaTTLMin),
		CaptchaMaxAttempts:             intPtr(CaptchaAttemptsMax),
		EmailCodeTTLSeconds:            intPtr(EmailCodeTTLMax),
		EmailCodeMaxAttempts:           intPtr(EmailCodeAttemptsMin),
		EmailCodeResendIntervalSeconds: intPtr(EmailCodeResendMax),
		EmailCodeEmailHourlyLimit:      intPtr(EmailCodeEmailLimitMin),
		EmailCodeIPHourlyLimit:         intPtr(EmailCodeIPLimitMin),
		LoginCaptchaFailureThreshold:   intPtr(LoginCaptchaMax),
		Security:                       strPtr("tls"),
		SMTPPort:                       intPtr(65535),
		SMTPFromAddress:                strPtr("no-reply@example.test"),
	}
	if err := patch.validate(); err != nil {
		t.Fatalf("boundary values were rejected: %v", err)
	}
}

func TestPatchClearSMTPPort(t *testing.T) {
	// Clearing is the only way to undo a mistyped port, because 0 is not an acceptable port value.
	clearOnly := Patch{ClearSMTPPort: true}
	if clearOnly.Empty() {
		t.Fatal("a clear-only patch reported no change")
	}
	if err := clearOnly.validate(); err != nil {
		t.Fatalf("clear-only patch was rejected: %v", err)
	}
	if got := strings.Join(clearOnly.Fields(), ","); got != "smtp_port" {
		t.Fatalf("fields=%q want smtp_port", got)
	}
	// Setting and clearing the same field at once is contradictory and must be refused.
	both := Patch{SMTPPort: intPtr(587), ClearSMTPPort: true}
	var invalid *ValidationError
	if err := both.validate(); !errors.As(err, &invalid) || invalid.Field != "smtp_port" {
		t.Fatalf("contradictory patch was accepted: %v", err)
	}
}
