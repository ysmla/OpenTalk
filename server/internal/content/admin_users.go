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

// Restrictions (mute and ban) and the account list an administrator works from.
//
// Three properties are deliberate, and each is the answer to a way this feature goes wrong:
//
//   - **A ban revokes the account's sessions in the same transaction.** The login path already refuses a
//     banned account, but a session issued before the ban would otherwise keep working until it expired —
//     which is the difference between "banned" and "banned from now on". Muting deliberately does not: a
//     muted reader may still read and manage their own content, so there is nothing to revoke.
//   - **Revoking writes a time, never a deletion.** The record is the reason a moderation decision can be
//     reviewed later; deleting the row would erase the fact that it happened. Revoked rows stay in the list.
//   - **Every change writes an audit row.** A restriction with no audit trail is indistinguishable from a
//     database edit, which is exactly what this endpoint exists to replace.

// Restriction kinds, as the contract spells them.
const (
	restrictionMute = "mute"
	restrictionBan  = "ban"
)

// Audit actions for this file.
const (
	actionMuteUser          = "mute_user"
	actionBanUser           = "ban_user"
	actionRevokeRestriction = "revoke_restriction"
)

// restrictionReasonMax bounds the stored reason (the column is VARCHAR(1000)).
const restrictionReasonMax = 1000

// adminRestrictionResponse mirrors the AdminRestriction schema.
type adminRestrictionResponse struct {
	ID        string     `json:"id"`
	UserID    string     `json:"user_id"`
	Kind      string     `json:"kind"`
	Reason    string     `json:"reason"`
	StartsAt  time.Time  `json:"starts_at"`
	EndsAt    *time.Time `json:"ends_at,omitempty"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// CreateRestriction records a mute or a ban.
//
// The account must exist: a restriction against a nonexistent id would be an audit row that can never be
// acted on. A ban also revokes every session the account holds, in the same transaction as the restriction
// itself, so there is no window in which the account is banned but still signed in.
func (s Store) CreateRestriction(ctx context.Context, actorID, userID uint64, kind, reason string, endsAt *time.Time) (uint64, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE id=?`, userID).Scan(&exists); err != nil {
		return 0, err
	}
	if exists == 0 {
		return 0, ErrContentNotFound
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO user_restrictions (user_id,kind,reason,starts_at,ends_at,created_by)
VALUES (?,?,?,UTC_TIMESTAMP(6),?,?)`, userID, kind, reason, endsAt, actorID)
	if err != nil {
		return 0, err
	}
	restrictionID, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	if kind == restrictionBan {
		// Sessions are rows: deleting them is what makes the ban take effect now rather than at the next
		// login. A mute keeps them, because a muted reader is still allowed to read.
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=?`, userID); err != nil {
			return 0, err
		}
	}
	action := actionMuteUser
	if kind == restrictionBan {
		action = actionBanUser
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO moderation_actions (actor_id,target_user_id,action,reason)
VALUES (?,?,?,?)`, actorID, userID, action, reason); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return uint64(restrictionID), nil
}

// RevokeRestriction records when a restriction stopped applying. It never deletes the row, and revoking an
// already revoked restriction is a no-op rather than an error: the caller asked for a state, and the state
// already holds.
func (s Store) RevokeRestriction(ctx context.Context, actorID, restrictionID uint64, reason string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var userID uint64
	var alreadyRevoked sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT user_id,revoked_at FROM user_restrictions WHERE id=? FOR UPDATE`, restrictionID).
		Scan(&userID, &alreadyRevoked)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrContentNotFound
	}
	if err != nil {
		return err
	}
	if alreadyRevoked.Valid {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE user_restrictions SET revoked_at=UTC_TIMESTAMP(6) WHERE id=?`, restrictionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO moderation_actions (actor_id,target_user_id,action,reason)
VALUES (?,?,?,?)`, actorID, userID, actionRevokeRestriction, reason); err != nil {
		return err
	}
	return tx.Commit()
}

