package auth

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"opentalk/server/internal/account"
	"opentalk/server/internal/mailer"
	"opentalk/server/internal/platform/httpapi"
	"opentalk/server/internal/settings"
)

// captchaPurposes is the closed set the contract allows. A challenge is bound to one of them, so a
// challenge fetched for login cannot be spent on registration.
var captchaPurposes = map[string]bool{"register": true, "reset_password": true, "login": true}

// emailCodePurposes is the subset that may send mail. "login" only ever uses the image challenge.
var emailCodePurposes = map[string]bool{"register": true, "reset_password": true}

// failDependency answers 503 and records the internal cause. The client only learns that a dependency
// is unavailable; the detail stays in the server log.
func (d Deps) failDependency(w http.ResponseWriter, r *http.Request, err error) {
	if d.Logger != nil {
		d.Logger.Error("dependency failure", "request_id", httpapi.RequestID(r.Context()), "error", err.Error())
	}
	httpapi.Fail(w, r, 503, "DEPENDENCY_UNAVAILABLE", "服务暂不可用")
}

// getCaptcha issues an image challenge. The purpose is part of the stored state, so an answer solved
// for one flow cannot be reused in another.
func (d Deps) getCaptcha(w http.ResponseWriter, r *http.Request) {
	purpose := r.URL.Query().Get("purpose")
	if !captchaPurposes[purpose] {
		httpapi.Fail(w, r, 422, "CAPTCHA_PURPOSE_INVALID", "验证码用途无效")
		return
	}
	current, err := d.Settings.Load(r.Context())
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	challenge, err := d.Challenges.CreateCaptcha(r.Context(), purpose, current.CaptchaTTLSeconds, current.CaptchaMaxAttempts)
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	httpapi.JSON(w, 200, captchaChallengeView{
		ChallengeID: challenge.ID,
		ImageURL:    "/api/v1/auth/captcha/" + challenge.ID + "/image",
		ExpiresAt:   formatTime(challenge.ExpiresAt),
	})
}

// getCaptchaImage serves the rendered challenge. The answer is never part of the response.
func (d Deps) getCaptchaImage(w http.ResponseWriter, r *http.Request) {
	image, err := d.Challenges.CaptchaImage(r.Context(), r.PathValue("challenge_id"))
	if errors.Is(err, ErrChallengeUnknown) {
		httpapi.Fail(w, r, 404, "NOT_FOUND", "验证码不存在或已过期")
		return
	}
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(image)
}

type sendEmailCodeRequest struct {
	Email              string `json:"email"`
	Purpose            string `json:"purpose"`
	CaptchaChallengeID string `json:"captcha_challenge_id"`
	CaptchaAnswer      string `json:"captcha_answer"`
}

