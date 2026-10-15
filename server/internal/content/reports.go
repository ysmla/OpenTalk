package content

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-sql-driver/mysql"

	"opentalk/server/internal/auth"
	"opentalk/server/internal/platform/httpapi"
)

// Reporting content.
//
// This is the reporter's half of moderation: a reader flags something, can follow their own reports, and
// learns the outcome from `resolution_note`. The queue an administrator works from, and the visibility
// change that closes a case, are the other half and are not implemented yet.
//
// Two properties are deliberate.
//
// **A report never reveals the reporter to the content's author.** The reporter is visible only to
// themselves and to administrators. Nothing here renders the reporter into anything the reported author
// could read.
//
// **Reporting does not touch the content.** A report is a claim, not a verdict: it does not hide, lock or
// mark anything, so a flood of reports cannot remove a post by itself. That is what keeps the moderation
// decision auditable and keeps this endpoint free of side effects on the reported row.

// Report statuses, as the contract spells them.
const (
	reportPending    = "pending"
	reportProcessing = "processing"
	reportResolved   = "resolved"
	reportDismissed  = "dismissed"
)

// reportMaxReason and reportMaxDetails bound what a reader can store. The column is VARCHAR(500)/TEXT, so
// these are checked before the database has to complain.
const (
	reportMaxReason  = 500
	reportMaxDetails = 2000
)

// ErrReportAlreadyActive reports a second open report by the same user about the same content. The unique
// key (reporter_id, content_id, active_slot) enforces it, and naming it here is what lets the handler answer
// with a clear conflict instead of a duplicate-key error.
var ErrReportAlreadyActive = errors.New("an open report already exists")

// reportView mirrors the MyReport schema: what the reporter may know about their own report.
type reportView struct {
	ID             string     `json:"id"`
	ContentID      string     `json:"content_id"`
	Reason         string     `json:"reason"`
	Details        string     `json:"details"`
	Status         string     `json:"status"`
	ResolutionNote string     `json:"resolution_note,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	ResolvedAt     *time.Time `json:"resolved_at,omitempty"`
}

// CreateReport records one report. The target must be visible to the reporter: an invisible row is reported
// as missing, so a report cannot be used to probe whether a hidden id exists.
//
// The insert relies on the table's own uniqueness rather than a read-then-write check: two concurrent
// submissions from the same reader would both pass a check and then race, while the unique key cannot be
// raced.
func (s Store) CreateReport(ctx context.Context, reporterID, contentID uint64, reason, details string) (uint64, error) {
	if err := requireVisible(ctx, s.DB, contentID); err != nil {
		return 0, err
	}
	result, err := s.DB.ExecContext(ctx, `INSERT INTO reports (reporter_id,content_id,reason,details)
VALUES (?,?,?,?)`, reporterID, contentID, reason, details)
	if err != nil {
		var mysqlErr *mysql.MySQLError
		if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
			return 0, ErrReportAlreadyActive
		}
		return 0, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	return uint64(id), nil
}

// ReportByID reads one report for its reporter. A report that belongs to somebody else is reported as
// missing rather than forbidden: the reporter of a hidden or foreign row is none of the caller's business.
func (s Store) ReportByID(ctx context.Context, reporterID, reportID uint64) (reportView, error) {
	var view reportView
	var resolutionNote sql.NullString
	var resolvedAt sql.NullTime
	err := s.DB.QueryRowContext(ctx, `SELECT id,content_id,reason,details,status,resolution_note,created_at,resolved_at
 FROM reports WHERE id=? AND reporter_id=?`, reportID, reporterID).
		Scan(&view.ID, &view.ContentID, &view.Reason, &view.Details, &view.Status, &resolutionNote, &view.CreatedAt, &resolvedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return reportView{}, ErrContentNotFound
	}
	if err != nil {
		return reportView{}, err
	}
	view.ResolutionNote = resolutionNote.String
	if resolvedAt.Valid {
		resolved := resolvedAt.Time
		view.ResolvedAt = &resolved
	}
	return view, nil
}

// ListMyReports returns the caller's own reports, newest first, with the same keyset pagination as every
// other list here.
func (s Store) ListMyReports(ctx context.Context, reporterID uint64, page pageRequest) ([]reportView, bool, uint64, error) {
	conditions := []string{`reporter_id=?`}
	args := []any{reporterID}
	if len(page.After) == 1 {
		conditions = append(conditions, `id<?`)
		args = append(args, page.After[0])
	}
	args = append(args, page.Limit+1)
	rows, err := s.DB.QueryContext(ctx, `SELECT id,content_id,reason,details,status,resolution_note,created_at,resolved_at
 FROM reports WHERE `+strings.Join(conditions, " AND ")+` ORDER BY id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, false, 0, err
	}
	views, err := scanRows(rows, func(rows *sql.Rows) (reportView, error) {
		var view reportView
		var resolutionNote sql.NullString
		var resolvedAt sql.NullTime
		if err := rows.Scan(&view.ID, &view.ContentID, &view.Reason, &view.Details, &view.Status, &resolutionNote, &view.CreatedAt, &resolvedAt); err != nil {
			return view, err
		}
		view.ResolutionNote = resolutionNote.String
		if resolvedAt.Valid {
			resolved := resolvedAt.Time
			view.ResolvedAt = &resolved
		}
		return view, nil
	})
	if err != nil {
		return nil, false, 0, err
	}
	hasMore := len(views) > page.Limit
	if hasMore {
		views = views[:page.Limit]
	}
	last := uint64(0)
	if len(views) > 0 {
		last = mustParseID(views[len(views)-1].ID)
	}
	return views, hasMore, last, nil
}

