package content

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

var (
	ErrBoardNotFound         = errors.New("board not found")
	ErrBoardArchived         = errors.New("board is archived")
	ErrAccountMuted          = errors.New("account is muted")
	ErrTagNotFound           = errors.New("tag not found")
	ErrTooManyTags           = errors.New("too many tags")
	ErrAttachmentUnavailable = errors.New("attachment unavailable")
	ErrTooManyAttachments    = errors.New("too many attachments")
	ErrEmailOverride         = errors.New("invalid email override")
)

// TagLimit is the confirmed per-content tag limit. It is a product rule rather than an administrator
// setting, so it is a constant here and maxItems in the contract.
const TagLimit = 5

// emailOverrides mirrors the column enum. "inherit" is the stored default and therefore needs no row.
var emailOverrides = map[string]bool{"inherit": true, "enabled": true, "disabled": true}

// NewTopic is validated input for creating a discussion topic or a question. Which one it becomes is
// decided by the board's kind, not by the request.
type NewTopic struct {
	AuthorID      uint64
	BoardID       uint64
	Title         string
	BodyMD        string
	IsAnonymous   bool
	TagIDs        []uint64
	AttachmentIDs []uint64
	EmailOverride string
	// MaxAttachments comes from the administrator setting, so this layer never reads settings itself.
	MaxAttachments int
}

// CreateTopic writes a topic or question in one transaction.
//
// The insert order is forced by the schema: contents first, then the topic row that
// anonymous_identities references, then the mapping, then the backfill. contents.anonymous_identity_id
// is briefly NULL only because the composite foreign key would otherwise be circular; the transaction
// never commits an anonymous topic without its mapping.
func (s Store) CreateTopic(ctx context.Context, in NewTopic) (uint64, error) {
	if len(in.TagIDs) > TagLimit {
		return 0, ErrTooManyTags
	}
	if in.MaxAttachments > 0 && len(in.AttachmentIDs) > in.MaxAttachments {
		return 0, ErrTooManyAttachments
	}
	if in.EmailOverride != "" && !emailOverrides[in.EmailOverride] {
		return 0, ErrEmailOverride
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	// A shared lock on the board keeps a concurrent archive from racing this create: archiving takes an
	// exclusive lock, so either it completes first (and this sees is_archived) or it waits.
	var boardKind string
	var archived bool
	err = tx.QueryRowContext(ctx, `SELECT kind,is_archived FROM boards WHERE id=? FOR SHARE`, in.BoardID).Scan(&boardKind, &archived)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrBoardNotFound
	}
	if err != nil {
		return 0, err
	}
	if archived {
		return 0, ErrBoardArchived
	}
	// Muting blocks new content. The session check only refuses banned accounts, so the restriction is
	// evaluated here, inside the transaction, where a concurrent mute cannot be missed.
	if err := refuseMuted(ctx, tx, in.AuthorID); err != nil {
		return 0, err
	}
	if err := requireAllTags(ctx, tx, in.TagIDs); err != nil {
		return 0, err
	}
	if err := requireAllAttachments(ctx, tx, in.AttachmentIDs, in.AuthorID); err != nil {
		return 0, err
	}

	result, err := tx.ExecContext(ctx,
		`INSERT INTO contents (author_id,kind,body_md,is_anonymous) VALUES (?,'topic',?,?)`,
		in.AuthorID, in.BodyMD, in.IsAnonymous)
	if err != nil {
		return 0, fmt.Errorf("cannot insert content: %w", err)
	}
	contentID, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	topicID := uint64(contentID)

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO topics (content_id,board_id,title,last_activity_at) VALUES (?,?,?,UTC_TIMESTAMP(6))`,
		topicID, in.BoardID, in.Title); err != nil {
		return 0, fmt.Errorf("cannot insert topic: %w", err)
	}
	if boardKind == "qa" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO questions (topic_id) VALUES (?)`, topicID); err != nil {
			return 0, fmt.Errorf("cannot insert question: %w", err)
		}
	}
	if in.IsAnonymous {
		identityID, _, err := ensureAnonymousIdentity(ctx, tx, topicID, in.AuthorID)
		if err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE contents SET anonymous_identity_id=? WHERE id=?`, identityID, topicID); err != nil {
			return 0, fmt.Errorf("cannot attach anonymous identity: %w", err)
		}
	}
	for _, tagID := range dedupe(in.TagIDs) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO topic_tags (topic_id,tag_id) VALUES (?,?)`, topicID, tagID); err != nil {
			return 0, fmt.Errorf("cannot attach tag: %w", err)
		}
	}
	for _, attachmentID := range dedupe(in.AttachmentIDs) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO content_attachments (content_id,attachment_id) VALUES (?,?)`, topicID, attachmentID); err != nil {
			return 0, fmt.Errorf("cannot attach image: %w", err)
		}
	}
	if err := promoteAttachments(ctx, tx, in.AttachmentIDs); err != nil {
		return 0, err
	}
	// "inherit" is the stored default, so only an explicit override needs a row.
	if in.EmailOverride != "" && in.EmailOverride != "inherit" {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO topic_subscriptions (user_id,topic_id,email_override) VALUES (?,?,?)`,
			in.AuthorID, topicID, in.EmailOverride); err != nil {
			return 0, fmt.Errorf("cannot store subscription override: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return topicID, nil
}

// refuseMuted reports ErrAccountMuted while a mute is in force. It takes the shared read surface rather
// than a transaction, because the permission endpoint asks the same question outside any write.
func refuseMuted(ctx context.Context, q queryer, userID uint64) error {
	var muted bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM user_restrictions r
 WHERE r.user_id=? AND r.kind='mute' AND r.revoked_at IS NULL AND r.starts_at<=UTC_TIMESTAMP(6)
 AND (r.ends_at IS NULL OR r.ends_at>UTC_TIMESTAMP(6)))`, userID).Scan(&muted)
	if err != nil {
		return err
	}
	if muted {
		return ErrAccountMuted
	}
	return nil
}

func requireAllTags(ctx context.Context, tx *sql.Tx, ids []uint64) error {
	unique := dedupe(ids)
	if len(unique) == 0 {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM tags WHERE id IN (`+placeholders(len(unique))+`)`, toAny(unique)...)
	if err != nil {
		return err
	}
	found, err := scanRows(rows, func(rows *sql.Rows) (uint64, error) {
		var id uint64
		return id, rows.Scan(&id)
	})
	if err != nil {
		return err
	}
	if len(found) != len(unique) {
		return ErrTagNotFound
	}
	return nil
}

