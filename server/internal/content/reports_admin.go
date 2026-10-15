package content

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"opentalk/server/internal/auth"
	"opentalk/server/internal/platform/httpapi"
)

// The administrative half of reporting: the queue an administrator works from, and closing a report.
//
// Two boundaries are deliberate.
//
// **Working a report is not a moderation action.** Closing a report records a decision about the *report*
// and writes an audit row; it does not hide, delete or otherwise touch the content. Those are separate
// endpoints on purpose, because a case can be closed either way ("resolved" with the content hidden, or
// "dismissed" with nothing done) and a single endpoint that did both would make the audit trail unable to
// say which of the two an administrator actually chose.
//
// **An administrator does not thereby see anonymous authors.** The queue payload carries the reporter and
// the handler, both of which are administrators' business; it deliberately carries nothing about who wrote
// the reported content. The contract says this in one line ("举报并不自动授权查看匿名作者"), and the way to
// honour it is to not select those columns at all rather than to filter them later.

// ErrReportAlreadyClosed reports a second close of the same report. Closing is a one-way transition.
var ErrReportAlreadyClosed = errors.New("report is already closed")

// activeRestrictionSQL is the same "currently in force" condition the account paths use, kept identical so
// that "banned" cannot mean two different things in two places.
const activeRestrictionSQL = `r.revoked_at IS NULL AND r.starts_at<=UTC_TIMESTAMP(6) AND (r.ends_at IS NULL OR r.ends_at>UTC_TIMESTAMP(6))`

// adminUserResponse mirrors the AdminUser schema: an internal id (never present in public content payloads),
// the public profile, and the two states an administrator needs.
type adminUserResponse struct {
	InternalUserID string             `json:"internal_user_id"`
	Profile        publicUserResponse `json:"profile"`
	AccountStatus  string             `json:"account_status"`
	Role           string             `json:"role"`
}

// adminReportResponse mirrors the AdminReport schema.
type adminReportResponse struct {
	Report   reportView         `json:"report"`
	Reporter adminUserResponse  `json:"reporter"`
	Handler  *adminUserResponse `json:"handler,omitempty"`
}

// adminReportSelect is shared by the queue and by the single report returned after closing one, so the two
// can never describe a report differently.
const adminReportSelect = `
SELECT r.id,r.content_id,r.reason,r.details,r.status,r.resolution_note,r.created_at,r.resolved_at,
 ru.id,ru.public_id,ru.nickname,ru.role,ru.created_at,
 EXISTS(SELECT 1 FROM user_restrictions r WHERE r.user_id=ru.id AND r.kind='ban' AND ` + activeRestrictionSQL + `),
 hu.id,hu.public_id,hu.nickname,hu.role,hu.created_at,
 EXISTS(SELECT 1 FROM user_restrictions r WHERE r.user_id=hu.id AND r.kind='ban' AND ` + activeRestrictionSQL + `)
FROM reports r
JOIN users ru ON ru.id=r.reporter_id
LEFT JOIN users hu ON hu.id=r.handler_id`

