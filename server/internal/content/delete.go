package content

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"time"

	"opentalk/server/internal/auth"
	"opentalk/server/internal/platform/httpapi"
)

// ArchiveRetention is the confirmed 30-day window in which an administrator may still review the
// original text of deleted content. The table's CHECK requires purge_after to be exactly this far after
// deleted_at, so it is computed rather than configured.
const ArchiveRetention = 30 * 24 * time.Hour

// DeletedTopicTitle replaces the title of a deleted topic so the original cannot leak through a list.
const DeletedTopicTitle = "已删除的主题"

// DeleteContentInput names the content the author is deleting.
type DeleteContentInput struct {
	ActorID   uint64
	ContentID uint64
}

// DeletionResult is what the endpoint reports.
type DeletionResult struct {
	ContentID     uint64
	AffectedCount int
	DeletedAt     time.Time
}

// DeleteContent soft-deletes the author's own content together with everything the confirmed cascade
// says must go with it.
//
// The scope differs by kind, and each scope was confirmed separately: a topic takes every comment under
// it and, for a question, every answer; an answer takes its own comments; a comment takes the comments
// that reply to it, directly or indirectly, along reply_to_comment_id — which is why that column exists
// — and leaves its ancestors and unrelated branches alone.
//
// The original text is copied into deleted_content_archives before the body is cleared, because clearing
// it is what makes the deletion real (a CHECK requires body_md to be NULL once deleted_at is set) and
// there would be nothing left to review afterwards.
func (s Store) DeleteContent(ctx context.Context, in DeleteContentInput) (DeletionResult, error) {
	result := DeletionResult{ContentID: in.ContentID}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback() }()

	rootID, err := s.rootTopicOf(ctx, in.ContentID)
	if err != nil {
		return result, err
	}
	if _, err := lockRootTopic(ctx, tx, rootID); err != nil {
		return result, err
	}

	var kind string
	var authorID uint64
	err = tx.QueryRowContext(ctx,
		`SELECT kind,author_id FROM contents WHERE id=?`, in.ContentID).
		Scan(&kind, &authorID)
	if errors.Is(err, sql.ErrNoRows) {
		return result, ErrContentNotFound
	}
	if err != nil {
		return result, err
	}
	// Deleting something already deleted or hidden is reported as missing: there is nothing left to act
	// on, and saying "already removed" would confirm that removed content once existed. The same shared
	// ancestor rule decides it, so a hidden answer is as unreachable for its author as it is for a reader
	// — deletion is not a way back into content a moderator took down.
	if err := requireVisible(ctx, tx, in.ContentID); err != nil {
		return result, err
	}
	if authorID != in.ActorID {
		// An administrator may remove content they did not write — that is what moderation needs. It is
		// recorded twice on purpose: deleted_content_archives keeps `deleted_by`, and an audit row states what
		// happened, because a deletion with no explanation is indistinguishable from the author's own.
		if err := requireAdmin(ctx, tx, in.ActorID); err != nil {
			if errors.Is(err, ErrAdminRequired) {
				return result, ErrNotContentOwner
			}
			return result, err
		}
		if err := writeAudit(ctx, tx, in.ActorID, &in.ContentID, nil, actionAdminDelete,
			"管理员删除他人内容 #"+formatID(in.ContentID)); err != nil {
			return result, err
		}
	}
	// Muting does not block deleting your own content: the permission table allows a muted user to manage
	// and delete what they posted, and only forbids new or edited speech.

	ids, err := s.deletionScope(ctx, tx, in.ContentID, kind)
	if err != nil {
		return result, err
	}
	// requirements 42: deletion has to take effect on what has not been sent yet, immediately and without
	// waiting for the cleanup task. A queued announcement of content that is gone has nothing left to say,
	// so it is voided here, in the same transaction that removes the content.
	if err := voidPendingNotifications(ctx, tx, ids); err != nil {
		return result, err
	}
	// One timestamp for the whole cascade, so every archived row shares a deleted_at and the derived
	// purge_after satisfies the table's CHECK exactly.
	now := time.Now().UTC().Truncate(time.Microsecond)
	result.DeletedAt = now
	purgeAfter := now.Add(ArchiveRetention)

	placeholdersFor := placeholders(len(ids))
	args := make([]any, 0, len(ids)+6)
	for _, id := range ids {
		args = append(args, id)
	}
	// Archive first: this SELECT still sees the original title and body. The SELECT's placeholders come
	// before the IN list in the statement, so the scalar values must come first in the argument list.
	archiveArgs := append([]any{in.ActorID, in.ContentID, now, purgeAfter}, args...)
	if _, err := tx.ExecContext(ctx, `INSERT INTO deleted_content_archives
 (content_id,original_title,original_body_md,deleted_by,deletion_root_id,deleted_at,purge_after)
SELECT c.id,t.title,c.body_md,?,?,?,?
FROM contents c LEFT JOIN topics t ON t.content_id=c.id
WHERE c.id IN (`+placeholdersFor+`) AND c.deleted_at IS NULL AND c.body_md IS NOT NULL`, archiveArgs...); err != nil {
		return result, err
	}
	// The body must be cleared in the same statement that sets deleted_at, because the table's CHECK
	// ties the two together.
	softArgs := append(append([]any{}, now), args...)
	update, err := tx.ExecContext(ctx, `UPDATE contents SET deleted_at=?,body_md=NULL
 WHERE id IN (`+placeholdersFor+`) AND deleted_at IS NULL`, softArgs...)
	if err != nil {
		return result, err
	}
	affected, err := update.RowsAffected()
	if err != nil {
		return result, err
	}
	result.AffectedCount = int(affected)
	// A deleted topic must not keep its title, or a board listing would still show it.
	if _, err := tx.ExecContext(ctx, `UPDATE topics SET title=? WHERE content_id IN (`+placeholdersFor+`)`,
		append([]any{DeletedTopicTitle}, args...)...); err != nil {
		return result, err
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	return result, nil
}

