package content

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"time"

	"opentalk/server/internal/auth"
	"opentalk/server/internal/markdown"
	"opentalk/server/internal/platform/httpapi"
)

// EditWindow is the confirmed window in which an author may still edit their own content.
const EditWindow = 30 * time.Minute

var (
	ErrNotContentOwner  = errors.New("actor is not the content owner")
	ErrEditWindowClosed = errors.New("the edit window has closed")
	ErrLockedEdit       = errors.New("a locked topic forbids editing")
	ErrTitleOnNonTopic  = errors.New("a title can only be edited on a topic")
	ErrContentNotFound  = errors.New("content not found")
)

// EditContentInput is validated input for editing the author's own content.
type EditContentInput struct {
	ActorID     uint64
	ContentID   uint64
	BodyMD      string
	Title       string
	NewTitle    bool
	Attachments []uint64
	NewAttach   bool
	MaxAttach   int
}

// EditContent updates the body of the author's own content, and optionally the title of a topic and the
// set of attached images.
//
// The four permissions that apply here are deliberately separate checks: visibility, ownership, the
// 30-minute window, and the locked-topic rule. Authorization is not a moderation action, so an
// administrator gets no shortcut through them.
func (s Store) EditContent(ctx context.Context, in EditContentInput) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// The uniform lock order is root topic first, so the root is located without a lock and then locked
	// before the target row is trusted. Everything read before the lock is re-validated after it.
	rootID, err := s.rootTopicOf(ctx, in.ContentID)
	if err != nil {
		return err
	}
	root, err := lockRootTopic(ctx, tx, rootID)
	if err != nil {
		return err
	}
	// A hidden or deleted root makes the target unreachable. It is reported as missing content rather
	// than as a missing topic, because from the reader's side the content is what disappeared.
	if root.Deleted || root.Hidden {
		return ErrContentNotFound
	}

	var kind string
	var authorID uint64
	var createdAt time.Time
	err = tx.QueryRowContext(ctx,
		`SELECT kind,author_id,created_at FROM contents WHERE id=?`, in.ContentID).
		Scan(&kind, &authorID, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrContentNotFound
	}
	if err != nil {
		return err
	}
	// Visibility is the shared ancestor-chain rule, checked after the lock so a row hidden between
	// locating the root and taking the lock is refused rather than edited. Checking the target row and
	// the root separately was the bug: it left a hidden answer editable inside a visible question.
	if err := requireVisible(ctx, tx, in.ContentID); err != nil {
		return err
	}
	if authorID != in.ActorID {
		return ErrNotContentOwner
	}
	if err := refuseMuted(ctx, tx, in.ActorID); err != nil {
		return err
	}
	// The window is measured from publication and never slides: editing does not extend it.
	if time.Since(createdAt) > EditWindow {
		return ErrEditWindowClosed
	}
	// A locked discussion forbids editing the body. body_md is mandatory in the request, so refusing the
	// body edit refuses the whole edit; that also keeps a locked discussion's text frozen, which is the
	// point of locking it.
	if root.IsLocked {
		return ErrLockedEdit
	}
	if in.NewTitle && kind != "topic" {
		return ErrTitleOnNonTopic
	}
	if in.NewAttach {
		if in.MaxAttach > 0 && len(in.Attachments) > in.MaxAttach {
			return ErrTooManyAttachments
		}
		if err := requireAllAttachments(ctx, tx, in.Attachments, in.ActorID); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE contents SET body_md=?, edited_at=UTC_TIMESTAMP(6) WHERE id=?`, in.BodyMD, in.ContentID); err != nil {
		return err
	}
	if in.NewTitle {
		if _, err := tx.ExecContext(ctx, `UPDATE topics SET title=? WHERE content_id=?`, in.Title, in.ContentID); err != nil {
			return err
		}
	}
	if in.NewAttach {
		// The request replaces the set: an empty array means "no images", which is why the contract
		// distinguishes an omitted field from an empty one.
		if _, err := tx.ExecContext(ctx, `DELETE FROM content_attachments WHERE content_id=?`, in.ContentID); err != nil {
			return err
		}
		for _, attachmentID := range dedupe(in.Attachments) {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO content_attachments (content_id,attachment_id) VALUES (?,?)`,
				in.ContentID, attachmentID); err != nil {
				return err
			}
		}
		if err := promoteAttachments(ctx, tx, in.Attachments); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PublicContentByID reads one content row and returns the response for its kind, so a caller does not
