package content

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"opentalk/server/internal/auth"
	"opentalk/server/internal/platform/httpapi"
)

// The two "reveal something that is normally hidden" operations: reading a deleted original, and tracing who
// wrote anonymous content.
//
// Both are legitimate moderation needs and both are dangerous, so both are built the same way: the reason is
// required and written to the audit trail **before** the answer is returned, and the tracing response carries
// the id of that audit row. An administrator who traces an anonymous author therefore cannot do it without
// leaving a record that says who asked, about what, and why — which is what makes traceable anonymity
// different from broken anonymity.
//
// One honest limit: the contract says anonymous tracing "需要专用权限" (a dedicated permission). This build has
// two roles, so the requirement is met by the admin role plus the mandatory audit. A separate permission level
// does not exist yet, and pretending otherwise in the code would be worse than saying so here.

// ErrArchiveExpired reports a deletion whose 30-day retention has passed. The contract is explicit that such
// a record must not be returned even if the cleanup job has not run yet, so the check is on the timestamp and
// not on the row's continued existence.
var ErrArchiveExpired = errors.New("archive retention window has passed")

// Audit actions for this file.
const (
	actionReadArchive    = "read_deleted_archive"
	actionAnonymousTrace = "anonymous_trace"
	actionAdminDelete    = "admin_delete_content"
)

// adminDeletedArchive mirrors the AdminDeletedArchive schema.
type adminDeletedArchive struct {
	ContentID      string    `json:"content_id"`
	OriginalBodyMD string    `json:"original_body_md"`
	OriginalTitle  string    `json:"original_title,omitempty"`
	DeletedAt      time.Time `json:"deleted_at"`
	PurgeAfter     time.Time `json:"purge_after"`
}

// adminAnonymousTrace mirrors the AdminAnonymousTrace schema.
type adminAnonymousTrace struct {
	ContentID string            `json:"content_id"`
	Author    adminUserResponse `json:"author"`
	AuditID   string            `json:"audit_id"`
}

// requireAdmin reports whether an account currently holds the admin role. It takes a queryer so it can be used
// inside a transaction, where the answer must be read from the same consistent view as the change.
func requireAdmin(ctx context.Context, q queryer, userID uint64) error {
	var role string
	err := q.QueryRowContext(ctx, `SELECT role FROM users WHERE id=?`, userID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrContentNotFound
	}
	if err != nil {
		return err
	}
	if role != "admin" {
		return ErrAdminRequired
	}
	return nil
}

// ErrAdminRequired reports an action that needs the admin role.
var ErrAdminRequired = errors.New("administrator role is required")

