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

var (
	ErrTopicLocked   = errors.New("topic is locked")
	ErrTopicSolved   = errors.New("topic is solved")
	ErrNotAQuestion  = errors.New("topic is not a question")
	ErrReplyTarget   = errors.New("reply target is not in this thread")
	ErrCommentTarget = errors.New("comment target is invalid")
)

// rootTopic is the discussion root, read under its own lock.
type rootTopic struct {
	ContentID   uint64
	BoardID     uint64
	AuthorID    uint64
	IsAnonymous bool
	IsLocked    bool
	Deleted     bool
	Hidden      bool
	IsQuestion  bool
	Resolution  string
}

// lockRootTopic takes the discussion's root lock. Every write that touches a discussion locks the root
// topic row first, which is the uniform lock order the design documents: it is what stops "mark solved"
// or "lock" from interleaving with "add a reply", so a submission that races a state change either
// sees the new state or made the state change wait. FOR UPDATE also locks the joined contents and
// questions rows — exactly the rows the competing operations need to read before they write.
func lockRootTopic(ctx context.Context, tx *sql.Tx, topicID uint64) (rootTopic, error) {
	var root rootTopic
	var deletedAt sql.NullTime
	var moderation string
	var resolution sql.NullString
	err := tx.QueryRowContext(ctx, `
SELECT t.content_id,t.board_id,t.is_locked,c.author_id,c.is_anonymous,c.deleted_at,c.moderation_status,
 q.resolution_status
FROM topics t
JOIN contents c ON c.id=t.content_id
LEFT JOIN questions q ON q.topic_id=t.content_id
WHERE t.content_id=? FOR UPDATE`, topicID).
		Scan(&root.ContentID, &root.BoardID, &root.IsLocked, &root.AuthorID, &root.IsAnonymous,
			&deletedAt, &moderation, &resolution)
	if errors.Is(err, sql.ErrNoRows) {
		return root, ErrTopicNotFound
	}
	if err != nil {
		return root, err
	}
	root.Deleted = deletedAt.Valid
	root.Hidden = moderation != "visible"
	root.IsQuestion = resolution.Valid
	root.Resolution = resolution.String
	return root, nil
}

// guardNewInteraction applies the rules that every new piece of interaction shares. A deleted or hidden
// discussion is reported as missing rather than as forbidden, so the endpoint cannot be used to confirm
// that removed material once existed.
func guardNewInteraction(root rootTopic) error {
	if root.Deleted || root.Hidden {
		return ErrTopicNotFound
	}
	if root.IsLocked {
		return ErrTopicLocked
	}
	// A solved question accepts no new answers, comments or replies from anyone, including the asker and
	// administrators, until it is reopened. Reading the status under the root lock is what makes a
	// submission that races the "mark solved" click fail instead of slipping through.
	if root.Resolution == "solved" {
		return ErrTopicSolved
	}
	return nil
}

// resolveIdentity decides whether new content is anonymous.
//
// The request flag is only a request. In an anonymous discussion the topic author's own answers,
// comments and replies are forced anonymous — the contract says so explicitly — because letting the
// author opt out would defeat the anonymity of the discussion they started and would let a self-answer
// expose them.
func resolveIdentity(root rootTopic, actorID uint64, requested bool) (anonymous, isTopicOwner bool) {
	isTopicOwner = actorID == root.AuthorID
	if root.IsAnonymous && isTopicOwner {
		return true, true
	}
	return requested, isTopicOwner
}