// have to know whether an id names a topic, an answer or a comment.
//
// Visibility is resolved before the kind is trusted, so a hidden or deleted row — or one whose ancestor
// chain is broken — reports the same not-found as an id that was never issued.
func (s Store) PublicContentByID(ctx context.Context, contentID uint64) (any, error) {
	if err := requireVisible(ctx, s.DB, contentID); err != nil {
		return nil, err
	}
	var kind string
	err := s.DB.QueryRowContext(ctx, `SELECT kind FROM contents WHERE id=?`, contentID).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrContentNotFound
	}
	if err != nil {
		return nil, err
	}
	switch kind {
	case "topic":
		view, err := s.TopicByContentID(ctx, contentID)
		if err != nil {
			return nil, err
		}
		return toTopicResponse(view), nil
	case "answer":
		view, err := s.AnswerByContentID(ctx, contentID)
		if err != nil {
			return nil, err
		}
		return toAnswerResponse(view), nil
	case "comment":
		view, err := s.CommentByContentID(ctx, contentID)
		if err != nil {
			return nil, err
		}
		return toCommentResponse(view), nil
	default:
		return nil, ErrContentNotFound
	}
}

// editContentRequest mirrors EditContentRequest. The pointers distinguish an omitted field from an empty
// one: an absent attachment_ids keeps the current images, while [] removes all of them.
type editContentRequest struct {
	BodyMD        string    `json:"body_md"`
	Title         *string   `json:"title"`
	AttachmentIDs *[]string `json:"attachment_ids"`
}

// editContent updates the author's own content.
func (d Deps) editContent(w http.ResponseWriter, r *http.Request) {
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
	var request editContentRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	body, err := markdown.ValidateBody(request.BodyMD)
	if err != nil {
		code, message := bodyFailure(err)
		httpapi.Fail(w, r, 422, code, message)
		return
	}
	input := EditContentInput{ActorID: principal.UserID, ContentID: contentID, BodyMD: body}
	if request.Title != nil {
		title, err := markdown.ValidateTitle(*request.Title)
		if err != nil {
			code, message := titleFailure(err)
			httpapi.Fail(w, r, 422, code, message)
			return
		}
		input.Title, input.NewTitle = title, true
	}
	if request.AttachmentIDs != nil {
		attachments, err := ParseIDs(*request.AttachmentIDs)
		if err != nil {
			httpapi.Fail(w, r, 422, "INVALID_REQUEST", "图片ID无效")
			return
		}
		if d.Settings == nil {
			d.failDependency(w, r, errors.New("settings store is not configured"))
			return
		}
		current, err := d.Settings.Load(r.Context())
		if err != nil {
			d.failDependency(w, r, err)
			return
		}
		if len(attachments) > current.MediaMaxPerContent {
			httpapi.Fail(w, r, 422, "TOO_MANY_ATTACHMENTS",
				"每篇内容最多关联 "+strconv.Itoa(current.MediaMaxPerContent)+" 张图片")
			return
		}
		input.Attachments, input.NewAttach, input.MaxAttach = attachments, true, current.MediaMaxPerContent
	}
	if err := d.Store.EditContent(r.Context(), input); err != nil {
		if d.failCreate(w, r, err) {
			return
		}
		return
	}
	response, err := d.Store.PublicContentByID(r.Context(), contentID)
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, response)
}
