package content

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"opentalk/server/internal/auth"
	"opentalk/server/internal/platform/httpapi"
)

// The in-site notification list. It is private to the recipient, so unlike every other read in this
// package it is served behind a session.
//
// Two rules shape the response, and both exist to stop a notification from becoming a side channel:
//
//   - the actor is attached at read time, from the content itself, under the same anonymity rules as
//     everywhere else, and is omitted entirely when the content has become unreachable;
//   - nothing about the content's text is stored or returned. The row carries the event type, the
//     recipient and the content id — a summary is composed from the event type alone, so no deleted or
//     hidden body can leak through a list that was written before the content changed.
//
// The notification's own history is deliberately kept when content is deleted (see
// voidPendingNotifications): the requirement is that deletion reaches what has not been *sent* yet, and a
// notification already in the list is the reader's own record of an event that did happen.

// ErrNotificationNotFound is returned for an id that is not the recipient's own, or does not exist.
// One error for both, so the endpoint cannot be used to confirm that somebody else's notification exists.
var ErrNotificationNotFound = errors.New("notification not found")

// NotificationView is one assembled row of the list.
type NotificationView struct {
	ID        uint64
	EventType string
	ContentID sql.NullInt64
	CreatedAt time.Time
	ReadAt    sql.NullTime

	// Content facts. They are only meaningful when Reachable is true: a notification whose content is gone
	// or was hidden still lists, but without an actor.
	Reachable       bool
	AuthorID        uint64
	TopicAuthorID   uint64
	IsAnonymous     bool
	AnonymousNumber int
	AuthorPublicID  string
	AuthorNickname  string
	AuthorAvatarKey string
	AuthorCreatedAt time.Time
}

// notificationCursorScope binds the cursor to this list. The recipient is deliberately not part of it:
// the cursor is an opaque value the client can read, and an internal user id has no business travelling
// in it. The query already filters by recipient, so a replayed cursor can only ever page the reader's own
// list.
const notificationCursorScope = "notifications"

// ListNotifications returns one page of the recipient's notifications, newest first.
//
// It reads one row beyond the limit so `has_more` is decided from the data rather than guessed, and it
// asks the shared visibility rule once for the whole page instead of once per row.
func (s Store) ListNotifications(ctx context.Context, recipientID uint64, page pageRequest) ([]NotificationView, bool, error) {
	query := `
SELECT n.id,n.event_type,n.content_id,n.created_at,n.read_at,
 c.id,c.author_id,c.is_anonymous,ai.display_number,
 u.public_id,u.nickname,u.created_at,av.storage_key,
 COALESCE(tc.author_id,0)
FROM notifications n
LEFT JOIN contents c ON c.id=n.content_id
LEFT JOIN anonymous_identities ai ON ai.id=c.anonymous_identity_id
LEFT JOIN users u ON u.id=c.author_id
LEFT JOIN attachments av ON av.id=u.avatar_attachment_id AND av.status IN ('pending','active')
LEFT JOIN topics t ON t.content_id=c.id
LEFT JOIN answers a ON a.content_id=c.id
LEFT JOIN comments cm ON cm.content_id=c.id
LEFT JOIN contents tc ON tc.id=COALESCE(t.content_id,a.question_id,cm.topic_id)`
	conditions := []string{`n.recipient_id=?`}
	args := []any{recipientID}
	if len(page.After) == 1 {
		conditions = append(conditions, `n.id<?`)
		args = append(args, page.After[0])
	}
	query += ` WHERE ` + strings.Join(conditions, ` AND `) + ` ORDER BY n.id DESC LIMIT ?`
	args = append(args, page.Limit+1)

	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, err
	}
	type rawRow struct {
		view      NotificationView
		contentID sql.NullInt64
	}
	raw, err := scanRows(rows, func(rows *sql.Rows) (rawRow, error) {
		var item rawRow
		// Every content-side column is nullable here: the notification outlives the row it points at, and
		// a deleted content leaves the join empty rather than the row absent.
		var avatarKey sql.NullString
		var anonymousNumber sql.NullInt64
		var authorID sql.NullInt64
		var publicID, nickname sql.NullString
		var authorCreatedAt sql.NullTime
		err := rows.Scan(&item.view.ID, &item.view.EventType, &item.view.ContentID, &item.view.CreatedAt, &item.view.ReadAt,
			&item.contentID, &authorID, &item.view.IsAnonymous, &anonymousNumber,
			&publicID, &nickname, &authorCreatedAt, &avatarKey,
			&item.view.TopicAuthorID)
		if err != nil {
			return item, err
		}
		item.view.AuthorID = uint64(authorID.Int64)
		item.view.AnonymousNumber = int(anonymousNumber.Int64)
		item.view.AuthorAvatarKey = avatarKey.String
		item.view.AuthorPublicID = publicID.String
		item.view.AuthorNickname = nickname.String
		item.view.AuthorCreatedAt = authorCreatedAt.Time
		// A notification pointing at content that no longer exists is not an error: the event happened.
		item.view.Reachable = item.contentID.Valid
		return item, nil
	})
	if err != nil {
		return nil, false, err
	}
	hasMore := len(raw) > page.Limit
	if hasMore {
		raw = raw[:page.Limit]
	}

	// One visibility question for the page. Rows whose content is already missing are excluded here, so
	// the rule is asked only about rows that still name something.
	ids := make([]uint64, 0, len(raw))
	for _, item := range raw {
		if item.contentID.Valid {
			ids = append(ids, uint64(item.contentID.Int64))
		}
	}
	visible := map[uint64]bool{}
	if len(ids) > 0 {
		visible, err = visibleContentIDs(ctx, s.DB, ids)
		if err != nil {
			return nil, false, err
		}
	}

	views := make([]NotificationView, 0, len(raw))
	for _, item := range raw {
		view := item.view
		view.Reachable = item.contentID.Valid && visible[uint64(item.contentID.Int64)]
		if !view.Reachable {
			// Nothing about the author survives a content that the reader may not see: not the nick, not
			// the avatar, not the anonymous number.
			view.AuthorID, view.AuthorPublicID, view.AuthorNickname, view.AuthorAvatarKey = 0, "", "", ""
			view.AnonymousNumber = 0
		}
		views = append(views, view)
	}
	return views, hasMore, nil
}

