package content

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"opentalk/server/internal/platform/httpapi"
)

// This file is the read side of the content loop: the topic list, one content row by id (including the
// tombstone a deleted row leaves behind), the permission summary an interface needs before it draws an
// action, and the answer, comment and tag lists.
//
// Two rules shape all of it.
//
// Visibility is never re-implemented here. A topic is its own ancestor, so constraining its row is the
// whole rule; for answers and comments the SQL only *narrows the candidate set* and the page is then put
// through visibleContentIDs, which decides with the same walk every other reader uses. Narrowing in SQL
// and deciding with the shared rule is what keeps a list from sampling one ancestor and leaking a hidden
// branch — the failure mode this package has already been bitten by twice.
//
// A page is a keyset, never an offset: the cursor carries the last row's ordering key, so content created
// or removed between two pages cannot duplicate or skip a row.

// scanner is the read surface shared by *sql.Row and *sql.Rows, so one scan function can serve both the
// single-row reader and the list. Without it the two would carry two copies of the same column list.
type scanner interface{ Scan(dest ...any) error }

// createdOrderExpression and activeOrderExpression are the two timestamps a topic list can be ordered by,
// projected to one integer each.
//
// They are projections rather than raw comparisons because the shared cursor format carries integers.
// DATE_FORMAT keeps the ordering in the calendar the column is stored in: converting to an epoch would
// make the sort depend on the connection's time zone, while the rows are written with UTC_TIMESTAMP(6)
// whatever that time zone is. Seconds are enough, because the content id breaks ties inside a second.
//
// The same constants build the selected column, the ORDER BY and the keyset comparison, so the key a page
// is resumed from is provably the key the rows were sorted by.
const (
	createdOrderExpression = `CAST(DATE_FORMAT(c.created_at,'%Y%m%d%H%i%s') AS UNSIGNED)`
	activeOrderExpression  = `CAST(DATE_FORMAT(t.last_activity_at,'%Y%m%d%H%i%s') AS UNSIGNED)`
)

// The select/from pairs below are each the single source of truth for one view's columns and joins. The
// single-row readers and the list readers share them, so a column added for one cannot be silently missing
// in the other.
const topicViewQuery = `
SELECT c.id,c.author_id,c.body_md,c.is_anonymous,c.created_at,c.edited_at,
 t.board_id,b.kind,t.title,t.is_locked,t.is_pinned,t.last_activity_at,
 u.public_id,u.nickname,u.created_at,av.storage_key,ai.display_number,
 q.resolution_status,q.resolved_at,` + createdOrderExpression + `,` + activeOrderExpression + `
FROM contents c
JOIN topics t ON t.content_id=c.id
JOIN boards b ON b.id=t.board_id
JOIN users u ON u.id=c.author_id
LEFT JOIN attachments av ON av.id=u.avatar_attachment_id AND av.status IN ('pending','active')
LEFT JOIN anonymous_identities ai ON ai.id=c.anonymous_identity_id
LEFT JOIN questions q ON q.topic_id=c.id`

const answerViewFrom = `
FROM contents c
JOIN answers a ON a.content_id=c.id
JOIN contents tc ON tc.id=a.question_id
JOIN users u ON u.id=c.author_id
LEFT JOIN attachments av ON av.id=u.avatar_attachment_id AND av.status IN ('pending','active')
LEFT JOIN anonymous_identities ai ON ai.id=c.anonymous_identity_id`

const answerViewQuery = `
SELECT c.id,c.author_id,c.body_md,c.is_anonymous,c.created_at,c.edited_at,
 a.question_id,tc.author_id,tc.is_anonymous,
 u.public_id,u.nickname,u.created_at,av.storage_key,ai.display_number` + answerViewFrom

const commentViewFrom = `
FROM contents c
JOIN comments cm ON cm.content_id=c.id
JOIN contents tc ON tc.id=cm.topic_id
JOIN users u ON u.id=c.author_id
LEFT JOIN attachments av ON av.id=u.avatar_attachment_id AND av.status IN ('pending','active')
LEFT JOIN anonymous_identities ai ON ai.id=c.anonymous_identity_id`

const commentViewQuery = `
SELECT c.id,c.author_id,c.body_md,c.is_anonymous,c.created_at,c.edited_at,
 cm.topic_id,cm.answer_id,cm.parent_comment_id,cm.reply_to_comment_id,
 tc.author_id,
 u.public_id,u.nickname,u.created_at,av.storage_key,ai.display_number` + commentViewFrom

// Cursor scopes bind a cursor to the list it was issued for. The topic scope also carries the filter,
// because "newest in board 3" and "newest everywhere" are different orderings of different sets.
const (
	sortNewest     = "newest"
	sortActive     = "active"
	tagCursorScope = "tags"
)

// scanTopicView reads one topic row. It is used by the single-topic reader and by the list, so both
// assemble a topic identically.
func scanTopicView(row scanner) (TopicView, error) {
	var view TopicView
	var avatarKey sql.NullString
	var anonymousNumber sql.NullInt64
	err := row.Scan(&view.ContentID, &view.AuthorID, &view.BodyMD, &view.IsAnonymous, &view.CreatedAt, &view.EditedAt,
		&view.BoardID, &view.BoardKind, &view.Title, &view.IsLocked, &view.IsPinned, &view.LastActivityAt,
		&view.AuthorPublicID, &view.AuthorNickname, &view.AuthorCreatedAt, &avatarKey, &anonymousNumber,
		&view.ResolutionState, &view.ResolvedAt, &view.createdKey, &view.activityKey)
	if err != nil {
		return view, err
	}
	view.AuthorAvatarKey = avatarKey.String
	view.AnonymousNumber = int(anonymousNumber.Int64)
	return view, nil
}

