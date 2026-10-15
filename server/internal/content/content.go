// Package content serves the discussion and Q&A surface. It starts with the read-only board list;
// topic, answer and comment writes land here as the content loop is implemented.
package content

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"opentalk/server/internal/auth"
	"opentalk/server/internal/media"
	"opentalk/server/internal/platform/httpapi"
	"opentalk/server/internal/settings"
)

// Store reads and writes content rows.
type Store struct{ DB *sql.DB }

// Deps carries what the content endpoints need.
type Deps struct {
	Store  Store
	Logger *slog.Logger
	// Sessions resolves the signed-in account. The write routes are mounted only when it is present, so
	// a misconfigured process cannot expose an unauthenticated write endpoint.
	Sessions auth.SessionStore
	// Settings supplies the administrator-configured per-content image limit.
	Settings *settings.Store
	// Media stores uploaded images. When it is absent the upload and image routes are not mounted.
	Media *media.Store
	// UnsubscribeKey signs the unsubscribe links carried by notification mail. It is its own subkey derived
	// from APP_SECRET, so a token that leaks cannot be reused as a session or a settings key.
	UnsubscribeKey []byte
}

// Register mounts the public content reads and the authenticated writes. Writes go through
// auth.Require, which performs the exact-Origin check and the session-bound CSRF token check.
func Register(mux *http.ServeMux, deps Deps) {
	mux.HandleFunc("GET /api/v1/boards", deps.listBoards)
	// The read routes are public: a guest may browse. Only the permission summary varies by viewer, and it
	// resolves the session optionally rather than requiring one.
	mux.HandleFunc("GET /api/v1/topics", deps.listTopics)
	mux.HandleFunc("GET /api/v1/topics/{topic_id}/comments", deps.listComments)
	mux.HandleFunc("GET /api/v1/questions/{topic_id}/answers", deps.listAnswers)
	mux.HandleFunc("GET /api/v1/contents/{content_id}", deps.getContent)
	mux.HandleFunc("GET /api/v1/contents/{content_id}/permissions", deps.getContentPermissions)
	mux.HandleFunc("GET /api/v1/tags", deps.listTags)
	// Another reader's public profile and their public content. Anonymous rows are excluded in SQL, which is
	// what keeps "traceable anonymity" from becoming "browseable anonymity".
	mux.HandleFunc("GET /api/v1/users/{public_id}", deps.getUserProfile)
	mux.HandleFunc("GET /api/v1/users/{public_id}/contents", deps.listUserContents)
	// Search is public like every other read here, and it never widens visibility: it narrows candidates and
	// then hands each one to the same reader the other endpoints use.
	mux.HandleFunc("GET /api/v1/search", deps.searchContents)
	if deps.Sessions != nil {
		mux.Handle("POST /api/v1/topics", auth.Require(deps.Sessions, false, http.HandlerFunc(deps.createTopic)))
		mux.Handle("POST /api/v1/answers", auth.Require(deps.Sessions, false, http.HandlerFunc(deps.createAnswer)))
		mux.Handle("POST /api/v1/comments", auth.Require(deps.Sessions, false, http.HandlerFunc(deps.createComment)))
		mux.Handle("PATCH /api/v1/questions/{topic_id}/resolution", auth.Require(deps.Sessions, false, http.HandlerFunc(deps.setResolution)))
		mux.Handle("PATCH /api/v1/contents/{content_id}", auth.Require(deps.Sessions, false, http.HandlerFunc(deps.editContent)))
		mux.Handle("DELETE /api/v1/contents/{content_id}", auth.Require(deps.Sessions, false, http.HandlerFunc(deps.deleteContent)))
		mux.Handle("POST /api/v1/attachments", auth.Require(deps.Sessions, false, http.HandlerFunc(deps.uploadImage)))
		// The notification list is the one read in this package that is not public: it is the recipient's
		// own inbox, and it goes through the same session check as the writes.
		mux.Handle("GET /api/v1/me/notifications", auth.Require(deps.Sessions, false, http.HandlerFunc(deps.listNotifications)))
		mux.Handle("PUT /api/v1/me/notifications/{notification_id}/read", auth.Require(deps.Sessions, false, http.HandlerFunc(deps.readNotification)))
		// Notification email preferences: read and written by their owner only, so they sit behind the same
		// session check as the notification list.
		mux.Handle("GET /api/v1/me/notification-preferences", auth.Require(deps.Sessions, false, http.HandlerFunc(deps.getNotificationPreferences)))
		mux.Handle("PUT /api/v1/me/notification-preferences", auth.Require(deps.Sessions, false, http.HandlerFunc(deps.setNotificationPreferences)))
		// Per-topic mail overrides. The unsubscribe route is mounted outside this block below: it is the one
		// endpoint that must work from a mail link, so it cannot require a session.
		mux.Handle("GET /api/v1/me/subscriptions/{topic_id}", auth.Require(deps.Sessions, false, http.HandlerFunc(deps.getSubscription)))
		mux.Handle("PUT /api/v1/me/subscriptions/{topic_id}", auth.Require(deps.Sessions, false, http.HandlerFunc(deps.setSubscription)))
		// Reporting. The queue an administrator works from is not mounted yet: reports land in the table and
		// are readable by their reporter, and nothing here is visible to the reported author.
		mux.Handle("POST /api/v1/reports", auth.Require(deps.Sessions, false, http.HandlerFunc(deps.createReport)))
		mux.Handle("GET /api/v1/me/reports", auth.Require(deps.Sessions, false, http.HandlerFunc(deps.listMyReports)))
		// The reader's own content, anonymous posts included: it is their own history, and hiding it from
		// them would make it unusable.
		mux.Handle("GET /api/v1/me/contents", auth.Require(deps.Sessions, false, http.HandlerFunc(deps.listMyContents)))
	}
	// Public, but only because the token is the credential: it names one account and one topic, and the
	// signature is the only thing that authorises the change. POST plus the middleware's exact-Origin check
	// keeps a mail scanner that merely follows links from unsubscribing anybody.
	mux.HandleFunc("POST /api/v1/subscriptions/unsubscribe", deps.unsubscribeEmail)
	// Moderation. These are the only routes here that require the admin role, and hiding is the only way a
	// moderation decision enters the system: until now that state could only be written with SQL.
	mux.Handle("PATCH /api/v1/admin/contents/{content_id}/visibility", auth.Require(deps.Sessions, true, http.HandlerFunc(deps.setContentVisibility)))
	mux.Handle("GET /api/v1/admin/audit", auth.Require(deps.Sessions, true, http.HandlerFunc(deps.listAudit)))
	// The administrative half of reporting: the queue, and closing one. Closing a report records a decision
	// about the report and nothing about the content — hiding it is the endpoint above, which is how the
	// audit trail can say which of the two an administrator actually chose.
	mux.Handle("GET /api/v1/admin/reports", auth.Require(deps.Sessions, true, http.HandlerFunc(deps.listAdminReports)))
	mux.Handle("PATCH /api/v1/admin/reports/{report_id}", auth.Require(deps.Sessions, true, http.HandlerFunc(deps.resolveReport)))
	// Restrictions and the account list. A ban revokes sessions in the same transaction as the restriction,
	// so there is no window in which an account is banned but still signed in.
	mux.Handle("POST /api/v1/admin/restrictions", auth.Require(deps.Sessions, true, http.HandlerFunc(deps.createRestriction)))
	mux.Handle("GET /api/v1/admin/restrictions", auth.Require(deps.Sessions, true, http.HandlerFunc(deps.listRestrictions)))
	mux.Handle("DELETE /api/v1/admin/restrictions/{restriction_id}", auth.Require(deps.Sessions, true, http.HandlerFunc(deps.revokeRestriction)))
	mux.Handle("GET /api/v1/admin/users", auth.Require(deps.Sessions, true, http.HandlerFunc(deps.listAdminUsers)))
	if deps.Media != nil {
		// Public: images are embedded in public content, so the route is readable without a session. The
		// key grants nothing on its own: the handler asks the database whether this image may be shown to
		// this viewer, and the session is resolved optionally to admit the owner of a pending upload.
		mux.HandleFunc("GET /media/{key}", deps.getImage)
	}
}

