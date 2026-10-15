package content

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"time"

	"opentalk/server/internal/auth"
	"opentalk/server/internal/media"
	"opentalk/server/internal/platform/httpapi"
)

// maxMultipartOverhead covers the multipart envelope around the file part, so the byte limit itself is
// enforced by the media layer while the whole request stays bounded.
const maxMultipartOverhead = 1 << 20

// promoteAttachments marks newly attached images as in use.
//
// Uploads are stored as 'pending' with a delete_after deadline, because an image that is never attached
// to anything should be swept away. Attaching it has to clear both, otherwise the cleanup job would
// delete the images of a published topic once the pending window expired.
func promoteAttachments(ctx context.Context, tx *sql.Tx, ids []uint64) error {
	unique := dedupe(ids)
	if len(unique) == 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE attachments SET status='active',delete_after=NULL
 WHERE id IN (`+placeholders(len(unique))+`) AND status='pending'`, toAny(unique)...)
	return err
}

// uploadImage accepts one multipart image and records it as a pending attachment owned by the uploader.
// The pending state is not a draft marker: it is what lets the cleanup job tell an abandoned upload from
// one that is now part of published content.
func (d Deps) uploadImage(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.FromContext(r.Context())
	if !ok {
		httpapi.Fail(w, r, 401, "UNAUTHENTICATED", "请先登录")
		return
	}
	if d.Settings == nil || d.Media == nil {
		d.failDependency(w, r, errors.New("media store is not configured"))
		return
	}
	current, err := d.Settings.Load(r.Context())
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	maxBytes := int64(current.MediaMaxBytes)
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes+maxMultipartOverhead)
	if err := r.ParseMultipartForm(maxBytes); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			httpapi.Fail(w, r, 413, "PAYLOAD_TOO_LARGE", "图片超过大小限制")
			return
		}
		httpapi.Fail(w, r, 400, "INVALID_MULTIPART", "上传格式无效")
		return
	}
	// Parsing can spill parts to temporary files, and the server does not clean them up for us.
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()
	file, _, err := r.FormFile("file")
	if err != nil {
		httpapi.Fail(w, r, 422, "MISSING_FILE", "请选择要上传的图片")
		return
	}
	defer func() { _ = file.Close() }()

	stored, err := d.Media.Save(file, media.Limits{
		MaxBytes:     maxBytes,
		MaxDimension: current.MediaMaxDimension,
	})
	if err != nil {
		switch {
		case errors.Is(err, media.ErrTooLarge):
			httpapi.Fail(w, r, 413, "PAYLOAD_TOO_LARGE", "图片超过大小限制")
		case errors.Is(err, media.ErrTooWide):
			httpapi.Fail(w, r, 422, "IMAGE_TOO_LARGE",
				"图片尺寸超过 "+strconv.Itoa(current.MediaMaxDimension)+" 像素限制")
		default:
			// One message for "not an image", "not an accepted format" and "corrupt", so a client learns
			// nothing about which check refused it.
			httpapi.Fail(w, r, 422, "UNSUPPORTED_IMAGE", "只接受 PNG、JPEG、WebP 或 GIF 图片")
		}
		return
	}
	result, err := d.Store.DB.ExecContext(r.Context(), `INSERT INTO attachments
 (owner_id,storage_key,mime_type,byte_size,status,delete_after)
 VALUES (?,?,?,?,'pending',DATE_ADD(UTC_TIMESTAMP(6), INTERVAL ? HOUR))`,
		principal.UserID, stored.Key, stored.MIMEType, stored.ByteSize, current.MediaPendingTTLHours)
	if err != nil {
		// Undo the write: a file with no row is invisible to a cleanup that works from the database, so it
		// would sit on disk forever.
		_ = d.Media.Remove(stored.Key)
		d.failDependency(w, r, err)
		return
	}
	id, err := result.LastInsertId()
	if err != nil {
		_ = d.Media.Remove(stored.Key)
		d.failDependency(w, r, err)
		return
	}
	httpapi.JSON(w, http.StatusCreated, attachmentResponse{
		ID:       strconv.FormatInt(id, 10),
		URL:      mediaURL(stored.Key),
		MIMEType: stored.MIMEType,
		ByteSize: stored.ByteSize,
	})
}

// attachmentReadable decides whether one stored image may be served to an anonymous reader. It is the
// owner-less form of attachmentReadableFor, kept for callers that have no session at all.
func (s Store) attachmentReadable(ctx context.Context, key string) (bool, string, error) {
	return s.attachmentReadableFor(ctx, key, 0)
}

// attachmentReadableFor decides whether one stored image may be served to a specific viewer. A viewerID
// of 0 means an anonymous reader.
//
// Knowing the key is not enough, and neither is the state of the row the image is attached to.
//
//   - A pending image has not been published yet: it exists only so the author can preview what they are
//     about to attach. Only the owner may read it, and only while its pending window lasts. Without the
//     owner check any visitor holding the link could read material the author never published, and the
//     unguessable key would be doing the work of an authorization decision.
//   - An active image is published, so it is readable while the content that shows it is still visible.
//     That means the whole ancestor chain, not just the row that references the image: an answer or a
//     comment is shown inside its topic, so hiding or deleting the root topic has to hide the image too,
//     even though the answer row itself is still 'visible'.
//   - The chain has to be walked to its end, not sampled at its root. A comment on a hidden answer is
//     never displayed, because the whole answer is gone from the page; moderation stops there, so the
//     comment image must stop being served even though the root question is still public. Checking only
//     the root would serve exactly the material an administrator had just removed.
func (s Store) attachmentReadableFor(ctx context.Context, key string, viewerID uint64) (bool, string, error) {
	var attachmentID uint64
	var mimeType string
	var status string
	var ownerID uint64
	var expired bool
	err := s.DB.QueryRowContext(ctx, `SELECT id,mime_type,status,owner_id,
 COALESCE(delete_after IS NOT NULL AND delete_after<=UTC_TIMESTAMP(6), FALSE)
FROM attachments WHERE storage_key=? AND status IN ('pending','active')`, key).
		Scan(&attachmentID, &mimeType, &status, &ownerID, &expired)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	if status == "pending" {
		return !expired && viewerID != 0 && viewerID == ownerID, mimeType, nil
	}
	// Active means it was attached to something, and an image is served while at least one content row
	// showing it is reachable — reachable meaning the whole ancestor graph above it is undeleted and
	// unmoderated, which is the rule in visibility.go.
	//
	// That rule is asked rather than re-implemented. The two used to be separate SQL predicates that had to
	// "stay equivalent" by inspection, and they did not: the image side picked one ancestor per row, so a
	// hidden thread root under a still-public answer left the reply's image public, and it pooled every
	// reference into one chain, so a hidden ancestor of one reference denied an image another public
	// reference still showed. Both were the copy drifting from the rule, not the rule being wrong.
	//
	// So the store answers *all* the questions at once. Every referencing row is looked up, the shared walk
	// decides which of them are reachable, and "at least one reachable reference" is the OR of those
	// answers. A reference whose own row is hidden, or whose ancestor anywhere above it is hidden or
	// deleted, contributes nothing — and its failure cannot reach the other references, because each one
	// was asked about its own chain.
	references, err := collectIDs(ctx, s.DB, `SELECT content_id FROM content_attachments WHERE attachment_id=?`, attachmentID)
	if err != nil {
		return false, "", err
	}
	reachable, err := visibleContentIDs(ctx, s.DB, references)
	if err != nil {
		return false, "", err
	}
	for _, reference := range references {
		if reachable[reference] {
			return true, mimeType, nil
		}
	}
	return false, mimeType, nil
}

// getImage serves one stored image. The route is public because images are embedded in public content,
// but the database decides every request: the key alone grants nothing, and a session turns an anonymous
// reader into the owner for their own not-yet-published uploads.
func (d Deps) getImage(w http.ResponseWriter, r *http.Request) {
	if d.Media == nil {
		d.failDependency(w, r, errors.New("media store is not configured"))
		return
	}
	key := r.PathValue("key")
	// Reading an image must never require a login, so a missing, invalid or expired session is simply an
	// anonymous reader instead of an error. This is resolved on every request, including image loads, so
	// the lookup has to stay a single indexed read of the sessions table.
	viewerID := d.viewerID(r)
	allowed, mimeType, err := d.Store.attachmentReadableFor(r.Context(), key, viewerID)
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	if !allowed {
		// A missing image and an image that must not be shown are the same answer, so the endpoint cannot
		// be used to confirm that a removed image existed.
		httpapi.Fail(w, r, 404, "NOT_FOUND", "图片不存在")
		return
	}
	file, err := d.Media.Open(key)
	if err != nil {
		httpapi.Fail(w, r, 404, "NOT_FOUND", "图片不存在")
		return
	}
	defer func() { _ = file.Close() }()
	w.Header().Set("Content-Type", mimeType)
	// The key never changes content, but a deletion must take effect promptly, so the browser is told to
	// revalidate. Revalidation after a deletion hits the check above and receives 404, which drops it.
	// The response varies by session now, so a shared cache must not reuse an owner's response for a
	// guest — the session cookie is part of the cache key.
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Header().Add("Vary", "Cookie")
	http.ServeContent(w, r, key, time.Time{}, file)
}

// viewerID resolves the optional session behind the request. It reports 0 for every anonymous reader,
// including one whose cookie is stale or malformed: a reader who is not signed in is not an error.
func (d Deps) viewerID(r *http.Request) uint64 {
	if d.Sessions == nil {
		return 0
	}
	raw, ok := auth.RawFromRequest(r)
	if !ok {
		return 0
	}
	principal, err := d.Sessions.Lookup(r.Context(), sha256.Sum256([]byte(raw)))
	if err != nil {
		return 0
	}
	return principal.UserID
}