// ensureAnonymousIdentity returns the actor's stable number within this discussion, allocating the next
// one only on first use. Stability is a requirement: the same person must keep the same 匿名#N for the
// whole discussion, so an existing mapping is always reused.
func ensureAnonymousIdentity(ctx context.Context, tx *sql.Tx, topicID, userID uint64) (uint64, int, error) {
	var id uint64
	var number int
	err := tx.QueryRowContext(ctx,
		`SELECT id,display_number FROM anonymous_identities WHERE topic_id=? AND user_id=?`,
		topicID, userID).Scan(&id, &number)
	if err == nil {
		return id, number, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, 0, err
	}
	// The unique key on (topic_id, display_number) is the real guard, so two concurrent allocations
	// cannot take the same number.
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(display_number),0)+1 FROM anonymous_identities WHERE topic_id=?`,
		topicID).Scan(&number); err != nil {
		return 0, 0, err
	}
	result, err := tx.ExecContext(ctx,
		`INSERT INTO anonymous_identities (topic_id,user_id,display_number) VALUES (?,?,?)`,
		topicID, userID, number)
	if err != nil {
		return 0, 0, err
	}
	newID, err := result.LastInsertId()
	if err != nil {
		return 0, 0, err
	}
	return uint64(newID), number, nil
}

// NewAnswer is validated input for answering a question.
type NewAnswer struct {
	AuthorID           uint64
	TopicID            uint64
	BodyMD             string
	RequestedAnonymous bool
	AttachmentIDs      []uint64
	MaxAttachments     int
}

// CreateAnswer writes an answer under the discussion's root lock.
func (s Store) CreateAnswer(ctx context.Context, in NewAnswer) (uint64, error) {
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
	if err := guardNewInteraction(root); err != nil {
		return 0, err
	}
	// Only a question has answers; a discussion board has no questions row.
	if !root.IsQuestion {
		return 0, ErrNotAQuestion
	}
	if err := refuseMuted(ctx, tx, in.AuthorID); err != nil {
		return 0, err
	}
	if err := requireAllAttachments(ctx, tx, in.AttachmentIDs, in.AuthorID); err != nil {
		return 0, err
	}
	anonymous, _ := resolveIdentity(root, in.AuthorID, in.RequestedAnonymous)

	result, err := tx.ExecContext(ctx,
		`INSERT INTO contents (author_id,kind,body_md,is_anonymous) VALUES (?,'answer',?,?)`,
		in.AuthorID, in.BodyMD, anonymous)
	if err != nil {
		return 0, err
	}
	lastID, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	contentID := uint64(lastID)

	// answers.question_id references questions(topic_id), so the question id is the topic id.
	if _, err := tx.ExecContext(ctx, `INSERT INTO answers (content_id,question_id) VALUES (?,?)`,
		contentID, in.TopicID); err != nil {
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
	// New activity moves the topic up the board's activity ordering.
	if _, err := tx.ExecContext(ctx, `UPDATE topics SET last_activity_at=UTC_TIMESTAMP(6) WHERE content_id=?`,
		in.TopicID); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return contentID, nil
}

// AnswerView is one answer assembled for a response.
type AnswerView struct {
	ContentID        uint64
	TopicID          uint64
	AuthorID         uint64
	TopicAuthorID    uint64
	TopicIsAnonymous bool
	BodyMD           string
	IsAnonymous      bool
	AnonymousNumber  int
	AuthorPublicID   string
	AuthorNickname   string
	AuthorAvatarKey  string
	AuthorCreatedAt  time.Time
	CreatedAt        time.Time
	EditedAt         sql.NullTime
	Attachments      []Attachment
}

// AnswerByContentID assembles one visible answer.
func (s Store) AnswerByContentID(ctx context.Context, contentID uint64) (AnswerView, error) {
	var view AnswerView
	var avatarKey sql.NullString
	var anonymousNumber sql.NullInt64
	err := s.DB.QueryRowContext(ctx, `
SELECT c.id,c.author_id,c.body_md,c.is_anonymous,c.created_at,c.edited_at,
 a.question_id,tc.author_id,tc.is_anonymous,
 u.public_id,u.nickname,u.created_at,av.storage_key,ai.display_number