// ReadDeletedArchive returns the archived original of a deleted content row and records who read it and why.
func (s Store) ReadDeletedArchive(ctx context.Context, actorID, contentID uint64, reason string) (adminDeletedArchive, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return adminDeletedArchive{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var archive adminDeletedArchive
	var title sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT original_body_md,original_title,deleted_at,purge_after
 FROM deleted_content_archives WHERE content_id=?`, contentID).
		Scan(&archive.OriginalBodyMD, &title, &archive.DeletedAt, &archive.PurgeAfter)
	if errors.Is(err, sql.ErrNoRows) {
		return adminDeletedArchive{}, ErrContentNotFound
	}
	if err != nil {
		return adminDeletedArchive{}, err
	}
	// Checked against the clock, not against whether the cleanup job has run: the contract says a record past
	// its window must not be returned even if it is still on disk.
	if time.Now().UTC().After(archive.PurgeAfter) {
		return adminDeletedArchive{}, ErrArchiveExpired
	}
	archive.ContentID = formatID(contentID)
	archive.OriginalTitle = title.String
	// The audit is written as part of reading, in the same transaction, so the record cannot be separated from
	// the disclosure it describes.
	if err := writeAudit(ctx, tx, actorID, &contentID, nil, actionReadArchive, reason); err != nil {
		return adminDeletedArchive{}, err
	}
	if err := tx.Commit(); err != nil {
		return adminDeletedArchive{}, err
	}
	return archive, nil
}

// TraceAnonymousAuthor returns the account behind anonymous content, with the id of the audit row that records
// the trace.
//
// Visibility is deliberately not required. The usual case is a moderator investigating content that has
// already been hidden, and demanding visibility first would push the investigation back to raw SQL — which is
// exactly the unaudited path this endpoint replaces. Deleted content is refused instead: its author row is
// still there, but the contract treats the archive as the audited way to look at deletions.
func (s Store) TraceAnonymousAuthor(ctx context.Context, actorID, contentID uint64, reason string) (adminAnonymousTrace, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return adminAnonymousTrace{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var authorID uint64
	err = tx.QueryRowContext(ctx, `SELECT author_id FROM contents WHERE id=? AND deleted_at IS NULL`, contentID).Scan(&authorID)
	if errors.Is(err, sql.ErrNoRows) {
		return adminAnonymousTrace{}, ErrContentNotFound
	}
	if err != nil {
		return adminAnonymousTrace{}, err
	}
	var author adminUserResponse
	var createdAt time.Time
	var banned bool
	err = tx.QueryRowContext(ctx, `SELECT id,public_id,nickname,role,created_at,
 EXISTS(SELECT 1 FROM user_restrictions r WHERE r.user_id=u.id AND r.kind='ban' AND `+activeRestrictionSQL+`)
 FROM users u WHERE u.id=?`, authorID).
		Scan(&author.InternalUserID, &author.Profile.PublicID, &author.Profile.Nickname, &author.Role, &createdAt, &banned)
	if err != nil {
		return adminAnonymousTrace{}, err
	}
	author.Profile.CreatedAt = formatTime(createdAt)
	author.AccountStatus = accountStatusOf(banned)

	result, err := tx.ExecContext(ctx, `INSERT INTO moderation_actions (actor_id,target_content_id,action,reason)
VALUES (?,?,?,?)`, actorID, contentID, actionAnonymousTrace, reason)
	if err != nil {
		return adminAnonymousTrace{}, err
	}
	auditID, err := result.LastInsertId()
	if err != nil {
		return adminAnonymousTrace{}, err
	}
	if err := tx.Commit(); err != nil {
		return adminAnonymousTrace{}, err
	}
	return adminAnonymousTrace{ContentID: formatID(contentID), Author: author, AuditID: formatID(uint64(auditID))}, nil
}

// readDeletedArchive serves POST /api/v1/admin/deleted-archives/read.
func (d Deps) readDeletedArchive(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.FromContext(r.Context())
	if !ok {
		httpapi.Fail(w, r, 401, "UNAUTHENTICATED", "请先登录")
		return
	}
	contentID, reason, ok := decodeRevealRequest(w, r)
	if !ok {
		return
	}
	archive, err := d.Store.ReadDeletedArchive(r.Context(), principal.UserID, contentID, reason)
	switch {
	case errors.Is(err, ErrContentNotFound):
		httpapi.Fail(w, r, 404, "ARCHIVE_NOT_FOUND", "没有该内容的删除归档")
		return
	case errors.Is(err, ErrArchiveExpired):
		// 410 rather than 404: the record existed and its window has closed, which is different information
		// from "there was never an archive".
		httpapi.Fail(w, r, 410, "ARCHIVE_EXPIRED", "已超过30天保留期，原文不可再读取")
		return
	case err != nil:
		d.failDependency(w, r, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, archive)
}

// traceAnonymousAuthor serves POST /api/v1/admin/anonymous-traces.
func (d Deps) traceAnonymousAuthor(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.FromContext(r.Context())
	if !ok {
		httpapi.Fail(w, r, 401, "UNAUTHENTICATED", "请先登录")
		return
	}
	contentID, reason, ok := decodeRevealRequest(w, r)
	if !ok {
		return
	}
	trace, err := d.Store.TraceAnonymousAuthor(r.Context(), principal.UserID, contentID, reason)
	if errors.Is(err, ErrContentNotFound) {
		httpapi.Fail(w, r, 404, "CONTENT_NOT_FOUND", "内容不存在或已删除")
		return
	}
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, trace)
}

// decodeRevealRequest validates the shared body of the two reveal operations.
func decodeRevealRequest(w http.ResponseWriter, r *http.Request) (uint64, string, bool) {
	var request struct {
		ContentID string `json:"content_id"`
		Reason    string `json:"reason"`
	}
	if !decodeJSON(w, r, &request) {
		return 0, "", false
	}
	contentID, err := ParseID(request.ContentID)
	if err != nil {
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "content_id 无效")
		return 0, "", false
	}
	reason := strings.TrimSpace(request.Reason)
	if reason == "" {
		// The reason is the whole accountability of these two endpoints; an empty one is refused rather than
		// defaulted, because a default would be a disclosure nobody asked for.
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "请填写访问原因（会写入审计）")
		return 0, "", false
	}
	if len([]rune(reason)) > auditReasonMax {
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "访问原因过长")
		return 0, "", false
	}
	return contentID, reason, true
}