// RestrictionByID reads one restriction in the administrative shape.
func (s Store) RestrictionByID(ctx context.Context, restrictionID uint64) (adminRestrictionResponse, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id,user_id,kind,reason,starts_at,ends_at,revoked_at
 FROM user_restrictions WHERE id=?`, restrictionID)
	if err != nil {
		return adminRestrictionResponse{}, err
	}
	items, err := scanRows(rows, scanRestriction)
	if err != nil {
		return adminRestrictionResponse{}, err
	}
	if len(items) == 0 {
		return adminRestrictionResponse{}, ErrContentNotFound
	}
	return items[0], nil
}

func scanRestriction(rows *sql.Rows) (adminRestrictionResponse, error) {
	var item adminRestrictionResponse
	var endsAt, revokedAt sql.NullTime
	if err := rows.Scan(&item.ID, &item.UserID, &item.Kind, &item.Reason, &item.StartsAt, &endsAt, &revokedAt); err != nil {
		return item, err
	}
	if endsAt.Valid {
		value := endsAt.Time
		item.EndsAt = &value
	}
	if revokedAt.Valid {
		value := revokedAt.Time
		item.RevokedAt = &value
	}
	return item, nil
}

// ListRestrictions returns the restriction record, newest first. Revoked rows are included: the list is the
// history of decisions, not only the ones in force.
func (s Store) ListRestrictions(ctx context.Context, page pageRequest) ([]adminRestrictionResponse, bool, uint64, error) {
	conditions := []string{"1=1"}
	args := []any{}
	if len(page.After) == 1 {
		conditions = append(conditions, `id<?`)
		args = append(args, page.After[0])
	}
	args = append(args, page.Limit+1)
	rows, err := s.DB.QueryContext(ctx, `SELECT id,user_id,kind,reason,starts_at,ends_at,revoked_at
 FROM user_restrictions WHERE `+strings.Join(conditions, " AND ")+` ORDER BY id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, false, 0, err
	}
	items, err := scanRows(rows, scanRestriction)
	if err != nil {
		return nil, false, 0, err
	}
	hasMore := len(items) > page.Limit
	if hasMore {
		items = items[:page.Limit]
	}
	last := uint64(0)
	if len(items) > 0 {
		last = mustParseID(items[len(items)-1].ID)
	}
	return items, hasMore, last, nil
}

// ListAdminUsers returns the account list, newest first.
//
// It is deliberately narrow: the AdminUser schema is what an administrator needs to identify an account
// (internal id, public profile, whether a ban is in force, role). Email addresses are not in it, and are not
// selected here either — a list that carries every address is a list that leaks every address.
func (s Store) ListAdminUsers(ctx context.Context, page pageRequest) ([]adminUserResponse, bool, uint64, error) {
	conditions := []string{"1=1"}
	args := []any{}
	if len(page.After) == 1 {
		conditions = append(conditions, `u.id<?`)
		args = append(args, page.After[0])
	}
	args = append(args, page.Limit+1)
	rows, err := s.DB.QueryContext(ctx, `SELECT u.id,u.public_id,u.nickname,u.role,u.created_at,
 EXISTS(SELECT 1 FROM user_restrictions r WHERE r.user_id=u.id AND r.kind='ban' AND `+activeRestrictionSQL+`)
 FROM users u WHERE `+strings.Join(conditions, " AND ")+` ORDER BY u.id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, false, 0, err
	}
	users, err := scanRows(rows, func(rows *sql.Rows) (adminUserResponse, error) {
		var user adminUserResponse
		var createdAt time.Time
		var banned bool
		if err := rows.Scan(&user.InternalUserID, &user.Profile.PublicID, &user.Profile.Nickname,
			&user.Role, &createdAt, &banned); err != nil {
			return user, err
		}
		user.Profile.CreatedAt = formatTime(createdAt)
		user.AccountStatus = accountStatusOf(banned)
		return user, nil
	})
	if err != nil {
		return nil, false, 0, err
	}
	hasMore := len(users) > page.Limit
	if hasMore {
		users = users[:page.Limit]
	}
	last := uint64(0)
	if len(users) > 0 {
		last = mustParseID(users[len(users)-1].InternalUserID)
	}
	return users, hasMore, last, nil
}

// createRestriction serves POST /api/v1/admin/restrictions.
func (d Deps) createRestriction(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.FromContext(r.Context())
	if !ok {
		httpapi.Fail(w, r, 401, "UNAUTHENTICATED", "请先登录")
		return
	}
	var request struct {
		UserID string `json:"user_id"`
		Kind   string `json:"kind"`
		Reason string `json:"reason"`
		EndsAt string `json:"ends_at"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	userID, err := ParseID(request.UserID)
	if err != nil {
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "user_id 无效")
		return
	}
	if request.Kind != restrictionMute && request.Kind != restrictionBan {
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "kind 只能是 mute 或 ban")
		return
	}
	reason := strings.TrimSpace(request.Reason)
	if reason == "" {
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "请填写处罚原因")
		return
	}
	if utf8.RuneCountInString(reason) > restrictionReasonMax {
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "处罚原因过长")
		return
	}
	var endsAt *time.Time
	if strings.TrimSpace(request.EndsAt) != "" {
		parsed, err := time.Parse(time.RFC3339, request.EndsAt)
		if err != nil {
			httpapi.Fail(w, r, 422, "INVALID_REQUEST", "ends_at 必须是 RFC3339 时间")
			return
		}
		// An end in the past would create a restriction that is already over, which reads as a mistake rather
		// than as an intention; refusing it is kinder than storing it.
		if !parsed.After(time.Now().UTC()) {
			httpapi.Fail(w, r, 422, "INVALID_REQUEST", "ends_at 必须晚于当前时间；永久处罚请省略该字段")
			return
		}
		utc := parsed.UTC()
		endsAt = &utc
	}
	restrictionID, err := d.Store.CreateRestriction(r.Context(), principal.UserID, userID, request.Kind, reason, endsAt)
	if errors.Is(err, ErrContentNotFound) {
		httpapi.Fail(w, r, 404, "USER_NOT_FOUND", "用户不存在")
		return
	}
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	created, err := d.Store.RestrictionByID(r.Context(), restrictionID)
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	httpapi.JSON(w, http.StatusCreated, created)
}