func scanAnswerView(row scanner) (AnswerView, error) {
	var view AnswerView
	var avatarKey sql.NullString
	var anonymousNumber sql.NullInt64
	err := row.Scan(&view.ContentID, &view.AuthorID, &view.BodyMD, &view.IsAnonymous, &view.CreatedAt, &view.EditedAt,
		&view.TopicID, &view.TopicAuthorID, &view.TopicIsAnonymous,
		&view.AuthorPublicID, &view.AuthorNickname, &view.AuthorCreatedAt, &avatarKey, &anonymousNumber)
	if err != nil {
		return view, err
	}
	view.AuthorAvatarKey = avatarKey.String
	view.AnonymousNumber = int(anonymousNumber.Int64)
	return view, nil
}

func scanCommentView(row scanner) (CommentView, error) {
	var view CommentView
	var avatarKey sql.NullString
	var anonymousNumber sql.NullInt64
	err := row.Scan(&view.ContentID, &view.AuthorID, &view.BodyMD, &view.IsAnonymous, &view.CreatedAt, &view.EditedAt,
		&view.TopicID, &view.AnswerID, &view.ParentID, &view.ReplyToID,
		&view.TopicAuthorID,
		&view.AuthorPublicID, &view.AuthorNickname, &view.AuthorCreatedAt, &avatarKey, &anonymousNumber)
	if err != nil {
		return view, err
	}
	view.AuthorAvatarKey = avatarKey.String
	view.AnonymousNumber = int(anonymousNumber.Int64)
	return view, nil
}

// topicFilter is the validated topic-list filter. A zero BoardID or TagID means "not filtered", which is
// unambiguous because ids start at 1.
type topicFilter struct {
	BoardID          uint64
	TagID            uint64
	ResolutionStatus string
	Sort             string
}

// cursorScope binds the cursor to the ordering and the filter that produced it.
func (f topicFilter) cursorScope() string {
	scope := "topics:" + f.Sort
	if f.BoardID != 0 {
		scope += ":board=" + strconv.FormatUint(f.BoardID, 10)
	}
	if f.TagID != 0 {
		scope += ":tag=" + strconv.FormatUint(f.TagID, 10)
	}
	if f.ResolutionStatus != "" {
		scope += ":status=" + f.ResolutionStatus
	}
	return scope
}

// orderKeyExpression is the timestamp the list is ordered by; see the two expressions above.
func (f topicFilter) orderKeyExpression() string {
	if f.Sort == sortActive {
		return activeOrderExpression
	}
	return createdOrderExpression
}

// cursorKey is the integer the returned page's last row is resumed from: the same value the query sorted
// by, read back from the row rather than recomputed in Go, so the two cannot drift.
func (f topicFilter) cursorKey(topic TopicView) int64 {
	if f.Sort == sortActive {
		return topic.activityKey
	}
	return topic.createdKey
}

// ListTopics returns one page of visible topics ordered by the filter's sort. Pinned topics come first in
// both sorts, which is what pinning means; that makes the ordering key three columns, and all three are
// in the cursor.
func (s Store) ListTopics(ctx context.Context, filter topicFilter, page pageRequest) ([]TopicView, bool, error) {
	orderKey := filter.orderKeyExpression()
	conditions := []string{`c.deleted_at IS NULL`, `c.moderation_status='visible'`}
	args := make([]any, 0, 8)
	if filter.BoardID != 0 {
		conditions = append(conditions, `t.board_id=?`)
		args = append(args, filter.BoardID)
	}
	if filter.TagID != 0 {
		conditions = append(conditions, `EXISTS(SELECT 1 FROM topic_tags tt WHERE tt.topic_id=c.id AND tt.tag_id=?)`)
		args = append(args, filter.TagID)
	}
	if filter.ResolutionStatus != "" {
		// A discussion has no questions row, so it can never satisfy this filter — which is the intended
		// reading of asking a mixed list for solved questions only.
		conditions = append(conditions, `q.resolution_status=?`)
		args = append(args, filter.ResolutionStatus)
	}
	if len(page.After) == 3 {
		conditions = append(conditions, `(t.is_pinned,`+orderKey+`,c.id) < (?,?,?)`)
		args = append(args, page.After[0], page.After[1], page.After[2])
	}
	query := topicViewQuery + ` WHERE ` + strings.Join(conditions, ` AND `) +
		` ORDER BY t.is_pinned DESC,` + orderKey + ` DESC,c.id DESC LIMIT ?`
	args = append(args, page.Limit+1)

	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, err
	}
	topics, err := scanRows(rows, func(rows *sql.Rows) (TopicView, error) { return scanTopicView(rows) })
	if err != nil {
		return nil, false, err
	}
	hasMore := len(topics) > page.Limit
	if hasMore {
		topics = topics[:page.Limit]
	}
	if err := s.attachTopicDetails(ctx, topics); err != nil {
		return nil, false, err
	}
	return topics, hasMore, nil
}

