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

// NewComment is validated input for a comment or a reply. AnswerID is zero for a comment on the topic
// itself; ReplyToCommentID is zero for a top-level comment.
type NewComment struct {
	AuthorID           uint64
	TopicID            uint64
	AnswerID           uint64
	ReplyToCommentID   uint64
	BodyMD             string
	RequestedAnonymous bool
	AttachmentIDs      []uint64
	MaxAttachments     int
}

// CreateComment writes a comment or reply under the discussion's root lock.
//
// A reply carries both a thread root and the comment it answers. The schema requires both or neither,
// and the two composite foreign keys keep the thread inside one topic; what they cannot express is the
// answer a reply belongs to, because a NULL answer_id is not covered by a composite foreign key. That
// comparison therefore has to happen here.
func (s Store) CreateComment(ctx context.Context, in NewComment) (uint64, error) {
	if in.MaxAttachments > 0 && len(in.AttachmentIDs) > in.MaxAttachments {
		return 0, ErrTooManyAttachments
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	root, err := lockRootTopic(ctx, tx, in.TopicID)
	if err != nil {
		return 0, err
	}
	// Solved, locked, hidden and deleted are all decided under the root lock, so a comment that races
	// the "mark solved" click fails rather than slipping through.
	if err := guardNewInteraction(root); err != nil {
		return 0, err
	}
	if err := refuseMuted(ctx, tx, in.AuthorID); err != nil {
		return 0, err
	}
	if in.AnswerID != 0 {
		visible, err := answerBelongsToTopic(ctx, tx, in.AnswerID, in.TopicID)
		if err != nil {
			return 0, err
		}
		if !visible {
			return 0, ErrCommentTarget
		}
	}
	var parent, replyTo any
	// The author of the comment being answered, when this is a reply: the notification for a reply goes to
	// them, so the id has to survive the block that reads the target.
	var targetAuthor uint64
	if in.ReplyToCommentID != 0 {
		target, err := loadReplyTarget(ctx, tx, in.ReplyToCommentID, in.TopicID)
		if err != nil {
			return 0, err
		}
		targetAuthor = target.AuthorID
		// 不能跨目标回复: a reply must stay under the same answer as the comment it answers. Both
		// directions matter, so a topic-level comment cannot answer an answer-level one and vice versa.
		if !sameAnswer(target.AnswerID, in.AnswerID) {
			return 0, ErrReplyTarget
		}
		replyTo = in.ReplyToCommentID
		if target.ParentID.Valid {
			// Answering a reply keeps the original thread root, which is what the two-level display needs.
			parent = uint64(target.ParentID.Int64)
		} else {
			parent = in.ReplyToCommentID
		}
	}
	if err := requireAllAttachments(ctx, tx, in.AttachmentIDs, in.AuthorID); err != nil {
		return 0, err
	}
	anonymous, _ := resolveIdentity(root, in.AuthorID, in.RequestedAnonymous)

	result, err := tx.ExecContext(ctx,
		`INSERT INTO contents (author_id,kind,body_md,is_anonymous) VALUES (?,'comment',?,?)`,
		in.AuthorID, in.BodyMD, anonymous)
	if err != nil {
		return 0, err
	}
	lastID, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	contentID := uint64(lastID)

	var answerArg any
	if in.AnswerID != 0 {
		answerArg = in.AnswerID
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO comments (content_id,topic_id,answer_id,parent_comment_id,reply_to_comment_id)
 VALUES (?,?,?,?,?)`, contentID, in.TopicID, answerArg, parent, replyTo); err != nil {
		return 0, err
	}
	if anonymous {
		identityID, _, err := ensureAnonymousIdentity(ctx, tx, in.TopicID, in.AuthorID)
		if err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE contents SET anonymous_identity_id=? WHERE id=?`,
			identityID, contentID); err != nil {
			return 0, err
		}
	}
	for _, attachmentID := range dedupe(in.AttachmentIDs) {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO content_attachments (content_id,attachment_id) VALUES (?,?)`,
			contentID, attachmentID); err != nil {
			return 0, err
		}
	}
	if err := promoteAttachments(ctx, tx, in.AttachmentIDs); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE topics SET last_activity_at=UTC_TIMESTAMP(6) WHERE content_id=?`,
		in.TopicID); err != nil {
		return 0, err
	}
	// Who hears about this comment is the person it addresses, and only them: the author of the comment
	// being answered, otherwise the author of the answer it hangs from, otherwise the topic author. Each
	// level already told its own author about the content below it, so notifying the whole chain here would
	// send the same reader several messages for one reply. The actor is filtered out inside.
	recipients, err := commentRecipients(ctx, tx, root.AuthorID, in, targetAuthor)
	if err != nil {
		return 0, err
	}
	eventType := eventNewComment
	if in.ReplyToCommentID != 0 {
		eventType = eventNewReply
	}
	if err := notifyRecipients(ctx, tx, eventType, contentID, in.AuthorID, recipients); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return contentID, nil
}