// requireAllAttachments checks existence, ownership and status together. A mismatch is reported as one
// error so the response never reveals whether an id exists for somebody else.
func requireAllAttachments(ctx context.Context, tx *sql.Tx, ids []uint64, authorID uint64) error {
	unique := dedupe(ids)
	if len(unique) == 0 {
		return nil
	}
	args := append(toAny(unique), authorID)
	rows, err := tx.QueryContext(ctx, `SELECT id FROM attachments WHERE id IN (`+placeholders(len(unique))+
		`) AND owner_id=? AND status IN ('pending','active')`, args...)
	if err != nil {
		return err
	}
	found, err := scanRows(rows, func(rows *sql.Rows) (uint64, error) {
		var id uint64
		return id, rows.Scan(&id)
	})
	if err != nil {
		return err
	}
	if len(found) != len(unique) {
		return ErrAttachmentUnavailable
	}
	return nil
}

// TopicView is one topic assembled for a response. The anonymous branch deliberately carries no public
// id, nickname or avatar: the contract forbids attaching real identity to anonymous content.
type TopicView struct {
	ContentID uint64
	// AuthorID is the real account behind the content. It is never serialised: it exists so later
	// authorization checks (edit, delete) do not have to re-derive the owner.
	AuthorID        uint64
	BoardID         uint64
	BoardKind       string
	Title           string
	BodyMD          string
	IsAnonymous     bool
	AuthorPublicID  string
	AuthorNickname  string
	AuthorAvatarKey string
	AuthorCreatedAt time.Time
	AnonymousNumber int
	CreatedAt       time.Time
	EditedAt        sql.NullTime
	IsLocked        bool
	IsPinned        bool
	LastActivityAt  time.Time
	ResolutionState sql.NullString
	ResolvedAt      sql.NullTime
	Tags            []Tag
	Attachments     []Attachment
	// createdKey and activityKey are the list orderings projected by the query (see read.go). They are
	// carried on the view so a page's cursor is the value the rows were sorted by, not a second computation
	// in Go that could disagree with it.
	createdKey  int64
	activityKey int64
}