// ListAnswers returns one page of a question's answers in publication order (oldest first), which is how a
// discussion reads.
//
// The answers are narrowed in SQL and then filtered through the shared rule. The caller has already
// established that the question itself is reachable, so the filter only ever removes a row whose own
// chain is broken — a row written around the application, which the rule must still refuse to serve.
func (s Store) ListAnswers(ctx context.Context, questionID uint64, page pageRequest) ([]AnswerView, bool, uint64, error) {
	conditions := []string{`a.question_id=?`, `c.deleted_at IS NULL`, `c.moderation_status='visible'`}
	args := []any{questionID}
	if len(page.After) == 1 {
		conditions = append(conditions, `c.id>?`)
		args = append(args, page.After[0])
	}
	query := answerViewQuery + ` WHERE ` + strings.Join(conditions, ` AND `) + ` ORDER BY c.id LIMIT ?`
	args = append(args, page.Limit+1)

	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, 0, err
	}
	answers, err := scanRows(rows, func(rows *sql.Rows) (AnswerView, error) { return scanAnswerView(rows) })
	if err != nil {
		return nil, false, 0, err
	}
	hasMore := len(answers) > page.Limit
	if hasMore {
		answers = answers[:page.Limit]
	}
	// The cursor follows the last row *examined*, not the last row returned: a page whose rows were all
	// filtered out still has to hand the client a way forward, or it would ask for the same page forever.
	lastExamined := uint64(0)
	if len(answers) > 0 {
		lastExamined = answers[len(answers)-1].ContentID
	}
	answers, err = s.reachableAnswers(ctx, answers)
	if err != nil {
		return nil, false, 0, err
	}
	if err := s.attachAnswerDetails(ctx, answers); err != nil {
		return nil, false, 0, err
	}
	return answers, hasMore, lastExamined, nil
}

// reachableAnswers drops the rows the shared visibility rule refuses.
func (s Store) reachableAnswers(ctx context.Context, answers []AnswerView) ([]AnswerView, error) {
	if len(answers) == 0 {
		return answers, nil
	}
	ids := make([]uint64, 0, len(answers))
	for _, answer := range answers {
		ids = append(ids, answer.ContentID)
	}
	visible, err := visibleContentIDs(ctx, s.DB, ids)
	if err != nil {
		return nil, err
	}
	reachable := make([]AnswerView, 0, len(answers))
	for _, answer := range answers {
		if visible[answer.ContentID] {
			reachable = append(reachable, answer)
		}
	}
	return reachable, nil
}

// commentFilter is the validated comment-list filter. A zero AnswerID or ParentCommentID means "the
// top-level scope of the level above", which is what makes the two-level display pageable: the client
// pages top-level comments, and asks for one thread's replies when it expands it.
type commentFilter struct {
	TopicID         uint64
	AnswerID        uint64
	ParentCommentID uint64
}

// cursorScope binds the cursor to the exact list that produced it.
func (f commentFilter) cursorScope() string {
	scope := "comments:topic=" + strconv.FormatUint(f.TopicID, 10)
	if f.AnswerID != 0 {
		scope += ":answer=" + strconv.FormatUint(f.AnswerID, 10)
	} else {
		scope += ":answer=topic"
	}
	if f.ParentCommentID != 0 {
		scope += ":parent=" + strconv.FormatUint(f.ParentCommentID, 10)
	} else {
		scope += ":parent=root"
	}
	return scope
}

// ListComments returns one page of comments in publication order (oldest first).
func (s Store) ListComments(ctx context.Context, filter commentFilter, page pageRequest) ([]CommentView, bool, uint64, error) {
	conditions := []string{`cm.topic_id=?`, `c.deleted_at IS NULL`, `c.moderation_status='visible'`}
	args := []any{filter.TopicID}
	if filter.AnswerID != 0 {
		conditions = append(conditions, `cm.answer_id=?`)
		args = append(args, filter.AnswerID)
	} else {
		conditions = append(conditions, `cm.answer_id IS NULL`)
	}
	if filter.ParentCommentID != 0 {
		conditions = append(conditions, `cm.parent_comment_id=?`)
		args = append(args, filter.ParentCommentID)
	} else {
		conditions = append(conditions, `cm.parent_comment_id IS NULL`)
	}
	if len(page.After) == 1 {
		conditions = append(conditions, `c.id>?`)
		args = append(args, page.After[0])
	}
	query := commentViewQuery + ` WHERE ` + strings.Join(conditions, ` AND `) + ` ORDER BY c.id LIMIT ?`
	args = append(args, page.Limit+1)

	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, 0, err
	}
	comments, err := scanRows(rows, func(rows *sql.Rows) (CommentView, error) { return scanCommentView(rows) })
	if err != nil {
		return nil, false, 0, err
	}
	hasMore := len(comments) > page.Limit
	if hasMore {
		comments = comments[:page.Limit]
	}
	lastExamined := uint64(0)
	if len(comments) > 0 {
		lastExamined = comments[len(comments)-1].ContentID
	}
	comments, err = s.reachableComments(ctx, comments)
	if err != nil {
		return nil, false, 0, err
	}
	if err := s.attachCommentDetails(ctx, comments); err != nil {
		return nil, false, 0, err
	}
	return comments, hasMore, lastExamined, nil
}