// commentRecipients picks the one person a comment or reply addresses.
func commentRecipients(ctx context.Context, tx *sql.Tx, topicAuthorID uint64, in NewComment, replyTargetAuthor uint64) ([]uint64, error) {
	switch {
	case in.ReplyToCommentID != 0:
		return []uint64{replyTargetAuthor}, nil
	case in.AnswerID != 0:
		author, err := contentAuthor(ctx, tx, in.AnswerID)
		if err != nil {
			return nil, err
		}
		return []uint64{author}, nil
	default:
		return []uint64{topicAuthorID}, nil
	}
}

// contentAuthor reads the account behind one content row. It is used for notification routing only, and
// never leaves the server: what the recipient sees is the anonymous or named author the payload rules
// allow, not this id.
func contentAuthor(ctx context.Context, q queryer, contentID uint64) (uint64, error) {
	var author uint64
	if err := q.QueryRowContext(ctx, `SELECT author_id FROM contents WHERE id=?`, contentID).Scan(&author); err != nil {
		return 0, err
	}
	return author, nil
}

// answerBelongsToTopic reports whether an answer is reachable inside a specific topic: the shared
// ancestor rule decides whether it can be reached at all, and the question id decides whether this is the
// topic it hangs from. Both are needed — a visible answer belonging to another topic is not a target here.
func answerBelongsToTopic(ctx context.Context, tx *sql.Tx, answerID, topicID uint64) (bool, error) {
	if err := requireVisible(ctx, tx, answerID); err != nil {
		if errors.Is(err, ErrContentNotFound) {
			return false, nil
		}
		return false, err
	}
	var belongs bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(
 SELECT 1 FROM answers WHERE content_id=? AND question_id=?)`, answerID, topicID).Scan(&belongs)
	return belongs, err
}

// replyTarget is what a reply needs to know about the comment it answers. The author is carried so the
// reply can notify them: the notification goes to the person being answered, not to everyone above them.
type replyTarget struct {
	AnswerID sql.NullInt64
	ParentID sql.NullInt64
	AuthorID uint64
}

// loadReplyTarget reads the comment a reply answers. A hidden or deleted target — or one whose own
// ancestor chain is broken — is not a reply target, so a reply cannot be attached to a thread a reader
// cannot reach.
func loadReplyTarget(ctx context.Context, tx *sql.Tx, commentID, topicID uint64) (replyTarget, error) {
	var target replyTarget
	if err := requireVisible(ctx, tx, commentID); err != nil {
		if errors.Is(err, ErrContentNotFound) {
			return target, ErrReplyTarget
		}
		return target, err
	}
	err := tx.QueryRowContext(ctx, `SELECT cm.answer_id,cm.parent_comment_id,c.author_id
FROM comments cm JOIN contents c ON c.id=cm.content_id
WHERE cm.content_id=? AND cm.topic_id=?`,
		commentID, topicID).Scan(&target.AnswerID, &target.ParentID, &target.AuthorID)
	if errors.Is(err, sql.ErrNoRows) {
		return target, ErrReplyTarget
	}
	if err != nil {
		return target, err
	}
	return target, nil
}

// sameAnswer compares a reply target's answer with the requested one, treating zero as "the topic
// itself". Comparing nullability explicitly is the point: a NULL answer_id escapes the composite
// foreign key, so without this a topic-level reply could be attached to an answer-level thread.
func sameAnswer(target sql.NullInt64, requested uint64) bool {
	if requested == 0 {
		return !target.Valid
	}
	return target.Valid && uint64(target.Int64) == requested
}

// CommentView is one comment assembled for a response.
type CommentView struct {
	ContentID       uint64
	TopicID         uint64
	AnswerID        sql.NullInt64
	ParentID        sql.NullInt64
	ReplyToID       sql.NullInt64
	AuthorID        uint64
	TopicAuthorID   uint64
	BodyMD          string
	IsAnonymous     bool
	AnonymousNumber int
	AuthorPublicID  string
	AuthorNickname  string
	AuthorAvatarKey string
	AuthorCreatedAt time.Time
	CreatedAt       time.Time
	EditedAt        sql.NullTime
	Attachments     []Attachment
}

// CommentByContentID assembles one comment. Visibility is the shared ancestor rule, so a comment under a
// hidden answer or a hidden thread root is reported as not found.
func (s Store) CommentByContentID(ctx context.Context, contentID uint64) (CommentView, error) {
	if err := requireVisible(ctx, s.DB, contentID); err != nil {
		return CommentView{}, err
	}
	var view CommentView
	// The column list and joins live in read.go so the comment list and this reader cannot drift apart.
	view, err := scanCommentView(s.DB.QueryRowContext(ctx, commentViewQuery+` WHERE c.id=?`, contentID))
	if errors.Is(err, sql.ErrNoRows) {
		return view, ErrTopicNotFound
	}
	if err != nil {
		return view, err
	}
	if view.Attachments, err = s.topicAttachments(ctx, contentID); err != nil {
		return view, err
	}
	return view, nil
}

// commentResponse mirrors the Comment schema. The thread ids are omitted at the top level, which is
// what the schema documents, so a two-level display can tell a top-level comment from a reply.
type commentResponse struct {
	ID          string               `json:"id"`
	State       string               `json:"state"`
	Author      authorResponse       `json:"author"`
	BodyMD      string               `json:"body_md"`
	Attachments []attachmentResponse `json:"attachments"`
	CreatedAt   string               `json:"created_at"`
	EditedAt    string               `json:"edited_at,omitempty"`
	Kind        string               `json:"kind"`
	TopicID     string               `json:"topic_id"`
	AnswerID    string               `json:"answer_id,omitempty"`
	ParentID    string               `json:"parent_comment_id,omitempty"`
	ReplyToID   string               `json:"reply_to_comment_id,omitempty"`
}

func toCommentResponse(view CommentView) commentResponse {
	response := commentResponse{
		ID:          strconv.FormatUint(view.ContentID, 10),
		State:       "published",
		Author:      renderAuthor(view.IsAnonymous, view.AuthorID == view.TopicAuthorID, view.AnonymousNumber, view.AuthorPublicID, view.AuthorNickname, view.AuthorAvatarKey, view.AuthorCreatedAt),
		BodyMD:      view.BodyMD,
		Attachments: make([]attachmentResponse, 0, len(view.Attachments)),
		CreatedAt:   formatTime(view.CreatedAt),
		Kind:        "comment",
		TopicID:     strconv.FormatUint(view.TopicID, 10),
	}
	if view.EditedAt.Valid {
		response.EditedAt = formatTime(view.EditedAt.Time)
	}
	if view.AnswerID.Valid {
		response.AnswerID = strconv.FormatInt(view.AnswerID.Int64, 10)
	}
	if view.ParentID.Valid {
		response.ParentID = strconv.FormatInt(view.ParentID.Int64, 10)
	}
	if view.ReplyToID.Valid {
		response.ReplyToID = strconv.FormatInt(view.ReplyToID.Int64, 10)
	}
	for _, attachment := range view.Attachments {
		response.Attachments = append(response.Attachments, attachmentResponse{
			ID:       strconv.FormatUint(attachment.ID, 10),
			URL:      mediaURL(attachment.Key),
			MIMEType: attachment.MIMEType,
			ByteSize: attachment.ByteSize,
		})
	}
	return response
}

// createCommentRequest mirrors CreateCommentRequest.
type createCommentRequest struct {
	TopicID       string   `json:"topic_id"`
	AnswerID      string   `json:"answer_id"`
	ReplyToID     string   `json:"reply_to_comment_id"`
	BodyMD        string   `json:"body_md"`
	AttachmentIDs []string `json:"attachment_ids"`
	IsAnonymous   bool     `json:"is_anonymous"`
}

// createComment publishes a comment on a topic, a comment on an answer, or a reply.
func (d Deps) createComment(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.FromContext(r.Context())
	if !ok {
		httpapi.Fail(w, r, 401, "UNAUTHENTICATED", "请先登录")
		return
	}
	var request createCommentRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	topicID, err := ParseID(request.TopicID)
	if err != nil {
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "主题ID无效")
		return
	}
	// An absent optional id is zero, which the store reads as "not present"; a malformed one is an error.
	var answerID, replyToID uint64
	if request.AnswerID != "" {
		if answerID, err = ParseID(request.AnswerID); err != nil {
			httpapi.Fail(w, r, 422, "INVALID_REQUEST", "回答ID无效")
			return
		}
	}
	if request.ReplyToID != "" {
		if replyToID, err = ParseID(request.ReplyToID); err != nil {
			httpapi.Fail(w, r, 422, "INVALID_REQUEST", "回复目标ID无效")
			return
		}
	}
	body, err := markdown.ValidateBody(request.BodyMD)
	if err != nil {
		code, message := bodyFailure(err)
		httpapi.Fail(w, r, 422, code, message)
		return
	}
	attachmentIDs, err := ParseIDs(request.AttachmentIDs)
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
	if len(attachmentIDs) > current.MediaMaxPerContent {
		httpapi.Fail(w, r, 422, "TOO_MANY_ATTACHMENTS",
			"每篇内容最多关联 "+strconv.Itoa(current.MediaMaxPerContent)+" 张图片")
		return
	}
	commentID, err := d.Store.CreateComment(r.Context(), NewComment{
		AuthorID:           principal.UserID,
		TopicID:            topicID,
		AnswerID:           answerID,
		ReplyToCommentID:   replyToID,
		BodyMD:             body,
		RequestedAnonymous: request.IsAnonymous,
		AttachmentIDs:      attachmentIDs,
		MaxAttachments:     current.MediaMaxPerContent,
	})
	if err != nil {
		if d.failCreate(w, r, err) {
			return
		}
		return
	}
	view, err := d.Store.CommentByContentID(r.Context(), commentID)
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	httpapi.JSON(w, http.StatusCreated, toCommentResponse(view))
}