// MarkNotificationRead marks one notification read, for its recipient only.
//
// It reads the row first instead of relying on the update's affected-row count: MySQL reports zero
// affected rows for an update that changes nothing, so "already read" and "not yours" would be
// indistinguishable, and idempotence would come at the cost of a 404.
func (s Store) MarkNotificationRead(ctx context.Context, recipientID, notificationID uint64) error {
	var readAt sql.NullTime
	err := s.DB.QueryRowContext(ctx,
		`SELECT read_at FROM notifications WHERE id=? AND recipient_id=?`, notificationID, recipientID).Scan(&readAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotificationNotFound
	}
	if err != nil {
		return err
	}
	if readAt.Valid {
		// Already read: the caller asked for a state that already holds, which is not an error.
		return nil
	}
	_, err = s.DB.ExecContext(ctx,
		`UPDATE notifications SET read_at=UTC_TIMESTAMP(6) WHERE id=? AND recipient_id=? AND read_at IS NULL`,
		notificationID, recipientID)
	return err
}

// notificationResponse mirrors the Notification schema.
type notificationResponse struct {
	ID        string          `json:"id"`
	EventType string          `json:"event_type"`
	ContentID string          `json:"content_id,omitempty"`
	Summary   string          `json:"summary"`
	Actor     *authorResponse `json:"actor,omitempty"`
	CreatedAt string          `json:"created_at"`
	ReadAt    string          `json:"read_at,omitempty"`
}

// notificationSummaryText is what the reader is told. When the content is no longer reachable the event
// type is not even named, because "your question got an answer" about an answer that was deleted says
// more than the reader is allowed to learn; and the text never contains a name, an id or a body.
func notificationSummaryText(view NotificationView) string {
	if !view.Reachable {
		return "该内容已被删除或不可见"
	}
	return notificationSummary(view.EventType)
}

func toNotificationResponse(view NotificationView) notificationResponse {
	response := notificationResponse{
		ID:        strconv.FormatUint(view.ID, 10),
		EventType: view.EventType,
		Summary:   notificationSummaryText(view),
		CreatedAt: formatTime(view.CreatedAt),
	}
	if view.ContentID.Valid {
		response.ContentID = strconv.FormatInt(view.ContentID.Int64, 10)
	}
	if view.ReadAt.Valid {
		response.ReadAt = formatTime(view.ReadAt.Time)
	}
	if view.Reachable {
		// The same author rendering every other read uses: an anonymous author contributes a display name
		// and nothing else, and the topic-owner marker is computed from the content, not from the reader.
		actor := renderAuthor(view.IsAnonymous, view.AuthorID == view.TopicAuthorID, view.AnonymousNumber,
			view.AuthorPublicID, view.AuthorNickname, view.AuthorAvatarKey, view.AuthorCreatedAt)
		response.Actor = &actor
	}
	return response
}

// listNotifications serves the recipient's own notification list.
func (d Deps) listNotifications(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.FromContext(r.Context())
	if !ok {
		httpapi.Fail(w, r, 401, "UNAUTHENTICATED", "请先登录")
		return
	}
	page, ok := d.parsePageOrFail(w, r, notificationCursorScope)
	if !ok {
		return
	}
	notifications, hasMore, err := d.Store.ListNotifications(r.Context(), principal.UserID, page)
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	view := notificationPageView{Items: make([]notificationResponse, 0, len(notifications)), HasMore: hasMore}
	for _, notification := range notifications {
		view.Items = append(view.Items, toNotificationResponse(notification))
	}
	if hasMore && len(notifications) > 0 {
		view.NextCursor = encodeCursor(notificationCursorScope, int64(notifications[len(notifications)-1].ID))
	}
	httpapi.JSON(w, http.StatusOK, view)
}

// readNotification marks one notification read. Marking it twice is not an error, and somebody else's
// notification is reported as missing rather than as forbidden.
func (d Deps) readNotification(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.FromContext(r.Context())
	if !ok {
		httpapi.Fail(w, r, 401, "UNAUTHENTICATED", "请先登录")
		return
	}
	notificationID, ok := pathID(w, r, "notification_id")
	if !ok {
		return
	}
	err := d.Store.MarkNotificationRead(r.Context(), principal.UserID, notificationID)
	switch {
	case errors.Is(err, ErrNotificationNotFound):
		httpapi.Fail(w, r, 404, "NOTIFICATION_NOT_FOUND", "通知不存在")
	case err != nil:
		d.failDependency(w, r, err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// notificationPageView mirrors NotificationPage.
type notificationPageView struct {
	Items      []notificationResponse `json:"items"`
	HasMore    bool                   `json:"has_more"`
	NextCursor string                 `json:"next_cursor,omitempty"`
}