// revokeRestriction serves DELETE /api/v1/admin/restrictions/{restriction_id}.
func (d Deps) revokeRestriction(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.FromContext(r.Context())
	if !ok {
		httpapi.Fail(w, r, 401, "UNAUTHENTICATED", "请先登录")
		return
	}
	restrictionID, ok := pathID(w, r, "restriction_id")
	if !ok {
		return
	}
	var request struct {
		Reason string `json:"reason"`
	}
	// The contract gives this route no request body; a reason is accepted when a client sends one, and
	// otherwise recorded as an explicit revocation so the audit row is never empty.
	reason := "撤销处罚"
	if r.ContentLength > 0 {
		if !decodeJSON(w, r, &request) {
			return
		}
		if trimmed := strings.TrimSpace(request.Reason); trimmed != "" {
			if utf8.RuneCountInString(trimmed) > restrictionReasonMax {
				httpapi.Fail(w, r, 422, "INVALID_REQUEST", "撤销原因过长")
				return
			}
			reason = trimmed
		}
	}
	err := d.Store.RevokeRestriction(r.Context(), principal.UserID, restrictionID, reason)
	if errors.Is(err, ErrContentNotFound) {
		httpapi.Fail(w, r, 404, "RESTRICTION_NOT_FOUND", "处罚记录不存在")
		return
	}
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// listRestrictions serves GET /api/v1/admin/restrictions.
func (d Deps) listRestrictions(w http.ResponseWriter, r *http.Request) {
	page, ok := d.parsePageOrFail(w, r, restrictionCursorScope)
	if !ok {
		return
	}
	items, hasMore, last, err := d.Store.ListRestrictions(r.Context(), page)
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	view := struct {
		Items      []adminRestrictionResponse `json:"items"`
		HasMore    bool                       `json:"has_more"`
		NextCursor string                     `json:"next_cursor,omitempty"`
	}{Items: items, HasMore: hasMore}
	if hasMore && last != 0 {
		view.NextCursor = encodeCursor(restrictionCursorScope, int64(last))
	}
	httpapi.JSON(w, http.StatusOK, view)
}

// listAdminUsers serves GET /api/v1/admin/users.
func (d Deps) listAdminUsers(w http.ResponseWriter, r *http.Request) {
	page, ok := d.parsePageOrFail(w, r, adminUserCursorScope)
	if !ok {
		return
	}
	users, hasMore, last, err := d.Store.ListAdminUsers(r.Context(), page)
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	view := struct {
		Items      []adminUserResponse `json:"items"`
		HasMore    bool                `json:"has_more"`
		NextCursor string              `json:"next_cursor,omitempty"`
	}{Items: users, HasMore: hasMore}
	if hasMore && last != 0 {
		view.NextCursor = encodeCursor(adminUserCursorScope, int64(last))
	}
	httpapi.JSON(w, http.StatusOK, view)
}

// Cursor scopes for the two administrative lists.
const (
	restrictionCursorScope = "admin-restrictions"
	adminUserCursorScope   = "admin-users"
)