// adminReports runs the shared query with an extra condition and reads the pairs.
func (s Store) adminReports(ctx context.Context, condition string, args []any, limit int) ([]adminReportResponse, error) {
	args = append(args, limit)
	rows, err := s.DB.QueryContext(ctx, adminReportSelect+` WHERE `+condition+` ORDER BY r.id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	return scanRows(rows, func(rows *sql.Rows) (adminReportResponse, error) {
		var response adminReportResponse
		var resolutionNote sql.NullString
		var resolvedAt sql.NullTime
		var reporterCreated time.Time
		var handlerID sql.NullInt64
		var handlerPublicID, handlerNickname, handlerRole sql.NullString
		var handlerCreated sql.NullTime
		var handlerBanned sql.NullBool
		var reporterBanned bool
		if err := rows.Scan(
			&response.Report.ID, &response.Report.ContentID, &response.Report.Reason, &response.Report.Details,
			&response.Report.Status, &resolutionNote, &response.Report.CreatedAt, &resolvedAt,
			&response.Reporter.InternalUserID, &response.Reporter.Profile.PublicID,
			&response.Reporter.Profile.Nickname, &response.Reporter.Role, &reporterCreated,
			&reporterBanned,
			&handlerID, &handlerPublicID, &handlerNickname, &handlerRole, &handlerCreated, &handlerBanned,
		); err != nil {
			return response, err
		}
		// A DATETIME cannot be scanned into a string field, so the timestamp is read as a time and rendered by
		// the same helper every other payload uses.
		response.Reporter.Profile.CreatedAt = formatTime(reporterCreated)
		response.Report.ResolutionNote = resolutionNote.String
		if resolvedAt.Valid {
			resolved := resolvedAt.Time
			response.Report.ResolvedAt = &resolved
		}
		response.Reporter.AccountStatus = accountStatusOf(reporterBanned)
		if handlerID.Valid {
			response.Handler = &adminUserResponse{
				InternalUserID: formatID(uint64(handlerID.Int64)),
				Profile: publicUserResponse{
					PublicID:  handlerPublicID.String,
					Nickname:  handlerNickname.String,
					CreatedAt: formatTime(handlerCreated.Time),
				},
				AccountStatus: accountStatusOf(handlerBanned.Valid && handlerBanned.Bool),
				Role:          handlerRole.String,
			}
		}
		return response, nil
	})
}

// accountStatusOf maps the ban check onto the contract's two values.
func accountStatusOf(banned bool) string {
	if banned {
		return "banned"
	}
	return "active"
}

// ListAdminReports returns the open reports, newest first: the queue is the work that is left, so closed
// reports are not in it. Their outcome is readable by their reporter, and the decision itself is in the
// audit trail.
func (s Store) ListAdminReports(ctx context.Context, page pageRequest) ([]adminReportResponse, bool, uint64, error) {
	condition := `r.status IN ('pending','processing')`
	args := []any{}
	if len(page.After) == 1 {
		condition += ` AND r.id<?`
		args = append(args, page.After[0])
	}
	reports, err := s.adminReports(ctx, condition, args, page.Limit+1)
	if err != nil {
		return nil, false, 0, err
	}
	hasMore := len(reports) > page.Limit
	if hasMore {
		reports = reports[:page.Limit]
	}
	last := uint64(0)
	if len(reports) > 0 {
		last = mustParseID(reports[len(reports)-1].Report.ID)
	}
	return reports, hasMore, last, nil
}

// ResolveReport closes one report and records who closed it and why, in one transaction.
//
// The status check and the update are in the same transaction with the row locked, so two administrators
// closing the same report cannot both succeed and both write an audit row for one decision.
func (s Store) ResolveReport(ctx context.Context, actorID, reportID uint64, status, note string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var current string
	err = tx.QueryRowContext(ctx, `SELECT status FROM reports WHERE id=? FOR UPDATE`, reportID).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrContentNotFound
	}
	if err != nil {
		return err
	}
	if current == reportResolved || current == reportDismissed {
		return ErrReportAlreadyClosed
	}
	// `active_slot` is a generated column, so it frees the uniqueness slot by itself once the status is no
	// longer open — that is what lets the same reader report the same content again.
	if _, err := tx.ExecContext(ctx, `UPDATE reports
 SET status=?, resolution_note=?, handler_id=?, resolved_at=UTC_TIMESTAMP(6) WHERE id=?`,
		status, note, actorID, reportID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO moderation_actions (actor_id,report_id,action,reason)
VALUES (?,?,?,?)`, actorID, reportID, actionResolveReport, note); err != nil {
		return err
	}
	return tx.Commit()
}

// actionResolveReport is the audit action for closing a report.
const actionResolveReport = "resolve_report"

// AdminReportByID reads one report in the administrative shape, used to answer the close request with the
// closed row.
func (s Store) AdminReportByID(ctx context.Context, reportID uint64) (adminReportResponse, error) {
	reports, err := s.adminReports(ctx, `r.id=?`, []any{reportID}, 1)
	if err != nil {
		return adminReportResponse{}, err
	}
	if len(reports) == 0 {
		return adminReportResponse{}, ErrContentNotFound
	}
	return reports[0], nil
}

// listAdminReports serves GET /api/v1/admin/reports.
func (d Deps) listAdminReports(w http.ResponseWriter, r *http.Request) {
	page, ok := d.parsePageOrFail(w, r, adminReportCursorScope)
	if !ok {
		return
	}
	reports, hasMore, last, err := d.Store.ListAdminReports(r.Context(), page)
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	view := struct {
		Items      []adminReportResponse `json:"items"`
		HasMore    bool                  `json:"has_more"`
		NextCursor string                `json:"next_cursor,omitempty"`
	}{Items: reports, HasMore: hasMore}
	if hasMore && last != 0 {
		view.NextCursor = encodeCursor(adminReportCursorScope, int64(last))
	}
	httpapi.JSON(w, http.StatusOK, view)
}

// resolveReport serves PATCH /api/v1/admin/reports/{report_id}.
func (d Deps) resolveReport(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.FromContext(r.Context())
	if !ok {
		httpapi.Fail(w, r, 401, "UNAUTHENTICATED", "请先登录")
		return
	}
	reportID, ok := pathID(w, r, "report_id")
	if !ok {
		return
	}
	var request struct {
		Status         string `json:"status"`
		ResolutionNote string `json:"resolution_note"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	// Only the two closing statuses are accepted: "pending" or "processing" would be a reopen, and reopening
	// is not a thing this endpoint does.
	if request.Status != reportResolved && request.Status != reportDismissed {
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "status 只能是 resolved 或 dismissed")
		return
	}
	note := strings.TrimSpace(request.ResolutionNote)
	if note == "" {
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "请填写处理说明")
		return
	}
	if utf8.RuneCountInString(note) > auditReasonMax {
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "处理说明过长")
		return
	}
	err := d.Store.ResolveReport(r.Context(), principal.UserID, reportID, request.Status, note)
	switch {
	case errors.Is(err, ErrReportAlreadyClosed):
		httpapi.Fail(w, r, 409, "REPORT_ALREADY_CLOSED", "该举报已经结案")
		return
	case errors.Is(err, ErrContentNotFound):
		httpapi.Fail(w, r, 404, "REPORT_NOT_FOUND", "举报不存在")
		return
	case err != nil:
		d.failDependency(w, r, err)
		return
	}
	response, err := d.Store.AdminReportByID(r.Context(), reportID)
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, response)
}

// adminReportCursorScope binds a cursor to the queue.
const adminReportCursorScope = "admin-reports"
