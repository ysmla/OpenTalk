package content

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

// Administered structure: boards, tags, and the two topic flags an administrator may change.
//
// Three rules come from the contract and are enforced here rather than left to the caller:
//
//   - **A board's kind is fixed once it has topics.** A discussion board that turns into a Q&A board would
//     silently change what every existing topic means (resolution status, answer threading), so the change is
//     refused instead of reinterpreted.
//   - **A topic may only move to a board of its own kind.** The same reason, one level down.
//   - **The asker's resolution status is not an administrator's to set.** This endpoint does not touch it at
//     all, which is why the request has no such field.
//
// Every change writes an audit row. Board and tag requests carry no operator-supplied reason (the contract
// gives them none), so the audit row states what changed; a topic change carries the operator's own reason.

// Audit actions for this file.
const (
	actionCreateBoard   = "create_board"
	actionUpdateBoard   = "update_board"
	actionCreateTag     = "create_tag"
	actionUpdateTag     = "update_tag"
	actionModerateTopic = "moderate_topic"
)

// ErrDuplicateTagName reports a tag whose normalised name is already taken.
var ErrDuplicateTagName = errors.New("tag name is already in use")

// ErrBoardKindLocked reports an attempt to change the kind of a board that already has topics.
var ErrBoardKindLocked = errors.New("board kind cannot change once topics exist")

// normalizeTagName is the uniqueness key. The column collation is binary, so folding case here is what makes
// "MySQL" and "mysql" the same tag instead of two that look identical.
func normalizeTagName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// BoardViewByID reads one board in the response shape.
func (s Store) BoardViewByID(ctx context.Context, boardID uint64) (boardView, error) {
	var board Board
	var createdAt, updatedAt time.Time
	err := s.DB.QueryRowContext(ctx, `SELECT id,name,description,kind,sort_order,is_archived,created_at,updated_at
 FROM boards WHERE id=?`, boardID).
		Scan(&board.ID, &board.Name, &board.Description, &board.Kind, &board.SortOrder, &board.IsArchived, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return boardView{}, ErrContentNotFound
	}
	if err != nil {
		return boardView{}, err
	}
	return toBoardView(board), nil
}

// TagViewByID reads one tag in the response shape.
func (s Store) TagViewByID(ctx context.Context, tagID uint64) (tagResponse, error) {
	var tag tagResponse
	err := s.DB.QueryRowContext(ctx, `SELECT id,name,description FROM tags WHERE id=?`, tagID).
		Scan(&tag.ID, &tag.Name, &tag.Description)
	if errors.Is(err, sql.ErrNoRows) {
		return tagResponse{}, ErrContentNotFound
	}
	if err != nil {
		return tagResponse{}, err
	}
	return tag, nil
}

// SaveBoard creates or replaces one board. A nil boardID creates.
func (s Store) SaveBoard(ctx context.Context, actorID uint64, boardID *uint64, request AdminBoardInput) (uint64, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	if boardID == nil {
		result, err := tx.ExecContext(ctx, `INSERT INTO boards (name,description,kind,sort_order,is_archived)
VALUES (?,?,?,?,?)`, request.Name, request.Description, request.Kind, request.SortOrder, request.IsArchived)
		if err != nil {
			return 0, err
		}
		id, err := result.LastInsertId()
		if err != nil {
			return 0, err
		}
		if err := writeAudit(ctx, tx, actorID, nil, nil, actionCreateBoard,
			"创建板块 #"+strconv.FormatInt(id, 10)+"："+request.Name); err != nil {
			return 0, err
		}
		if err := tx.Commit(); err != nil {
			return 0, err
		}
		return uint64(id), nil
	}

	var currentKind string
	err = tx.QueryRowContext(ctx, `SELECT kind FROM boards WHERE id=? FOR UPDATE`, *boardID).Scan(&currentKind)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrContentNotFound
	}
	if err != nil {
		return 0, err
	}
	if currentKind != request.Kind {
		// Refused when the board already holds topics: their meaning depends on the kind.
		var topics int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM topics WHERE board_id=?`, *boardID).Scan(&topics); err != nil {
			return 0, err
		}
		if topics > 0 {
			return 0, ErrBoardKindLocked
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE boards SET name=?,description=?,kind=?,sort_order=?,is_archived=? WHERE id=?`,
		request.Name, request.Description, request.Kind, request.SortOrder, request.IsArchived, *boardID); err != nil {
		return 0, err
	}
	if err := writeAudit(ctx, tx, actorID, nil, nil, actionUpdateBoard,
		"修改板块 #"+strconv.FormatUint(*boardID, 10)+"："+request.Name); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return *boardID, nil
}