func (s Store) reachableComments(ctx context.Context, comments []CommentView) ([]CommentView, error) {
	if len(comments) == 0 {
		return comments, nil
	}
	ids := make([]uint64, 0, len(comments))
	for _, comment := range comments {
		ids = append(ids, comment.ContentID)
	}
	visible, err := visibleContentIDs(ctx, s.DB, ids)
	if err != nil {
		return nil, err
	}
	reachable := make([]CommentView, 0, len(comments))
	for _, comment := range comments {
		if visible[comment.ContentID] {
			reachable = append(reachable, comment)
		}
	}
	return reachable, nil
}

// ListTags returns one page of tags ordered by name, filtered by a name substring when q is set.
//
// The keyset compares against the row the cursor names rather than carrying the name itself, because the
// shared cursor format holds integers: `(name,id) > (SELECT name,id FROM tags WHERE id=?)` resumes exactly
// after that row. A cursor whose row is gone matches nothing, which is the safe direction.
func (s Store) ListTags(ctx context.Context, search string, page pageRequest) ([]Tag, bool, error) {
	conditions := make([]string, 0, 2)
	args := make([]any, 0, 3)
	if search != "" {
		conditions = append(conditions, `g.name LIKE CONCAT('%',?,'%') ESCAPE '\\'`)
		args = append(args, escapeLike(search))
	}
	if len(page.After) == 1 {
		conditions = append(conditions, `(g.name,g.id) > (SELECT g2.name,g2.id FROM tags g2 WHERE g2.id=?)`)
		args = append(args, page.After[0])
	}
	query := `SELECT g.id,g.name,g.description FROM tags g`
	if len(conditions) > 0 {
		query += ` WHERE ` + strings.Join(conditions, ` AND `)
	}
	query += ` ORDER BY g.name,g.id LIMIT ?`
	args = append(args, page.Limit+1)

	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, err
	}
	tags, err := scanRows(rows, func(rows *sql.Rows) (Tag, error) {
		var tag Tag
		return tag, rows.Scan(&tag.ID, &tag.Name, &tag.Description)
	})
	if err != nil {
		return nil, false, err
	}
	hasMore := len(tags) > page.Limit
	if hasMore {
		tags = tags[:page.Limit]
	}
	return tags, hasMore, nil
}

// escapeLike neutralises the LIKE wildcards in user input, so searching for "%" looks for a percent sign
// instead of matching every tag.
func escapeLike(value string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(value)
}

// BoardExists and TagExists let a filtered list answer "that board does not exist" instead of returning an
// empty page, which would read as "this board has no topics".
func (s Store) BoardExists(ctx context.Context, boardID uint64) (bool, error) {
	return existsByID(ctx, s.DB, `SELECT EXISTS(SELECT 1 FROM boards WHERE id=?)`, boardID)
}

func (s Store) TagExists(ctx context.Context, tagID uint64) (bool, error) {
	return existsByID(ctx, s.DB, `SELECT EXISTS(SELECT 1 FROM tags WHERE id=?)`, tagID)
}

func existsByID(ctx context.Context, q queryer, query string, id uint64) (bool, error) {
	var exists bool
	if err := q.QueryRowContext(ctx, query, id).Scan(&exists); err != nil {
		return false, err
	}
	return exists, nil
}

// attachTopicDetails fills tags and attachments for a whole page in two queries instead of two per row.
func (s Store) attachTopicDetails(ctx context.Context, topics []TopicView) error {
	if len(topics) == 0 {
		return nil
	}
	ids := make([]uint64, 0, len(topics))
	for _, topic := range topics {
		ids = append(ids, topic.ContentID)
	}
	tags, err := s.tagsForContents(ctx, ids)
	if err != nil {
		return err
	}
	attachments, err := s.attachmentsForContents(ctx, ids)
	if err != nil {
		return err
	}
	for index := range topics {
		topics[index].Tags = tags[topics[index].ContentID]
		topics[index].Attachments = attachments[topics[index].ContentID]
	}
	return nil
}

func (s Store) attachAnswerDetails(ctx context.Context, answers []AnswerView) error {
	if len(answers) == 0 {
		return nil
	}
	ids := make([]uint64, 0, len(answers))
	for _, answer := range answers {
		ids = append(ids, answer.ContentID)
	}
	attachments, err := s.attachmentsForContents(ctx, ids)
	if err != nil {
		return err
	}
	for index := range answers {
		answers[index].Attachments = attachments[answers[index].ContentID]
	}
	return nil
}

func (s Store) attachCommentDetails(ctx context.Context, comments []CommentView) error {
	if len(comments) == 0 {
		return nil
	}
	ids := make([]uint64, 0, len(comments))
	for _, comment := range comments {
		ids = append(ids, comment.ContentID)
	}
	attachments, err := s.attachmentsForContents(ctx, ids)
	if err != nil {
		return err
	}
	for index := range comments {
		comments[index].Attachments = attachments[comments[index].ContentID]
	}
	return nil
}