// deletionScope collects the content ids the confirmed cascade covers, including the target itself.
func (s Store) deletionScope(ctx context.Context, tx *sql.Tx, contentID uint64, kind string) ([]uint64, error) {
	ids := []uint64{contentID}
	switch kind {
	case "topic":
		// Every comment under this discussion: the column carries the root topic even for comments on an
		// answer, so one query covers both levels.
		comments, err := collectIDs(ctx, tx, `SELECT content_id FROM comments WHERE topic_id=?`, contentID)
		if err != nil {
			return nil, err
		}
		ids = append(ids, comments...)
		// For a question, every answer as well.
		answers, err := collectIDs(ctx, tx, `SELECT content_id FROM answers WHERE question_id=?`, contentID)
		if err != nil {
			return nil, err
		}
		ids = append(ids, answers...)
	case "answer":
		comments, err := collectIDs(ctx, tx, `SELECT content_id FROM comments WHERE answer_id=?`, contentID)
		if err != nil {
			return nil, err
		}
		ids = append(ids, comments...)
	case "comment":
		// Walk reply_to_comment_id outwards. This is the reason the column exists: it is the real reply
		// tree, while parent_comment_id only groups a thread for a two-level display.
		descendants, err := collectIDs(ctx, tx, `WITH RECURSIVE descendants (content_id) AS (
 SELECT content_id FROM comments WHERE reply_to_comment_id=?
 UNION ALL
 SELECT c.content_id FROM comments c JOIN descendants d ON c.reply_to_comment_id=d.content_id)
SELECT content_id FROM descendants`, contentID)
		if err != nil {
			return nil, err
		}
		ids = append(ids, descendants...)
	default:
		return nil, ErrContentNotFound
	}
	return dedupe(ids), nil
}

func collectIDs(ctx context.Context, q queryer, query string, args ...any) ([]uint64, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return scanRows(rows, func(rows *sql.Rows) (uint64, error) {
		var id uint64
		return id, rows.Scan(&id)
	})
}

// deleteResponse mirrors DeletionResult.
type deleteResponse struct {
	ContentID     string `json:"content_id"`
	AffectedCount int    `json:"affected_count"`
	DeletedAt     string `json:"deleted_at"`
}

// deleteContent removes the author's own content and everything that must go with it.
func (d Deps) deleteContent(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.FromContext(r.Context())
	if !ok {
		httpapi.Fail(w, r, 401, "UNAUTHENTICATED", "请先登录")
		return
	}
	contentID, err := ParseID(r.PathValue("content_id"))
	if err != nil {
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "内容ID无效")
		return
	}
	result, err := d.Store.DeleteContent(r.Context(), DeleteContentInput{
		ActorID:   principal.UserID,
		ContentID: contentID,
	})
	if err != nil {
		if d.failCreate(w, r, err) {
			return
		}
		return
	}
	httpapi.JSON(w, http.StatusOK, deleteResponse{
		ContentID:     strconv.FormatUint(result.ContentID, 10),
		AffectedCount: result.AffectedCount,
		DeletedAt:     formatTime(result.DeletedAt),
	})
}