// Tag is a tag attached to a topic.
type Tag struct {
	ID          uint64
	Name        string
	Description string
}

// Attachment is an image attached to a topic. Public responses never carry owner_id or storage_key.
type Attachment struct {
	ID       uint64
	Key      string
	MIMEType string
	ByteSize int64
}

// TopicByContentID assembles one reachable topic. Hidden or deleted content is reported as not found so
// the caller cannot use this endpoint to probe for removed material. A topic is its own ancestor, so the
// shared rule reduces to this one row here, but it is still the shared rule rather than a copy of it.
func (s Store) TopicByContentID(ctx context.Context, contentID uint64) (TopicView, error) {
	if err := requireVisible(ctx, s.DB, contentID); err != nil {
		if errors.Is(err, ErrContentNotFound) {
			return TopicView{}, ErrTopicNotFound
		}
		return TopicView{}, err
	}
	var view TopicView
	// The column list and joins live in read.go so the list reader and this one cannot drift apart.
	view, err := scanTopicView(s.DB.QueryRowContext(ctx, topicViewQuery+` WHERE c.id=?`, contentID))
	if errors.Is(err, sql.ErrNoRows) {
		return view, ErrTopicNotFound
	}
	if err != nil {
		return view, err
	}
	if view.Tags, err = s.topicTags(ctx, contentID); err != nil {
		return view, err
	}
	if view.Attachments, err = s.topicAttachments(ctx, contentID); err != nil {
		return view, err
	}
	return view, nil
}

var ErrTopicNotFound = errors.New("topic not found")

// topicTags and topicAttachments read one content's details. Both delegate to the batched readers in
// read.go, so a list page and a single row answer from the same queries.
func (s Store) topicTags(ctx context.Context, contentID uint64) ([]Tag, error) {
	grouped, err := s.tagsForContents(ctx, []uint64{contentID})
	if err != nil {
		return nil, err
	}
	return grouped[contentID], nil
}

func (s Store) topicAttachments(ctx context.Context, contentID uint64) ([]Attachment, error) {
	grouped, err := s.attachmentsForContents(ctx, []uint64{contentID})
	if err != nil {
		return nil, err
	}
	return grouped[contentID], nil
}

// placeholders builds "?,?,?" for an IN clause of n values.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func toAny(values []uint64) []any {
	out := make([]any, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	return out
}

// dedupe preserves order while removing repeats, so a duplicated id cannot violate a composite primary
// key mid-transaction.
func dedupe(values []uint64) []uint64 {
	seen := make(map[uint64]bool, len(values))
	out := make([]uint64, 0, len(values))
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

// ParseID reads a decimal id from JSON. Ids travel as strings because they are BIGINT UNSIGNED.
func ParseID(raw string) (uint64, error) {
	value, err := strconv.ParseUint(strings.TrimSpace(raw), 10, 64)
	if err != nil || value == 0 {
		return 0, fmt.Errorf("invalid id")
	}
	return value, nil
}

// ParseIDs reads a list of decimal ids, returning the first invalid entry as an error.
func ParseIDs(raw []string) ([]uint64, error) {
	out := make([]uint64, 0, len(raw))
	for _, item := range raw {
		value, err := ParseID(item)
		if err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	return out, nil
}