// sendEmailCode verifies the image challenge first, then applies the sending ceilings, then delivers.
// A password reset for an unknown address takes the same path and returns the same body, but the
// stored code is unusable and no mail is sent, so the endpoint cannot enumerate accounts.
func (d Deps) sendEmailCode(w http.ResponseWriter, r *http.Request) {
	var request sendEmailCodeRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	if !emailCodePurposes[request.Purpose] {
		httpapi.Fail(w, r, 422, "PURPOSE_INVALID", "验证码用途无效")
		return
	}
	email, err := account.NormalizeEmail(request.Email)
	if err != nil {
		fieldFailure(w, r, err)
		return
	}
	current, err := d.Settings.Load(r.Context())
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	// Answering "required" is clearer than letting an absent challenge look like an expired one.
	if request.CaptchaChallengeID == "" || request.CaptchaAnswer == "" {
		httpapi.Fail(w, r, 422, "CAPTCHA_REQUIRED", "请先完成图形验证码")
		return
	}
	if err := d.Challenges.VerifyCaptcha(r.Context(), request.CaptchaChallengeID, request.Purpose, request.CaptchaAnswer); err != nil {
		challengeFailure(w, r, err)
		return
	}

	_, _, lookupErr := d.Store.UserByEmail(r.Context(), email)
	registered := lookupErr == nil
	if lookupErr != nil && !errors.Is(lookupErr, ErrUserNotFound) {
		d.failDependency(w, r, lookupErr)
		return
	}
	if request.Purpose == "register" && registered {
		httpapi.Fail(w, r, 409, "EMAIL_ALREADY_REGISTERED", "该邮箱已注册")
		return
	}

	ip := d.clientIP(r)
	retryAfter, err := d.Challenges.ReserveEmailSend(r.Context(), email, ip, EmailSendLimits{
		ResendIntervalSeconds: current.EmailCodeResendIntervalSeconds,
		EmailHourlyLimit:      current.EmailCodeEmailHourlyLimit,
		IPHourlyLimit:         current.EmailCodeIPHourlyLimit,
	})
	if errors.Is(err, ErrRateLimited) {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		httpapi.Fail(w, r, 429, "RATE_LIMITED", "请求过于频繁，请稍后再试")
		return
	}
	if err != nil {
		d.failDependency(w, r, err)
		return
	}

	code, err := randomDigits(6)
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	// Registration always has an unregistered address, so it is the flow that must send. A password
	// reset for an unknown address is the opposite: it answers identically but stores an unusable code
	// and sends nothing, which keeps the endpoint from enumerating accounts.
	sendable := false
	switch request.Purpose {
	case "register":
		sendable = !registered
	case "reset_password":
		sendable = registered
	}
	storedCode := code
	if !sendable {
		storedCode = ""
	}
	challenge, err := d.Challenges.CreateEmailCode(r.Context(), email, request.Purpose, storedCode,
		current.EmailCodeTTLSeconds, current.EmailCodeMaxAttempts, current.EmailCodeResendIntervalSeconds)
	if err != nil {
		d.failDependency(w, r, err)
		return
	}

	if sendable {
		if err := d.sendCode(r.Context(), current, email, request.Purpose, code); err != nil {
			// Undo the reservation so a transient outage does not lock the address out, and drop the
			// challenge so a code that never arrived is not left valid.
			d.Challenges.ReleaseEmailSend(r.Context(), email, ip)
			_ = d.Challenges.DiscardEmailCode(r.Context(), challenge.ID)
			if errors.Is(err, mailer.ErrNotConfigured) {
				httpapi.Fail(w, r, 503, "SMTP_NOT_CONFIGURED", "邮件服务尚未配置")
				return
			}
			d.failDependency(w, r, err)
			return
		}
	}

	httpapi.JSON(w, 202, emailCodeChallengeView{
		ChallengeID:       challenge.ID,
		ExpiresAt:         formatTime(challenge.ExpiresAt),
		RetryAfterSeconds: challenge.RetryAfterSeconds,
	})
}

// sendCode builds a sender from the current settings. The code never reaches a log.
func (d Deps) sendCode(ctx context.Context, current settings.Settings, email, purpose, code string) error {
	if !current.MailerReady() {
		return mailer.ErrNotConfigured
	}
	sender, err := mailer.New(mailer.Config{
		Host:        current.SMTPHost,
		Port:        current.SMTPPort,
		Security:    current.Security,
		Username:    current.SMTPUsername,
		Password:    current.SMTPPassword,
		FromAddress: current.SMTPFromAddress,
		FromName:    current.SMTPFromName,
	})
	if err != nil {
		return err
	}
	minutes := current.EmailCodeTTLSeconds / 60
	if minutes < 1 {
		minutes = 1
	}
	subject, action := "OpenTalk 注册验证码", "注册"
	if purpose == "reset_password" {
		subject, action = "OpenTalk 找回密码验证码", "找回密码"
	}
	body := "你的" + action + "验证码是 " + code + "，" + strconv.Itoa(minutes) + " 分钟内有效。\n若非本人操作，请忽略本邮件。\n"
	return sender.Send(ctx, mailer.Message{To: email, Subject: subject, Body: body})
}

type registerRequest struct {
	Email       string `json:"email"`
	ChallengeID string `json:"challenge_id"`
	EmailCode   string `json:"email_code"`
	Password    string `json:"password"`
	Nickname    string `json:"nickname"`
}