// failDependency answers 503 and keeps the internal cause in the server log only.
func (d Deps) failDependency(w http.ResponseWriter, r *http.Request, err error) {
	if d.Logger != nil {
		d.Logger.Error("dependency failure", "request_id", httpapi.RequestID(r.Context()), "error", err.Error())
	}
	httpapi.Fail(w, r, 503, "DEPENDENCY_UNAVAILABLE", "服务暂不可用")
}

var (
	ErrInvalidCursor = errors.New("invalid cursor")
	ErrInvalidLimit  = errors.New("invalid limit")
)

// Paging bounds are the contract's: default 20, maximum 100.
const (
	defaultLimit = 20
	maxLimit     = 100
)

// pageRequest is the validated pagination input. After is the ordering key of the last row of the
// previous page; it is empty on the first page.
type pageRequest struct {
	Limit int
	After []int64
}

// parsePage validates limit and cursor. The cursor carries the name of the list it was issued for, so
// a cursor from one endpoint cannot be replayed against a differently ordered list — the contract
// requires the cursor to be bound to the filter and sort conditions rather than being an offset in
// disguise.
func parsePage(r *http.Request, scope string) (pageRequest, error) {
	page := pageRequest{Limit: defaultLimit}
	if raw := r.URL.Query().Get("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > maxLimit {
			return page, ErrInvalidLimit
		}
		page.Limit = value
	}
	raw := r.URL.Query().Get("cursor")
	if raw == "" {
		return page, nil
	}
	key, err := decodeCursor(scope, raw)
	if err != nil {
		return page, err
	}
	page.After = key
	return page, nil
}