// tagsForContents and attachmentsForContents are the batched form of topicTags and topicAttachments.
func (s Store) tagsForContents(ctx context.Context, ids []uint64) (map[uint64][]Tag, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT tt.topic_id,g.id,g.name,g.description FROM topic_tags tt
 JOIN tags g ON g.id=tt.tag_id WHERE tt.topic_id IN (`+placeholders(len(ids))+`) ORDER BY tt.topic_id,g.id`,
		toAny(ids)...)
	if err != nil {
		return nil, err
	}
	return groupByContent(rows, func(rows *sql.Rows) (uint64, Tag, error) {
		var contentID uint64
		var tag Tag
		err := rows.Scan(&contentID, &tag.ID, &tag.Name, &tag.Description)
		return contentID, tag, err
	})
}

func (s Store) attachmentsForContents(ctx context.Context, ids []uint64) (map[uint64][]Attachment, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT ca.content_id,a.id,a.storage_key,a.mime_type,a.byte_size
 FROM content_attachments ca JOIN attachments a ON a.id=ca.attachment_id
 WHERE ca.content_id IN (`+placeholders(len(ids))+`) AND a.status IN ('pending','active')
 ORDER BY ca.content_id,a.id`, toAny(ids)...)
	if err != nil {
		return nil, err
	}
	return groupByContent(rows, func(rows *sql.Rows) (uint64, Attachment, error) {
		var contentID uint64
		var attachment Attachment
		err := rows.Scan(&contentID, &attachment.ID, &attachment.Key, &attachment.MIMEType, &attachment.ByteSize)
		return contentID, attachment, err
	})
}

// groupByContent turns a (content_id, value) row set into one slice per content. A content with no rows is
// absent from the map, and every caller reads it through a zero-value lookup, which is an empty slice and
// marshals as [] rather than null.
func groupByContent[T any](rows *sql.Rows, scan func(*sql.Rows) (uint64, T, error)) (map[uint64][]T, error) {
	grouped := make(map[uint64][]T)
	_, err := scanRows(rows, func(rows *sql.Rows) (struct{}, error) {
		contentID, value, err := scan(rows)
		if err != nil {
			return struct{}{}, err
		}
		grouped[contentID] = append(grouped[contentID], value)
		return struct{}{}, nil
	})
	if err != nil {
		return nil, err
	}
	return grouped, nil
}

// deletedContentView is the tombstone a deleted row leaves: enough to place the gap in a thread and say
// what used to be there, and nothing else. The archived original is reachable only through the moderation
// route, which audits the access.
type deletedContentView struct {
	ContentID uint64
	Kind      string
	TopicID   sql.NullInt64
	AnswerID  sql.NullInt64
	ParentID  sql.NullInt64
	ReplyToID sql.NullInt64
	DeletedAt time.Time
}

// DeletedContentByID reads the tombstone of a deleted row. It does not check visibility: the caller has to
// decide separately whether a reader may learn that this row existed at all, which is what
// ancestorsReachable answers.
func (s Store) DeletedContentByID(ctx context.Context, contentID uint64) (deletedContentView, error) {
	var view deletedContentView
	var kind string
	var boardKind sql.NullString
	// answers.question_id and comments.topic_id are mutually exclusive — a row is one kind or another — so
	// they are read into separate targets and then merged into the one topic_id the response carries.
	var answerTopicID sql.NullInt64
	var commentTopicID sql.NullInt64
	err := s.DB.QueryRowContext(ctx, `
SELECT c.id,c.kind,c.deleted_at,b.kind,a.question_id,cm.topic_id,cm.answer_id,cm.parent_comment_id,cm.reply_to_comment_id
FROM contents c
LEFT JOIN topics t ON t.content_id=c.id
LEFT JOIN boards b ON b.id=t.board_id
LEFT JOIN answers a ON a.content_id=c.id
LEFT JOIN comments cm ON cm.content_id=c.id
WHERE c.id=? AND c.deleted_at IS NOT NULL`, contentID).
		Scan(&view.ContentID, &kind, &view.DeletedAt, &boardKind, &answerTopicID, &commentTopicID,
			&view.AnswerID, &view.ParentID, &view.ReplyToID)
	if errors.Is(err, sql.ErrNoRows) {
		return view, ErrContentNotFound
	}
	if err != nil {
		return view, err
	}
	if !answerTopicID.Valid {
		view.TopicID = commentTopicID
	}
	switch kind {
	case "topic":
		// The board's kind is what distinguishes a deleted post from a deleted question; the contents row
		// only knows "topic".
		if boardKind.String == "qa" {
			view.Kind = "question"
		} else {
			view.Kind = "discussion"
		}
	case "answer":
		view.Kind = "answer"
	case "comment":
		view.Kind = "comment"
	default:
		return view, ErrContentNotFound
	}
	return view, nil
}

// deletedMessage is the contract's fixed display text per original kind.
func (v deletedContentView) message() string {
	switch v.Kind {
	case "question":
		return "该问题已被删除"
	case "answer":
		return "该回答已被删除"
	case "comment":
		return "该评论已被删除"
	default:
		return "该帖子已被删除"
	}
}

// ContentPermissions mirrors the ContentPermissions schema: what this viewer may do with this content.
// It is an interface convenience, never an authorization decision — every write re-checks on the server.
type ContentPermissions struct {
	CanEdit               bool `json:"can_edit"`
	CanDelete             bool `json:"can_delete"`
	CanCreateAnswer       bool `json:"can_create_answer"`
	CanCreateComment      bool `json:"can_create_comment"`
	CanSetResolution      bool `json:"can_set_resolution"`
	ForceAnonymousComment bool `json:"force_anonymous_comment"`
}