// register consumes the email code once and creates the account. Registration deliberately does not
// start a session: the contract says the user logs in afterwards.
func (d Deps) register(w http.ResponseWriter, r *http.Request) {
	var request registerRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	email, err := account.NormalizeEmail(request.Email)
	if err != nil {
		fieldFailure(w, r, err)
		return
	}
	if err := account.ValidatePassword(request.Password); err != nil {
		fieldFailure(w, r, err)
		return
	}
	nickname := strings.TrimSpace(request.Nickname)
	if err := account.ValidateNickname(nickname); err != nil {
		fieldFailure(w, r, err)
		return
	}
	// Check the duplicate before consuming the code, so a user who mistypes an address does not lose a
	// freshly requested code to a conflict they could have been told about first.
	if _, _, lookupErr := d.Store.UserByEmail(r.Context(), email); lookupErr == nil {
		httpapi.Fail(w, r, 409, "EMAIL_ALREADY_REGISTERED", "该邮箱已注册")
		return
	} else if !errors.Is(lookupErr, ErrUserNotFound) {
		d.failDependency(w, r, lookupErr)
		return
	}
	if err := d.Challenges.VerifyEmailCode(r.Context(), request.ChallengeID, email, "register", request.EmailCode); err != nil {
		emailCodeFailure(w, r, err)
		return
	}
	passwordHash, err := account.HashPassword(request.Password)
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	publicID, err := account.NewPublicID()
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	// The unique index is the real guard: two concurrent registrations for one address cannot both
	// succeed even though the pre-check above ran first.
	if _, err := d.Store.CreateUser(r.Context(), NewUser{
		Email:        email,
		PasswordHash: passwordHash,
		Nickname:     nickname,
		Bio:          "",
		PublicID:     publicID,
	}); err != nil {
		if errors.Is(err, ErrEmailTaken) {
			httpapi.Fail(w, r, 409, "EMAIL_ALREADY_REGISTERED", "该邮箱已注册")
			return
		}
		d.failDependency(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type loginRequest struct {
	Email              string `json:"email"`
	Password           string `json:"password"`
	CaptchaChallengeID string `json:"captcha_challenge_id"`
	CaptchaAnswer      string `json:"captcha_answer"`
}

// login verifies the password and starts a session. Every failure path answers the same 401, and an
// unknown address still runs a password hash so the response time does not reveal whether the account
// exists.
func (d Deps) login(w http.ResponseWriter, r *http.Request) {
	var request loginRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	if request.Password == "" {
		httpapi.Fail(w, r, 422, "PASSWORD_REQUIRED", "请输入密码")
		return
	}
	email, err := account.NormalizeEmail(request.Email)
	if err != nil {
		fieldFailure(w, r, err)
		return
	}
	current, err := d.Settings.Load(r.Context())
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	failures, err := d.Challenges.LoginFailures(r.Context(), email)
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	if failures >= current.LoginCaptchaFailureThreshold {
		if request.CaptchaChallengeID == "" || request.CaptchaAnswer == "" {
			httpapi.Fail(w, r, 422, "CAPTCHA_REQUIRED", "连续登录失败次数过多，请先完成图形验证码")
			return
		}
		if err := d.Challenges.VerifyCaptcha(r.Context(), request.CaptchaChallengeID, "login", request.CaptchaAnswer); err != nil {
			challengeFailure(w, r, err)
			return
		}
	}

	user, passwordHash, err := d.Store.UserByEmail(r.Context(), email)
	if errors.Is(err, ErrUserNotFound) {
		account.VerifyPassword(dummyPasswordHash(), request.Password)
		_, _ = d.Challenges.RecordLoginFailure(r.Context(), email)
		httpapi.Fail(w, r, 401, "INVALID_CREDENTIALS", "邮箱或密码不正确")
		return
	}
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	if user.Banned {
		// A ban must take effect on sessions that already exist, not only on the next login attempt.
		if err := d.Sessions.RevokeAll(r.Context(), user.ID); err != nil {
			d.failDependency(w, r, err)
			return
		}
		httpapi.Fail(w, r, 403, "ACCOUNT_BANNED", "账号已被封禁")
		return
	}
	if !account.VerifyPassword(passwordHash, request.Password) {
		_, _ = d.Challenges.RecordLoginFailure(r.Context(), email)
		httpapi.Fail(w, r, 401, "INVALID_CREDENTIALS", "邮箱或密码不正确")
		return
	}
	if err := d.Challenges.ClearLoginFailures(r.Context(), email); err != nil {
		d.failDependency(w, r, err)
		return
	}
	rawToken, csrfToken, err := d.Sessions.Create(r.Context(), user.ID)
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	d.Sessions.SetCookie(w, rawToken)
	httpapi.JSON(w, 200, authSessionView{User: toCurrentUser(user), CSRFToken: csrfToken})
}

// logout revokes the presented session and clears the cookie. Logging out twice is not an error.
func (d Deps) logout(w http.ResponseWriter, r *http.Request) {
	if raw, ok := RawFromRequest(r); ok {
		if err := d.Sessions.Revoke(r.Context(), raw); err != nil {
			d.failDependency(w, r, err)
			return
		}
	}
	d.Sessions.ClearCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

// getCSRF returns the token derived from the caller's own session. It does not rotate, so several
// tabs can read it without invalidating each other.
func (d Deps) getCSRF(w http.ResponseWriter, r *http.Request) {
	raw, ok := RawFromRequest(r)
	if !ok {
		httpapi.Fail(w, r, 401, "UNAUTHENTICATED", "请先登录")
		return
	}
	token, err := d.Sessions.CSRFFor(r.Context(), raw)
	if errors.Is(err, ErrSessionUnknown) {
		httpapi.Fail(w, r, 401, "UNAUTHENTICATED", "请重新登录")
		return
	}
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	httpapi.JSON(w, 200, csrfTokenView{CSRFToken: token})
}

type resetPasswordRequest struct {
	Email       string `json:"email"`
	ChallengeID string `json:"challenge_id"`
	EmailCode   string `json:"email_code"`
	NewPassword string `json:"new_password"`
}

// resetPassword consumes the code and revokes every existing session, because a password change must
// not leave an attacker's session alive.
func (d Deps) resetPassword(w http.ResponseWriter, r *http.Request) {
	var request resetPasswordRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	email, err := account.NormalizeEmail(request.Email)
	if err != nil {
		fieldFailure(w, r, err)
		return
	}
	if err := account.ValidatePassword(request.NewPassword); err != nil {
		fieldFailure(w, r, err)
		return
	}
	if err := d.Challenges.VerifyEmailCode(r.Context(), request.ChallengeID, email, "reset_password", request.EmailCode); err != nil {
		emailCodeFailure(w, r, err)
		return
	}
	user, _, err := d.Store.UserByEmail(r.Context(), email)
	if errors.Is(err, ErrUserNotFound) {
		// Unreachable through the normal flow, because a code for an unknown address is never valid.
		// Answering 204 would still be wrong, so report the code as unusable.
		emailCodeFailure(w, r, err)
		return
	}
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	passwordHash, err := account.HashPassword(request.NewPassword)
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	if _, err := d.Store.DB.ExecContext(r.Context(), `UPDATE users SET password_hash=? WHERE id=?`, passwordHash, user.ID); err != nil {
		d.failDependency(w, r, err)
		return
	}
	if err := d.Sessions.RevokeAll(r.Context(), user.ID); err != nil {
		d.failDependency(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// getMe returns the caller's own profile. It never includes a password digest or a session credential.
func (d Deps) getMe(w http.ResponseWriter, r *http.Request) {
	principal, ok := FromContext(r.Context())
	if !ok {
		httpapi.Fail(w, r, 401, "UNAUTHENTICATED", "请先登录")
		return
	}
	user, err := d.Store.UserByID(r.Context(), principal.UserID)
	if errors.Is(err, ErrUserNotFound) {
		httpapi.Fail(w, r, 401, "UNAUTHENTICATED", "请重新登录")
		return
	}
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	httpapi.JSON(w, 200, toCurrentUser(user))
}

type updateProfileRequest struct {
	Nickname           *string `json:"nickname"`
	Bio                *string `json:"bio"`
	AvatarAttachmentID *string `json:"avatar_attachment_id"`
}

// updateMe applies a partial profile change. At least one field must be present, matching the
// minProperties rule in the contract.
func (d Deps) updateMe(w http.ResponseWriter, r *http.Request) {
	var request updateProfileRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	principal, ok := FromContext(r.Context())
	if !ok {
		httpapi.Fail(w, r, 401, "UNAUTHENTICATED", "请先登录")
		return
	}
	patch := ProfilePatch{Nickname: request.Nickname, Bio: request.Bio}
	if request.Nickname != nil {
		trimmed := strings.TrimSpace(*request.Nickname)
		if err := account.ValidateNickname(trimmed); err != nil {
			fieldFailure(w, r, err)
			return
		}
		patch.Nickname = &trimmed
	}
	if request.Bio != nil {
		if err := account.ValidateBio(*request.Bio); err != nil {
			fieldFailure(w, r, err)
			return
		}
	}
	if request.AvatarAttachmentID != nil {
		id, err := strconv.ParseUint(*request.AvatarAttachmentID, 10, 64)
		if err != nil || id == 0 {
			httpapi.Fail(w, r, 422, "AVATAR_INVALID", "头像标识无效")
			return
		}
		patch.AvatarAttachmentID = &id
	}
	if patch.Empty() {
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "至少需要修改一项资料")
		return
	}
	user, err := d.Store.UpdateProfile(r.Context(), principal.UserID, patch)
	if errors.Is(err, ErrAvatarMissing) {
		// 404 keeps the response from confirming whether an attachment id exists for someone else.
		httpapi.Fail(w, r, 404, "NOT_FOUND", "头像附件不存在")
		return
	}
	if err != nil {
		fieldFailure(w, r, err)
		return
	}
	httpapi.JSON(w, 200, toCurrentUser(user))
}
