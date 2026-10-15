package content

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"opentalk/server/internal/auth"
	"opentalk/server/internal/platform/httpapi"
)

// Notification email preferences: the global per-user switch that decides whether an in-site notification
// is also mailed.
//
// Two properties are deliberate.
//
// **Email is opt-in.** A user who has never opened this screen has no row, and no row reads as "all off".
// The alternative — inventing a default that starts mailing people who never asked — is exactly what the
// requirements forbid ("不设置未经确认的默认值"), and it is the only safe direction for a setting whose
// failure mode is unwanted mail. In-site notifications are unaffected: they are written regardless, which
// is also what "关闭邮件不影响站内通知" says.
//
// **The write replaces all three flags, and a partial body is refused.** A PUT that silently treated a
// missing field as false would turn "turn on replies" into "turn off comments and answers", which is a
// data-losing surprise rather than a validation error.

// notificationPreferences mirrors the NotificationPreferences schema.
type notificationPreferences struct {
	EmailOnComment bool `json:"email_on_comment"`
	EmailOnAnswer  bool `json:"email_on_answer"`
	EmailOnReply   bool `json:"email_on_reply"`
}

// NotificationPreferences reads one account's flags. A missing row is "nothing chosen yet", which reads as
// all-false rather than as an error.
func (s Store) NotificationPreferences(ctx context.Context, userID uint64) (notificationPreferences, error) {
	var preferences notificationPreferences
	err := s.DB.QueryRowContext(ctx,
		`SELECT email_on_comment,email_on_answer,email_on_reply FROM notification_preferences WHERE user_id=?`,
		userID).
		Scan(&preferences.EmailOnComment, &preferences.EmailOnAnswer, &preferences.EmailOnReply)
	if errors.Is(err, sql.ErrNoRows) {
		return notificationPreferences{}, nil
	}
	if err != nil {
		return notificationPreferences{}, err
	}
	return preferences, nil
}

// SaveNotificationPreferences upserts the row. The row alias form is used instead of the deprecated
// VALUES() function, which MySQL 8.4 still accepts but warns about.
func (s Store) SaveNotificationPreferences(ctx context.Context, userID uint64, preferences notificationPreferences) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO notification_preferences
 (user_id,email_on_comment,email_on_answer,email_on_reply)
VALUES (?,?,?,?) AS incoming
ON DUPLICATE KEY UPDATE email_on_comment=incoming.email_on_comment,
 email_on_answer=incoming.email_on_answer,
 email_on_reply=incoming.email_on_reply`,
		userID, preferences.EmailOnComment, preferences.EmailOnAnswer, preferences.EmailOnReply)
	return err
}

func (d Deps) getNotificationPreferences(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.FromContext(r.Context())
	if !ok {
		httpapi.Fail(w, r, 401, "UNAUTHENTICATED", "请先登录")
		return
	}
	preferences, err := d.Store.NotificationPreferences(r.Context(), principal.UserID)
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, preferences)
}

// setNotificationPreferencesRequest uses pointers for the same reason the edit endpoint does: a missing
// field has to be distinguishable from a false one.
type setNotificationPreferencesRequest struct {
	EmailOnComment *bool `json:"email_on_comment"`
	EmailOnAnswer  *bool `json:"email_on_answer"`
	EmailOnReply   *bool `json:"email_on_reply"`
}

func (d Deps) setNotificationPreferences(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.FromContext(r.Context())
	if !ok {
		httpapi.Fail(w, r, 401, "UNAUTHENTICATED", "请先登录")
		return
	}
	var request setNotificationPreferencesRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	if request.EmailOnComment == nil || request.EmailOnAnswer == nil || request.EmailOnReply == nil {
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "三项邮件偏好都必须提供")
		return
	}
	preferences := notificationPreferences{
		EmailOnComment: *request.EmailOnComment,
		EmailOnAnswer:  *request.EmailOnAnswer,
		EmailOnReply:   *request.EmailOnReply,
	}
	if err := d.Store.SaveNotificationPreferences(r.Context(), principal.UserID, preferences); err != nil {
		d.failDependency(w, r, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, preferences)
}