// PermissionsFor answers the six flags for one viewer.
//
// It mirrors the write paths rather than inventing its own rules, including the places where they are
// deliberately stricter than a role check: deleting is the author's alone (there is no administrator
// shortcut), the edit window is measured from publication and does not slide, muting removes speech but
// not the right to delete what you posted, and switching the resolution belongs to the asker — an
// administrator is not an exception, because whether a question is answered is the asker's judgement.
//
// A banned account cannot reach this code with a session at all: the session lookup refuses banned users,
// so a banned viewer is an anonymous one here.
func (s Store) PermissionsFor(ctx context.Context, contentID, viewerID uint64) (ContentPermissions, error) {
	var permissions ContentPermissions
	// A hidden row, or one whose chain is broken, has no permissions to report: it is not there for this
	// reader, so it is not there to be acted on either.
	if err := requireVisible(ctx, s.DB, contentID); err != nil {
		return permissions, err
	}
	rootID, err := s.rootTopicOf(ctx, contentID)
	if err != nil {
		return permissions, err
	}
	root, err := loadRootTopic(ctx, s.DB, rootID)
	if err != nil {
		return permissions, err
	}
	var authorID uint64
	var createdAt time.Time
	if err := s.DB.QueryRowContext(ctx, `SELECT author_id,created_at FROM contents WHERE id=?`, contentID).
		Scan(&authorID, &createdAt); err != nil {
		return permissions, err
	}
	// A guest may read the flags but never act on them; all six stay false.
	if viewerID == 0 {
		return permissions, nil
	}
	muted := false
	if err := refuseMuted(ctx, s.DB, viewerID); err != nil {
		if !errors.Is(err, ErrAccountMuted) {
			return permissions, err
		}
		muted = true
	}
	isAuthor := viewerID == authorID
	permissions.CanDelete = isAuthor
	permissions.CanEdit = isAuthor && !muted && !root.IsLocked && time.Since(createdAt) <= EditWindow
	permissions.CanCreateAnswer = !muted && !root.IsLocked && root.IsQuestion && root.Resolution == "open"
	permissions.CanCreateComment = !muted && !root.IsLocked && root.Resolution != "solved"
	permissions.CanSetResolution = root.IsQuestion && viewerID == root.AuthorID
	permissions.ForceAnonymousComment = root.IsAnonymous && viewerID == root.AuthorID
	return permissions, nil
}