FROM contents c
JOIN answers a ON a.content_id=c.id
JOIN contents tc ON tc.id=a.question_id
JOIN users u ON u.id=c.author_id
LEFT JOIN attachments av ON av.id=u.avatar_attachment_id AND av.status IN ('pending','active')
LEFT JOIN anonymous_identities ai ON ai.id=c.anonymous_identity_id
WHERE c.id=? AND c.deleted_at IS NULL AND c.moderation_status='visible'`, contentID).
		Scan(&view.ContentID, &view.AuthorID, &view.BodyMD, &view.IsAnonymous, &view.CreatedAt, &view.EditedAt,
			&view.TopicID, &view.TopicAuthorID, &view.TopicIsAnonymous,
			&view.AuthorPublicID, &view.AuthorNickname, &view.AuthorCreatedAt, &avatarKey, &anonymousNumber)
	if errors.Is(err, sql.ErrNoRows) {
		return view, ErrTopicNotFound
	}
	if err != nil {
		return view, err
	}
	view.AuthorAvatarKey = avatarKey.String
	view.AnonymousNumber = int(anonymousNumber.Int64)
	if view.Attachments, err = s.topicAttachments(ctx, contentID); err != nil {
		return view, err
	}
	return view, nil
}

// answerResponse mirrors the Answer schema.
type answerResponse struct {
	ID          string               `json:"id"`
	State       string               `json:"state"`
	Author      authorResponse       `json:"author"`
	BodyMD      string               `json:"body_md"`
	Attachments []attachmentResponse `json:"attachments"`
	CreatedAt   string               `json:"created_at"`
	EditedAt    string               `json:"edited_at,omitempty"`
	Kind        string               `json:"kind"`
	TopicID     string               `json:"topic_id"`
}

func toAnswerResponse(view AnswerView) answerResponse {
	response := answerResponse{
		ID:          strconv.FormatUint(view.ContentID, 10),
		State:       "published",
		Author:      renderAuthor(view.IsAnonymous, view.AuthorID == view.TopicAuthorID, view.AnonymousNumber, view.AuthorPublicID, view.AuthorNickname, view.AuthorAvatarKey, view.AuthorCreatedAt),
		BodyMD:      view.BodyMD,
		Attachments: make([]attachmentResponse, 0, len(view.Attachments)),
		CreatedAt:   formatTime(view.CreatedAt),
		Kind:        "answer",
		TopicID:     strconv.FormatUint(view.TopicID, 10),
	}
	if view.EditedAt.Valid {
		response.EditedAt = formatTime(view.EditedAt.Time)
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

// renderAuthor builds the ContentAuthor union. The anonymous branch never receives a user object: both
// schemas forbid additional properties, and leaking one would be a privacy defect. The discussion owner
// is always shown as 楼主 and other anonymous participants as 匿名#N.
func renderAuthor(anonymous, isTopicOwner bool, number int, publicID, nickname, avatarKey string, createdAt time.Time) authorResponse {
	if anonymous {
		display := "楼主"
		if !isTopicOwner {
			display = "匿名#" + strconv.Itoa(number)
		}
		return authorResponse{
			Mode:          "anonymous",
			DisplayName:   display,
			DisplayNumber: number,
			IsTopicOwner:  isTopicOwner,
		}
	}
	return authorResponse{
		Mode: "named",
		User: &publicUserResponse{
			PublicID:  publicID,
			Nickname:  nickname,
			AvatarURL: optionalMediaURL(avatarKey),
			CreatedAt: formatTime(createdAt),
		},
		IsTopicOwner: isTopicOwner,
	}
}

// createAnswerRequest mirrors CreateAnswerRequest.
type createAnswerRequest struct {
	TopicID       string   `json:"topic_id"`
	BodyMD        string   `json:"body_md"`
	AttachmentIDs []string `json:"attachment_ids"`
	IsAnonymous   bool     `json:"is_anonymous"`
}

// createAnswer publishes an answer to a question.
func (d Deps) createAnswer(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.FromContext(r.Context())
	if !ok {
		httpapi.Fail(w, r, 401, "UNAUTHENTICATED", "请先登录")
		return
	}
	var request createAnswerRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	topicID, err := ParseID(request.TopicID)
	if err != nil {
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "问题ID无效")
		return
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
	answerID, err := d.Store.CreateAnswer(r.Context(), NewAnswer{
		AuthorID:           principal.UserID,
		TopicID:            topicID,
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
	view, err := d.Store.AnswerByContentID(r.Context(), answerID)
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	httpapi.JSON(w, http.StatusCreated, toAnswerResponse(view))
}