// encodeCursor builds an opaque cursor from the ordering key of the last returned row. The scope is
// part of the payload so decoding can reject a cursor issued for another list.
func encodeCursor(scope string, key ...int64) string {
	parts := make([]string, 0, len(key)+1)
	parts = append(parts, scope)
	for _, value := range key {
		parts = append(parts, strconv.FormatInt(value, 10))
	}
	return base64.RawURLEncoding.EncodeToString([]byte(strings.Join(parts, "|")))
}

func decodeCursor(scope, cursor string) ([]int64, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return nil, ErrInvalidCursor
	}
	parts := strings.Split(string(decoded), "|")
	if len(parts) < 2 || parts[0] != scope {
		return nil, ErrInvalidCursor
	}
	key := make([]int64, 0, len(parts)-1)
	for _, part := range parts[1:] {
		value, err := strconv.ParseInt(part, 10, 64)
		if err != nil || value < 0 {
			return nil, ErrInvalidCursor
		}
		key = append(key, value)
	}
	return key, nil
}

// scanRows turns a query into a slice, closing the rows and surfacing both iteration and close
// errors so a truncated read is never mistaken for a short list.
func scanRows[T any](rows *sql.Rows, scan func(*sql.Rows) (T, error)) ([]T, error) {
	defer rows.Close()
	items := make([]T, 0)
	for rows.Next() {
		item, err := scan(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

// boardCursorScope binds board cursors to the board ordering.
const boardCursorScope = "boards"

// Board is the stored board row. Archived boards stay visible; the flag only stops new topics.
type Board struct {
	ID          uint64
	Name        string
	Description string
	Kind        string
	SortOrder   int
	IsArchived  bool
}

// ListBoards returns boards ordered by (sort_order, id). It reads one row beyond the limit to decide
// has_more without a second query, so a full page never lies about having a next page.
func (s Store) ListBoards(ctx context.Context, page pageRequest) ([]Board, bool, error) {
	query := `SELECT id,name,description,kind,sort_order,is_archived FROM boards`
	args := make([]any, 0, 3)
	if len(page.After) == 2 {
		query += ` WHERE (sort_order,id) > (?,?)`
		args = append(args, page.After[0], page.After[1])
	}
	query += ` ORDER BY sort_order,id LIMIT ?`
	args = append(args, page.Limit+1)

	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, err
	}
	boards, err := scanRows(rows, func(rows *sql.Rows) (Board, error) {
		var b Board
		err := rows.Scan(&b.ID, &b.Name, &b.Description, &b.Kind, &b.SortOrder, &b.IsArchived)
		return b, err
	})
	if err != nil {
		return nil, false, err
	}
	hasMore := len(boards) > page.Limit
	if hasMore {
		boards = boards[:page.Limit]
	}
	return boards, hasMore, nil
}

// SeedBoards inserts the two first-version boards when the installation has none. It is idempotent:
// running it on a populated instance reports the count and changes nothing, so it cannot duplicate
// boards that an administrator deliberately renamed or removed.
func (s Store) SeedBoards(ctx context.Context) (int, error) {
	var existing int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM boards`).Scan(&existing); err != nil {
		return 0, err
	}
	if existing > 0 {
		return existing, nil
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	for _, board := range []Board{
		{Name: "综合讨论", Description: "一般话题、闲聊与公告。", Kind: "discussion", SortOrder: 10},
		{Name: "问答", Description: "提问与回答，问题可由提问者标记为已解决。", Kind: "qa", SortOrder: 20},
	} {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO boards (name,description,kind,sort_order) VALUES (?,?,?,?)`,
			board.Name, board.Description, board.Kind, board.SortOrder); err != nil {
			return 0, fmt.Errorf("cannot seed board %q: %w", board.Name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return 0, nil
}
