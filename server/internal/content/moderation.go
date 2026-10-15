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

	"opentalk/server/internal/auth"
	"opentalk/server/internal/platform/httpapi"
)

// Moderation: hiding and restoring content, and the append-only record of who did it and why.
//
// Hiding is deliberately the *only* thing this does. It does not delete, does not touch the author, and does
// not consult the report that prompted it: a report and a moderation decision are separate records, and
// keeping them separate is what lets the decision be reviewed later without having to reconstruct which
// report was open at the time.
//
// Descendant visibility needs no code here. `moderation_status` is one of the two columns the shared
// ancestor rule already reads, so hiding a topic stops its answers, their replies and every attachment
// underneath from being readable — the same rule that already governs deleted content. That is why this
// file can be small: the hard part was built and tested once, in visibility.go.

// Moderation audit actions. They are stored as short ascii names because the audit row is a record of
// intent, not a place to store prose.
const (
	actionHideContent    = "hide_content"
	actionRestoreContent = "restore_content"
)

// auditReasonMax bounds the reason. The column is TEXT, so this is a product decision rather than a storage
// limit: an audit entry has to stay readable.
const auditReasonMax = 2000

// auditEntry mirrors the AdminAuditEntry schema. Targets are optional because an action may concern only a
// user or only content.
type auditEntry struct {
	ID              string    `json:"id"`
	ActorUserID     string    `json:"actor_user_id"`
	Action          string    `json:"action"`
	Reason          string    `json:"reason"`
	TargetContentID string    `json:"target_content_id,omitempty"`
	TargetUserID    string    `json:"target_user_id,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
}

// SetContentVisibility hides or restores one content row and records why, in one transaction.
//
// Repeating the same decision is a no-op that writes no audit row: an administrator pressing "hide" twice
// must not produce two entries claiming two decisions were made. The audit trail is only useful if a row
// means "a decision was taken here".
//
// Deleted content cannot be hidden or restored. A deleted row has no body left (it was archived), so there is
// nothing to hide, and reporting it as missing keeps "deleted" and "hidden" from blurring into one state.
func (s Store) SetContentVisibility(ctx context.Context, actorID, contentID uint64, visibility, reason string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var current string
	var deletedAt sql.NullTime
	// FOR UPDATE: two administrators acting at once would otherwise both read the old status and both write
	// an audit row for the same transition.
	err = tx.QueryRowContext(ctx, `SELECT moderation_status,deleted_at FROM contents WHERE id=? FOR UPDATE`, contentID).
		Scan(&current, &deletedAt)
	if errors.Is(err, sql.ErrNoRows) || deletedAt.Valid {
		return ErrContentNotFound
	}
	if err != nil {
		return err
	}
	if current == visibility {
		// Already in the requested state: nothing to change and nothing to record.
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE contents SET moderation_status=? WHERE id=?`, visibility, contentID); err != nil {
		return err
	}
	action := actionHideContent
	if visibility == "visible" {
		action = actionRestoreContent
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO moderation_actions (actor_id,target_content_id,action,reason)
VALUES (?,?,?,?)`, actorID, contentID, action, reason); err != nil {
		return err
	}
	return tx.Commit()
}

// ListAudit returns the moderation record, newest first.
//
// It is a plain read of an append-only table: nothing here writes, updates or deletes an entry, and the
// payload carries ids and the stated reason only — never the content's text, which is what the schema
// comment demands ("不复制内容正文").
func (s Store) ListAudit(ctx context.Context, page pageRequest) ([]auditEntry, bool, uint64, error) {
	conditions := []string{"1=1"}
	args := []any{}
	if len(page.After) == 1 {
		conditions = append(conditions, `id<?`)
		args = append(args, page.After[0])
	}
	args = append(args, page.Limit+1)
	rows, err := s.DB.QueryContext(ctx, `SELECT id,actor_id,action,reason,
 COALESCE(target_content_id,0),COALESCE(target_user_id,0),created_at
 FROM moderation_actions WHERE `+strings.Join(conditions, " AND ")+` ORDER BY id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, false, 0, err
	}
	entries, err := scanRows(rows, func(rows *sql.Rows) (auditEntry, error) {
		var entry auditEntry
		var contentID, userID uint64
		if err := rows.Scan(&entry.ID, &entry.ActorUserID, &entry.Action, &entry.Reason, &contentID, &userID, &entry.CreatedAt); err != nil {
			return entry, err
		}
		if contentID != 0 {
			entry.TargetContentID = formatID(contentID)
		}
		if userID != 0 {
			entry.TargetUserID = formatID(userID)
		}
		return entry, nil
	})
	if err != nil {
		return nil, false, 0, err
	}
	hasMore := len(entries) > page.Limit
	if hasMore {
		entries = entries[:page.Limit]
	}
	last := uint64(0)
	if len(entries) > 0 {
		last = mustParseID(entries[len(entries)-1].ID)
	}
	return entries, hasMore, last, nil
}

// setContentVisibility serves PATCH /api/v1/admin/contents/{content_id}/visibility. It answers 204 because
// the caller asked for a state, not for a representation of the row.
func (d Deps) setContentVisibility(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.FromContext(r.Context())
	if !ok {
		httpapi.Fail(w, r, 401, "UNAUTHENTICATED", "请先登录")
		return
	}
	contentID, ok := pathID(w, r, "content_id")
	if !ok {
		return
	}
	var request struct {
		Visibility string `json:"visibility"`
		Reason     string `json:"reason"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	if request.Visibility != "visible" && request.Visibility != "hidden" {
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "visibility 只能是 visible 或 hidden")
		return
	}
	reason := strings.TrimSpace(request.Reason)
	if reason == "" {
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "请填写操作原因")
		return
	}
	if utf8.RuneCountInString(reason) > auditReasonMax {
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "操作原因过长")
		return
	}
	err := d.Store.SetContentVisibility(r.Context(), principal.UserID, contentID, request.Visibility, reason)
	if errors.Is(err, ErrContentNotFound) {
		httpapi.Fail(w, r, 404, "CONTENT_NOT_FOUND", "内容不存在或已删除")
		return
	}
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// listAudit serves GET /api/v1/admin/audit.
func (d Deps) listAudit(w http.ResponseWriter, r *http.Request) {
	page, ok := d.parsePageOrFail(w, r, auditCursorScope)
	if !ok {
		return
	}
	entries, hasMore, last, err := d.Store.ListAudit(r.Context(), page)
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	view := struct {
		Items      []auditEntry `json:"items"`
		HasMore    bool         `json:"has_more"`
		NextCursor string       `json:"next_cursor,omitempty"`
	}{Items: entries, HasMore: hasMore}
	if hasMore && last != 0 {
		view.NextCursor = encodeCursor(auditCursorScope, int64(last))
	}
	httpapi.JSON(w, http.StatusOK, view)
}

// auditCursorScope binds a cursor to the audit list.
const auditCursorScope = "admin-audit"

// formatID renders an internal id for a JSON string field.
func formatID(value uint64) string {
	return strconv.FormatUint(value, 10)
}