// SaveTag creates or replaces one tag. A nil tagID creates.
//
// The unique key is on the normalised name, so a duplicate surfaces as a MySQL 1062 rather than as a
// read-then-write check that two concurrent requests could both pass.
func (s Store) SaveTag(ctx context.Context, actorID uint64, tagID *uint64, name, description string) (uint64, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	normalized := normalizeTagName(name)
	action := actionCreateTag
	reason := "创建标签：" + name
	var target uint64
	if tagID == nil {
		result, err := tx.ExecContext(ctx, `INSERT INTO tags (name,normalized_name,description) VALUES (?,?,?)`,
			strings.TrimSpace(name), normalized, description)
		if err != nil {
			return 0, mapDuplicateTag(err)
		}
		id, err := result.LastInsertId()
		if err != nil {
			return 0, err
		}
		target = uint64(id)
		reason = "创建标签 #" + strconv.FormatInt(id, 10) + "：" + name
	} else {
		action = actionUpdateTag
		target = *tagID
		result, err := tx.ExecContext(ctx, `UPDATE tags SET name=?,normalized_name=?,description=? WHERE id=?`,
			strings.TrimSpace(name), normalized, description, *tagID)
		if err != nil {
			return 0, mapDuplicateTag(err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return 0, err
		}
		// A tag that does not exist and one renamed to itself both report zero rows changed; the former has to
		// be a not-found, so it is checked directly.
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM tags WHERE id=?`, *tagID).Scan(&exists); err != nil {
			return 0, err
		}
		if exists == 0 {
			return 0, ErrContentNotFound
		}
		_ = affected
		reason = "修改标签 #" + strconv.FormatUint(*tagID, 10) + "：" + name
	}
	if err := writeAudit(ctx, tx, actorID, nil, nil, action, reason); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return target, nil
}

// mapDuplicateTag translates the unique key on tags.normalized_name into a named error.
func mapDuplicateTag(err error) error {
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
		return ErrDuplicateTagName
	}
	return err
}

// ModerateTopic changes a topic's lock, pin or board, and records why.
//
// The board check is done here as well as in SQL: moving a Q&A topic into a discussion board would leave its
// questions and answers pointing at a board whose kind no longer describes them.
func (s Store) ModerateTopic(ctx context.Context, actorID, topicID uint64, input AdminTopicInput) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// The comparison is between the *board* the topic sits in and the board it would move to. Comparing
	// `contents.kind` (which is 'topic') with a board's kind would refuse every move.
	var currentBoardKind string
	err = tx.QueryRowContext(ctx, `SELECT b.kind FROM topics t
 JOIN boards b ON b.id=t.board_id
 JOIN contents c ON c.id=t.content_id
 WHERE t.content_id=? AND c.deleted_at IS NULL FOR UPDATE`, topicID).Scan(&currentBoardKind)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrContentNotFound
	}
	if err != nil {
		return err
	}
	if input.BoardID != nil {
		var targetKind string
		err := tx.QueryRowContext(ctx, `SELECT kind FROM boards WHERE id=?`, *input.BoardID).Scan(&targetKind)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrContentNotFound
		}
		if err != nil {
			return err
		}
		if targetKind != currentBoardKind {
			return ErrBoardKindMismatch
		}
		if _, err := tx.ExecContext(ctx, `UPDATE topics SET board_id=? WHERE content_id=?`, *input.BoardID, topicID); err != nil {
			return err
		}
	}
	if input.IsLocked != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE topics SET is_locked=? WHERE content_id=?`, *input.IsLocked, topicID); err != nil {
			return err
		}
	}
	if input.IsPinned != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE topics SET is_pinned=? WHERE content_id=?`, *input.IsPinned, topicID); err != nil {
			return err
		}
	}
	if err := writeAudit(ctx, tx, actorID, &topicID, nil, actionModerateTopic, input.Reason); err != nil {
		return err
	}
	return tx.Commit()
}

// ErrBoardKindMismatch reports a move to a board of a different kind.
var ErrBoardKindMismatch = errors.New("topic may only move to a board of the same kind")

// writeAudit appends one audit row. It takes a transaction so a change and its record cannot be separated:
// an audit entry without the change, or a change without the entry, is exactly the state this exists to
// prevent.
func writeAudit(ctx context.Context, tx *sql.Tx, actorID uint64, contentID, userID *uint64, action, reason string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO moderation_actions (actor_id,target_content_id,target_user_id,action,reason)
VALUES (?,?,?,?,?)`, actorID, contentID, userID, action, reason)
	return err
}