// createReport serves POST /api/v1/reports.
func (d Deps) createReport(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.FromContext(r.Context())
	if !ok {
		httpapi.Fail(w, r, 401, "UNAUTHENTICATED", "请先登录")
		return
	}
	var request struct {
		ContentID string `json:"content_id"`
		Reason    string `json:"reason"`
		Details   string `json:"details"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	contentID, err := ParseID(request.ContentID)
	if err != nil {
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "content_id 无效")
		return
	}
	reason := strings.TrimSpace(request.Reason)
	if reason == "" {
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "请填写举报原因")
		return
	}
	if utf8.RuneCountInString(reason) > reportMaxReason {
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "举报原因过长")
		return
	}
	details := strings.TrimSpace(request.Details)
	if utf8.RuneCountInString(details) > reportMaxDetails {
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "补充说明过长")
		return
	}
	reportID, err := d.Store.CreateReport(r.Context(), principal.UserID, contentID, reason, details)
	switch {
	case errors.Is(err, ErrReportAlreadyActive):
		httpapi.Fail(w, r, 409, "REPORT_ALREADY_ACTIVE", "你已经举报过这条内容，正在处理中")
		return
	case errors.Is(err, ErrContentNotFound), errors.Is(err, ErrTopicNotFound):
		// Reported as missing, not as forbidden: otherwise a report could be used to test whether a hidden
		// id exists.
		httpapi.Fail(w, r, 404, "CONTENT_NOT_FOUND", "内容不存在或不可见")
		return
	case err != nil:
		d.failDependency(w, r, err)
		return
	}
	view, err := d.Store.ReportByID(r.Context(), principal.UserID, reportID)
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	httpapi.JSON(w, http.StatusCreated, view)
}

// listMyReports serves GET /api/v1/me/reports.
func (d Deps) listMyReports(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.FromContext(r.Context())
	if !ok {
		httpapi.Fail(w, r, 401, "UNAUTHENTICATED", "请先登录")
		return
	}
	page, ok := d.parsePageOrFail(w, r, reportCursorScope)
	if !ok {
		return
	}
	views, hasMore, last, err := d.Store.ListMyReports(r.Context(), principal.UserID, page)
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	view := struct {
		Items      []reportView `json:"items"`
		HasMore    bool         `json:"has_more"`
		NextCursor string       `json:"next_cursor,omitempty"`
	}{Items: views, HasMore: hasMore}
	if hasMore && last != 0 {
		view.NextCursor = encodeCursor(reportCursorScope, int64(last))
	}
	httpapi.JSON(w, http.StatusOK, view)
}

// reportCursorScope binds a cursor to this list, so a cursor issued elsewhere cannot page it.
const reportCursorScope = "my-reports"

// mustParseID reads an id the database returned as text. It cannot fail for a row that exists, and a zero
// would only end a page early, so it does not need an error path.
func mustParseID(value string) uint64 {
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0
	}
	return parsed
}
