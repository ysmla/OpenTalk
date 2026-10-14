package content

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"opentalk/server/internal/media"
)

// CleanupReport summarises one cleanup run.
type CleanupReport struct {
	PendingRemoved int
	OrphanRemoved  int
	ArchivesPurged int
	Failures       []string
}

// CleanupOptions carries the administrator-configured windows.
type CleanupOptions struct {
	PendingTTLHours   int
	OrphanCleanupDays int
	Media             *media.Store
}

// Cleanup removes abandoned images and expires archived original text.
//
// Each phase marks its intent before doing the slow part. An attachment whose file has not been removed
// yet is left in status 'pending_delete' rather than being recorded as deleted, so a crash or a failed
// unlink is retried on the next run instead of silently leaving an orphan file that no query can see.
// That is why the enum has three states and not two.
func (s Store) Cleanup(ctx context.Context, options CleanupOptions) (CleanupReport, error) {
	var report CleanupReport
	if options.Media == nil {
		return report, errors.New("media store is not configured")
	}
	if options.PendingTTLHours <= 0 {
		options.PendingTTLHours = 24
	}
	if options.OrphanCleanupDays <= 0 {
		options.OrphanCleanupDays = 7
	}

	// Phase 1: uploads that were never attached to anything. Rows already in pending_delete are included,
	// which is what makes a previous failure retryable.
	if err := s.cleanupAttachments(ctx, mediaCleanupQuery(`
  status IN ('pending','pending_delete')
  AND delete_after IS NOT NULL AND delete_after<=UTC_TIMESTAMP(6)
  AND NOT EXISTS (SELECT 1 FROM content_attachments ca WHERE ca.attachment_id=attachments.id)`),
		options, &report, "pending"); err != nil {
		return report, err
	}

	// Phase 2: images whose content was deleted, once the orphan window has passed.
	//
	// An image still referenced by an un-expired archive is kept: the archive exists so an administrator
	// can review the original material, and removing the images before that review would defeat it. The
	// attachment therefore goes when the archive expires in phase 3, not before.
	if err := s.cleanupAttachments(ctx, mediaCleanupQuery(fmt.Sprintf(`
  status IN ('active','pending_delete')
  AND EXISTS (SELECT 1 FROM content_attachments ca JOIN contents c ON c.id=ca.content_id
    WHERE ca.attachment_id=attachments.id AND c.deleted_at IS NOT NULL
    AND c.deleted_at <= DATE_SUB(UTC_TIMESTAMP(6), INTERVAL %d DAY))
  AND NOT EXISTS (SELECT 1 FROM content_attachments ca JOIN contents c ON c.id=ca.content_id
    WHERE ca.attachment_id=attachments.id AND c.deleted_at IS NULL)
  AND NOT EXISTS (SELECT 1 FROM content_attachments ca
    JOIN deleted_content_archives dca ON dca.content_id=ca.content_id
    WHERE ca.attachment_id=attachments.id AND dca.purge_after>UTC_TIMESTAMP(6))`,
		options.OrphanCleanupDays)),
		options, &report, "orphan"); err != nil {
		return report, err
	}

	// Phase 3: the retained original text. original_body_md is NOT NULL, so an expired archive is
	// emptied rather than nulled; the title is nullable and is cleared outright.
	result, err := s.DB.ExecContext(ctx, `UPDATE deleted_content_archives
 SET original_body_md='', original_title=NULL
 WHERE purge_after<=UTC_TIMESTAMP(6) AND original_body_md<>''`)
	if err != nil {
		return report, err
	}
	purged, err := result.RowsAffected()
	if err != nil {
		return report, err
	}
	report.ArchivesPurged = int(purged)
	return report, nil
}

func mediaCleanupQuery(condition string) string {
	return `SELECT id,storage_key FROM attachments WHERE` + condition
}

// cleanupAttachments marks candidates, unlinks their files, and only then records them as deleted.
func (s Store) cleanupAttachments(ctx context.Context, query string, options CleanupOptions, report *CleanupReport, kind string) error {
	rows, err := s.DB.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	candidates, err := scanRows(rows, func(rows *sql.Rows) (struct {
		ID  uint64
		Key string
	}, error) {
		var item struct {
			ID  uint64
			Key string
		}
		return item, rows.Scan(&item.ID, &item.Key)
	})
	if err != nil {
		return err
	}
	for _, candidate := range candidates {
		// Step one: commit the intent. From here on the row is retryable even if this process dies.
		if _, err := s.DB.ExecContext(ctx,
			`UPDATE attachments SET status='pending_delete' WHERE id=? AND status<>'deleted'`,
			candidate.ID); err != nil {
			return err
		}
		// Step two: the file. A missing file counts as success, because the goal is "not on disk".
		if err := options.Media.Remove(candidate.Key); err != nil {
			// Left in pending_delete on purpose: the next run picks it up again.
			report.Failures = append(report.Failures,
				fmt.Sprintf("attachment %d: cannot remove file: %v", candidate.ID, err))
			continue
		}
		// Step three: only now is it actually gone.
		if _, err := s.DB.ExecContext(ctx,
			`UPDATE attachments SET status='deleted',delete_after=NULL WHERE id=?`, candidate.ID); err != nil {
			return err
		}
		if kind == "pending" {
			report.PendingRemoved++
		} else {
			report.OrphanRemoved++
		}
	}
	return nil
}

// CleanupReportLine renders one run for the operator.
func (r CleanupReport) String() string {
	line := fmt.Sprintf("待用附件清除 %d、孤儿附件清除 %d、归档原文清理 %d",
		r.PendingRemoved, r.OrphanRemoved, r.ArchivesPurged)
	if len(r.Failures) > 0 {
		line += fmt.Sprintf("；失败 %d 项（已保留为待删除状态，下次运行重试）", len(r.Failures))
	}
	return line
}

// Timeout guards one cleanup run so a scheduled invocation cannot hang forever.
const CleanupTimeout = 10 * time.Minute