// contentPageView is the shared page envelope for the mixed-kind lists.
type contentPageView struct {
	Items      []any  `json:"items"`
	HasMore    bool   `json:"has_more"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// deletedContentResponse mirrors the DeletedContent schema.
type deletedContentResponse struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	State     string `json:"state"`
	Message   string `json:"message"`
	DeletedAt string `json:"deleted_at"`
	TopicID   string `json:"topic_id,omitempty"`
	AnswerID  string `json:"answer_id,omitempty"`
	ParentID  string `json:"parent_comment_id,omitempty"`
	ReplyToID string `json:"reply_to_comment_id,omitempty"`
}

func toDeletedContentResponse(view deletedContentView) deletedContentResponse {
	response := deletedContentResponse{
		ID:        strconv.FormatUint(view.ContentID, 10),
		Kind:      view.Kind,
		State:     "deleted",
		Message:   view.message(),
		DeletedAt: formatTime(view.DeletedAt),
	}
	if view.TopicID.Valid {
		response.TopicID = strconv.FormatInt(view.TopicID.Int64, 10)
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
	return response
}

// tagPageView mirrors TagPage.
type tagPageView struct {
	Items      []tagResponse `json:"items"`
	HasMore    bool          `json:"has_more"`
	NextCursor string        `json:"next_cursor,omitempty"`
}

// parsePageOrFail turns the shared pagination errors into the contract's responses and reports whether it
// already answered the request.
func (d Deps) parsePageOrFail(w http.ResponseWriter, r *http.Request, scope string) (pageRequest, bool) {
	page, err := parsePage(r, scope)
	switch {
	case errors.Is(err, ErrInvalidCursor):
		httpapi.Fail(w, r, 400, "CURSOR_INVALID", "游标无效，请重新加载列表")
		return page, false
	case errors.Is(err, ErrInvalidLimit):
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "分页数量必须在 1 到 100 之间")
		return page, false
	case err != nil:
		d.failDependency(w, r, err)
		return page, false
	}
	return page, true
}

// optionalID reads a filter that may be absent. An empty value means "not filtered"; anything else must
// be a positive decimal id.
func optionalID(r *http.Request, name string) (uint64, bool, bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return 0, false, true
	}
	value, err := ParseID(raw)
	if err != nil {
		return 0, false, false
	}
	return value, true, true
}

// pathID reads a required path parameter.
func pathID(w http.ResponseWriter, r *http.Request, name string) (uint64, bool) {
	value, err := ParseID(r.PathValue(name))
	if err != nil {
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "ID无效")
		return 0, false
	}
	return value, true
}

// failRead maps a read failure onto the contract's codes.
func (d Deps) failRead(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrContentNotFound), errors.Is(err, ErrTopicNotFound):
		httpapi.Fail(w, r, 404, "CONTENT_NOT_FOUND", "内容不存在或不可见")
	case errors.Is(err, ErrNotAQuestion):
		httpapi.Fail(w, r, 409, "NOT_A_QUESTION", "该主题是讨论帖，不能回答")
	case errors.Is(err, ErrCommentTarget), errors.Is(err, ErrReplyTarget):
		httpapi.Fail(w, r, 422, "INVALID_COMMENT_TARGET", "评论或回复目标无效")
	case errors.Is(err, ErrBoardNotFound):
		httpapi.Fail(w, r, 404, "BOARD_NOT_FOUND", "板块不存在")
	case errors.Is(err, ErrTagNotFound):
		httpapi.Fail(w, r, 404, "TAG_NOT_FOUND", "所选标签不存在")
	default:
		d.failDependency(w, r, err)
	}
}

// listTopics serves the public topic list. Guests may browse it, so no session is resolved.
func (d Deps) listTopics(w http.ResponseWriter, r *http.Request) {
	filter := topicFilter{Sort: sortNewest}
	switch sort := r.URL.Query().Get("sort"); sort {
	case "", sortNewest:
	case sortActive:
		filter.Sort = sortActive
	default:
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "排序方式只能是 newest 或 active")
		return
	}
	boardID, _, ok := optionalID(r, "board_id")
	if !ok {
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "板块ID无效")
		return
	}
	tagID, _, ok := optionalID(r, "tag_id")
	if !ok {
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "标签ID无效")
		return
	}
	filter.BoardID, filter.TagID = boardID, tagID
	switch status := r.URL.Query().Get("resolution_status"); status {
	case "":
	case "open", "solved":
		filter.ResolutionStatus = status
	default:
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "解决状态只能是 open 或 solved")
		return
	}
	page, ok := d.parsePageOrFail(w, r, filter.cursorScope())
	if !ok {
		return
	}
	if filter.BoardID != 0 {
		exists, err := d.Store.BoardExists(r.Context(), filter.BoardID)
		if err != nil {
			d.failDependency(w, r, err)
			return
		}
		if !exists {
			httpapi.Fail(w, r, 404, "BOARD_NOT_FOUND", "板块不存在")
			return
		}
	}
	if filter.TagID != 0 {
		exists, err := d.Store.TagExists(r.Context(), filter.TagID)
		if err != nil {
			d.failDependency(w, r, err)
			return
		}
		if !exists {
			httpapi.Fail(w, r, 404, "TAG_NOT_FOUND", "所选标签不存在")
			return
		}
	}
	topics, hasMore, err := d.Store.ListTopics(r.Context(), filter, page)
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	view := contentPageView{Items: make([]any, 0, len(topics)), HasMore: hasMore}
	for _, topic := range topics {
		view.Items = append(view.Items, toTopicResponse(topic))
	}
	if hasMore && len(topics) > 0 {
		last := topics[len(topics)-1]
		pinned := int64(0)
		if last.IsPinned {
			pinned = 1
		}
		view.NextCursor = encodeCursor(filter.cursorScope(), pinned, filter.cursorKey(last), int64(last.ContentID))
	}
	httpapi.JSON(w, http.StatusOK, view)
}

// getContent serves one content row. A deleted row answers with the tombstone the contract defines — but
// only when the rest of its chain is intact, so a deleted comment inside a hidden answer still reports the
// same not-found as an id that never existed.
func (d Deps) getContent(w http.ResponseWriter, r *http.Request) {
	contentID, ok := pathID(w, r, "content_id")
	if !ok {
		return
	}
	var deletedAt sql.NullTime
	err := d.Store.DB.QueryRowContext(r.Context(), `SELECT deleted_at FROM contents WHERE id=?`, contentID).Scan(&deletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		httpapi.Fail(w, r, 404, "CONTENT_NOT_FOUND", "内容不存在或不可见")
		return
	}
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	if deletedAt.Valid {
		reachable, err := ancestorsReachable(r.Context(), d.Store.DB, contentID)
		if err != nil {
			d.failDependency(w, r, err)
			return
		}
		if !reachable {
			httpapi.Fail(w, r, 404, "CONTENT_NOT_FOUND", "内容不存在或不可见")
			return
		}
		tombstone, err := d.Store.DeletedContentByID(r.Context(), contentID)
		if err != nil {
			d.failRead(w, r, err)
			return
		}
		httpapi.JSON(w, http.StatusOK, toDeletedContentResponse(tombstone))
		return
	}
	response, err := d.Store.PublicContentByID(r.Context(), contentID)
	if err != nil {
		d.failRead(w, r, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, response)
}

// getContentPermissions serves the current viewer's flags for one content row. It resolves the session
// optionally, because a guest legitimately asks this question and the answer is "nothing".
func (d Deps) getContentPermissions(w http.ResponseWriter, r *http.Request) {
	contentID, ok := pathID(w, r, "content_id")
	if !ok {
		return
	}
	permissions, err := d.Store.PermissionsFor(r.Context(), contentID, d.viewerID(r))
	if err != nil {
		d.failRead(w, r, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, permissions)
}

// listAnswers serves one page of a question's answers.
func (d Deps) listAnswers(w http.ResponseWriter, r *http.Request) {
	topicID, ok := pathID(w, r, "topic_id")
	if !ok {
		return
	}
	// The question is the anchor for the page's visibility, so it is checked with the shared rule first.
	root, err := loadRootTopic(r.Context(), d.Store.DB, topicID)
	if err != nil {
		d.failRead(w, r, err)
		return
	}
	if root.Deleted || root.Hidden {
		httpapi.Fail(w, r, 404, "CONTENT_NOT_FOUND", "内容不存在或不可见")
		return
	}
	if !root.IsQuestion {
		httpapi.Fail(w, r, 409, "NOT_A_QUESTION", "该主题是讨论帖，不能回答")
		return
	}
	page, ok := d.parsePageOrFail(w, r, "answers:"+strconv.FormatUint(topicID, 10))
	if !ok {
		return
	}
	answers, hasMore, lastExamined, err := d.Store.ListAnswers(r.Context(), topicID, page)
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	view := contentPageView{Items: make([]any, 0, len(answers)), HasMore: hasMore}
	for _, answer := range answers {
		view.Items = append(view.Items, toAnswerResponse(answer))
	}
	if hasMore && lastExamined != 0 {
		view.NextCursor = encodeCursor("answers:"+strconv.FormatUint(topicID, 10), int64(lastExamined))
	}
	httpapi.JSON(w, http.StatusOK, view)
}

// listComments serves one page of comments: the top-level rows of the requested scope, or the replies under
// one thread root when parent_comment_id is given. Paging only the level the client is showing is what
// keeps a two-level display consistent page by page.
func (d Deps) listComments(w http.ResponseWriter, r *http.Request) {
	topicID, ok := pathID(w, r, "topic_id")
	if !ok {
		return
	}
	root, err := loadRootTopic(r.Context(), d.Store.DB, topicID)
	if err != nil {
		d.failRead(w, r, err)
		return
	}
	if root.Deleted || root.Hidden {
		httpapi.Fail(w, r, 404, "CONTENT_NOT_FOUND", "内容不存在或不可见")
		return
	}
	answerID, _, ok := optionalID(r, "answer_id")
	if !ok {
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "回答ID无效")
		return
	}
	parentID, _, ok := optionalID(r, "parent_comment_id")
	if !ok {
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "评论ID无效")
		return
	}
	filter := commentFilter{TopicID: topicID, AnswerID: answerID, ParentCommentID: parentID}
	// A scope that names a row must name a row a reader can reach, or the filter would be a way to ask
	// whether content exists. The answer scope and the thread scope are checked with the shared rule.
	if filter.AnswerID != 0 {
		answer, err := d.Store.AnswerByContentID(r.Context(), filter.AnswerID)
		if err != nil || answer.TopicID != topicID {
			httpapi.Fail(w, r, 422, "INVALID_COMMENT_TARGET", "评论或回复目标无效")
			return
		}
	}
	if filter.ParentCommentID != 0 {
		parent, err := d.Store.CommentByContentID(r.Context(), filter.ParentCommentID)
		if err != nil || parent.TopicID != topicID || parent.ParentID.Valid {
			httpapi.Fail(w, r, 422, "INVALID_COMMENT_TARGET", "评论或回复目标无效")
			return
		}
		// The replies under a thread root must live in the same narrower scope as the root, or the two
		// levels of the display would disagree about which answer they belong to.
		if (parent.AnswerID.Valid && parent.AnswerID.Int64 != int64(filter.AnswerID)) ||
			(!parent.AnswerID.Valid && filter.AnswerID != 0) {
			httpapi.Fail(w, r, 422, "INVALID_COMMENT_TARGET", "评论或回复目标无效")
			return
		}
	}
	page, ok := d.parsePageOrFail(w, r, filter.cursorScope())
	if !ok {
		return
	}
	comments, hasMore, lastExamined, err := d.Store.ListComments(r.Context(), filter, page)
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	view := contentPageView{Items: make([]any, 0, len(comments)), HasMore: hasMore}
	for _, comment := range comments {
		view.Items = append(view.Items, toCommentResponse(comment))
	}
	if hasMore && lastExamined != 0 {
		view.NextCursor = encodeCursor(filter.cursorScope(), int64(lastExamined))
	}
	httpapi.JSON(w, http.StatusOK, view)
}

// listTags serves the public tag list. Tags are administrator-maintained and public, so guests read them.
func (d Deps) listTags(w http.ResponseWriter, r *http.Request) {
	// The scope carries the search text so a cursor issued for one search is not replayed against another.
	// The cursor is a single component, so the separator is stripped from the text; this is a guard against
	// cross-list replay, not a security boundary.
	search := r.URL.Query().Get("q")
	scope := tagCursorScope + ":" + strings.ReplaceAll(search, "|", "")
	page, ok := d.parsePageOrFail(w, r, scope)
	if !ok {
		return
	}
	tags, hasMore, err := d.Store.ListTags(r.Context(), search, page)
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	view := tagPageView{Items: make([]tagResponse, 0, len(tags)), HasMore: hasMore}
	for _, tag := range tags {
		view.Items = append(view.Items, tagResponse{ID: strconv.FormatUint(tag.ID, 10), Name: tag.Name, Description: tag.Description})
	}
	if hasMore && len(tags) > 0 {
		view.NextCursor = encodeCursor(scope, int64(tags[len(tags)-1].ID))
	}
	httpapi.JSON(w, http.StatusOK, view)
}
