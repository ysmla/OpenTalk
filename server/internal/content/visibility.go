package content

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Visibility is the one rule every reader of content shares: a piece of content is reachable only when it
// and every ancestor above it are undeleted and unmoderated.
//
// The rule used to be written out at each call site, and the copies drifted. Image serving resolved only
// the root topic, so hiding an answer left its comments' images public. Edit and delete checked the root
// topic and the target row but skipped everything between them, so a hidden answer stayed editable by its
// author. And the single-content readers for an answer or a comment checked their own row and their root,
// so hiding a *question* left its answers and comments readable whenever no soft-delete cascade had run —
// which is exactly what hiding does, since hiding is moderation and does not touch descendant rows.
//
// All of them were the same mistake: sampling a chain instead of walking it. So the whole ancestor graph
// is walked here, once, and every reader asks this instead of assembling its own predicate — including the
// image route, which has a second question (which content still shows this image?) but the same ancestor
// rule behind it.
//
// The graph is small and acyclic, so a plain breadth-first walk is enough and no recursive query is
// needed. A row's ancestors are:
//
//   - a topic has none, so the walk ends where it started;
//   - an answer answers exactly one question (answers.question_id);
//   - a comment belongs to one topic (comments.topic_id), and *also*, when it is shown inside something
//     narrower, to that thing: the answer it hangs from (comments.answer_id) and the thread root it is
//     displayed under (comments.parent_comment_id, which stores the display root rather than the
//     immediate reply target).
//
// The "also" is the part that is easy to get wrong, and getting it wrong is a permission leak rather than
// a cosmetic bug. A reply under an answer carries both an answer and a thread root, and the two are
// independent: hiding the thread root has to hide the reply even though the answer is still public, and
// hiding the answer has to hide it even though the thread root is still public. Picking either one as
// "the" ancestor — which is what a COALESCE over the two columns did — samples the graph instead of
// walking it and leaves the other branch unchecked. So every ancestor of a row is visited, not one of
// them.
//
// Visibility therefore never means "this row happens to be visible"; it means "a reader can reach this row
// at all". Hiding and deleting are the same fact to a reader but live in two columns, so both are checked
// at every step rather than collapsed into one.

// ancestorsExpression is the immediate ancestors of a content row, as a comma-separated list. It is
// deliberately not parameterised: every use site aliases contents as c and joins topics/answers/comments
// as t/a/cm, which is what keeps the expression identical everywhere rather than merely similar. A topic
// has no ancestor and answers with an empty list, which ends the walk; a comment with an answer *and* a
// thread root lists both, because both have to be checked.
//
// It is one expression rather than three columns so that a caller cannot accidentally treat "an ancestor"
// as a single value: the failure this replaced was exactly that a reader took one ancestor and stopped.
const ancestorsExpression = "CONCAT_WS(',',t.content_id,a.question_id,cm.answer_id,cm.parent_comment_id,cm.topic_id)"

// contentJoins is the FROM/join clause the ancestors expression reads. It is written once so the
// expression above and the tables behind it cannot drift apart.
const contentJoins = `
FROM contents c
LEFT JOIN topics t ON t.content_id=c.id
LEFT JOIN answers a ON a.content_id=c.id
LEFT JOIN comments cm ON cm.content_id=c.id`

// maxAncestorDepth bounds the walk. The schema allows at most four steps (a reply, its thread root, an
// answer, the topic), so reaching this bound means the rows form a cycle the schema forbids. The bound is
// what stops a corrupt row from turning a read into an infinite loop; it is a safety net, not a depth
// limit on legal content.
const maxAncestorDepth = 32

// queryer is the read surface shared by *sql.DB and *sql.Tx, so the visibility rule has exactly one
// implementation and can run inside a transaction as well as outside one. That matters because a write
// must re-check visibility after taking the root lock, not before it.
type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// rootTopicOf finds the discussion a content row belongs to: itself for a topic, the question for an
// answer, and the topic for a comment. This is the chain's root, which is what the uniform lock order
// needs. Each kind has exactly one discussion, so unlike the ancestor walk below a single COALESCE is
// right here: a comment's answer and its thread root both live under the same topic by schema, and
// comments.topic_id names it directly.
//
// It is not the visibility rule: locking the root and checking the root says nothing about the nodes in
// between, which is the gap this file exists to close.
func (s Store) rootTopicOf(ctx context.Context, contentID uint64) (uint64, error) {
	var root sql.NullInt64
	err := s.DB.QueryRowContext(ctx, `
SELECT COALESCE(t.content_id,a.question_id,cm.topic_id)`+contentJoins+`
WHERE c.id=?`, contentID).Scan(&root)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrContentNotFound
	}
	if err != nil {
		return 0, err
	}
	if !root.Valid {
		// The row exists but belongs to no discussion, which the schema does not allow and would mean a
		// corrupt row rather than a missing one.
		return 0, ErrContentNotFound
	}
	return uint64(root.Int64), nil
}

// contentVisible reports whether one content row is reachable by a reader: the row itself and every
// ancestor above it, along every branch it can be shown through, must be undeleted and unmoderated.
func (s Store) contentVisible(ctx context.Context, contentID uint64) (bool, error) {
	return contentVisible(ctx, s.DB, contentID)
}

// contentVisible runs the same rule on whatever queryer the caller has. A write passes its *sql.Tx so the
// check happens after the root lock is taken, not before it.
func contentVisible(ctx context.Context, q queryer, contentID uint64) (bool, error) {
	visible, err := visibleContentIDs(ctx, q, []uint64{contentID})
	if err != nil {
		return false, err
	}
	return visible[contentID], nil
}

// requireVisible is the visibility rule as a precondition. It answers ErrContentNotFound, the same error
// a nonexistent id produces, so no endpoint can be used to tell "hidden" from "never existed".
func requireVisible(ctx context.Context, q queryer, contentID uint64) error {
	visible, err := contentVisible(ctx, q, contentID)
	if err != nil {
		return err
	}
	if !visible {
		return ErrContentNotFound
	}
	return nil
}

// ancestorsReachable reports whether everything *above* one row is undeleted and unmoderated. The row
// itself is deliberately excluded, because the one caller is the tombstone route: a deleted row is what it
// is describing, and the question there is only whether the reader may learn that the row existed.
//
// A row with no ancestors — a topic — is trivially reachable. Everything else has to pass the same rule
// every other reader uses, so a deleted comment under a hidden answer still reports the not-found that an
// id nobody ever issued would produce.
func ancestorsReachable(ctx context.Context, q queryer, contentID uint64) (bool, error) {
	closure, err := ancestorClosure(ctx, q, []uint64{contentID})
	if err != nil {
		return false, err
	}
	ancestors := make([]uint64, 0, len(closure))
	for _, id := range closure {
		if id != contentID {
			ancestors = append(ancestors, id)
		}
	}
	if len(ancestors) == 0 {
		return true, nil
	}
	visible, err := visibleContentIDs(ctx, q, ancestors)
	if err != nil {
		return false, err
	}
	for _, id := range ancestors {
		if !visible[id] {
			return false, nil
		}
	}
	return true, nil
}

// visibleContentIDs answers one question per requested row: is that row reachable, meaning is every row in
// its ancestor closure undeleted and unmoderated?
//
// The set is asked together because the image route needs it that way. A stored image is served while
// *some* content row showing it is reachable, so the reference rows are passed in as a set and the caller
// combines the answers — rather than the caller asking the same question itself in its own SQL, which is
// the copy that drifted and let a hidden ancestor of one reference deny an image another public reference
// still showed.
//
// Reachability is per row, but its *verdict* spans a whole closure: the row is reachable only when every
// row in its closure is visible, because a hidden ancestor is exactly what makes content unreachable. So
// the closure is computed per root and the per-row visibility map is consulted for each entry. Sharing one
// closure walk and one visibility query across the roots keeps that from costing a query per chain, which
// matters on the image route, where a popular image can carry many references.
//
// A row that does not exist is reported unreachable, and so is one whose chain the schema forbids: asking
// about a row that is not there must never be the permissive branch.
func visibleContentIDs(ctx context.Context, q queryer, contentIDs []uint64) (map[uint64]bool, error) {
	reachable := make(map[uint64]bool, len(contentIDs))
	roots := dedupe(contentIDs)
	if len(roots) == 0 {
		return reachable, nil
	}

	// One shared walk over every root's ancestors, kept per root. The closures overlap heavily — every
	// comment in a topic shares the topic — so the rows are collected into one visibility query below, but
	// each root keeps its own closure because reachability is decided per root.
	byRoot := make([][]uint64, len(roots))
	all := make([]uint64, 0, len(roots)*4)
	for index, root := range roots {
		closure, err := ancestorClosure(ctx, q, []uint64{root})
		if err != nil {
			return nil, err
		}
		byRoot[index] = closure
		all = append(all, closure...)
	}
	all = dedupe(all)

	// Every row that decides any root's verdict, tested in one query. A missing id is simply absent here,
	// which the loop below reads as not visible.
	visible := make(map[uint64]bool, len(all))
	rows, err := q.QueryContext(ctx, `SELECT c.id,
 COALESCE(SUM(c.deleted_at IS NOT NULL OR c.moderation_status<>'visible'),0)
`+contentJoins+`
WHERE c.id IN (`+placeholders(len(all))+`)
GROUP BY c.id`, toAny(all)...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	if _, err := scanRows(rows, func(rows *sql.Rows) (struct{}, error) {
		var id uint64
		var hidden int
		if err := rows.Scan(&id, &hidden); err != nil {
			return struct{}{}, err
		}
		visible[id] = hidden == 0
		return struct{}{}, nil
	}); err != nil {
		return nil, err
	}

	// A root is reachable when its whole closure is visible, and unreachable otherwise — including when a
	// row of the closure is missing, which reads as not visible. A root that does not exist closes this
	// over an empty closure and would read as reachable, so it is refused explicitly.
	for index, root := range roots {
		reachable[root] = len(byRoot[index]) > 0
		for _, row := range byRoot[index] {
			if !visible[row] {
				reachable[root] = false
				break
			}
		}
	}
	return reachable, nil
}

// ancestorClosure returns every row reachable from any of the starting rows by following immediate
// ancestors, including the starting rows themselves.
//
// The walk expands a whole frontier per query instead of one row at a time for two reasons: a comment has
// two ancestors, so the walk branches rather than following a line, and the image route starts from every
// content row that shows one image, so the frontier can be several rows wide from the first step.
//
// A row that is still in the frontier at maxAncestorDepth is data the schema forbids — a cycle — and its
// chain is reported as unreachable by adding it as its own ancestor, which no undeleted, unmoderated row
// can be. That is deliberate: the depth bound is a safety net against a corrupt row turning a read into an
// infinite loop, and it must fail closed rather than stop early and call the rest of the chain visible.
func ancestorClosure(ctx context.Context, q queryer, contentIDs []uint64) ([]uint64, error) {
	found := dedupe(contentIDs)
	known := make(map[uint64]bool, len(found))
	frontier := found
	for depth := 0; len(frontier) > 0; depth++ {
		for _, id := range frontier {
			known[id] = true
		}
		parents := make(map[uint64]struct{}, len(frontier))
		for _, id := range frontier {
			var raw sql.NullString
			err := q.QueryRowContext(ctx, `SELECT `+ancestorsExpression+`
`+contentJoins+`
WHERE c.id=?`, id).Scan(&raw)
			if errors.Is(err, sql.ErrNoRows) {
				// The row disappeared between two queries. Nothing names it any more, so it contributes no
				// ancestor and stays out of the visibility query below, where its absence reads as
				// unreachable.
				continue
			}
			if err != nil {
				return nil, err
			}
			direct, err := splitIDs(raw.String)
			if err != nil {
				return nil, err
			}
			for _, parent := range direct {
				if !known[parent] {
					parents[parent] = struct{}{}
				}
			}
		}
		// A fresh slice per step: the previous frontier is already part of `found`, and appending over its
		// backing array would overwrite the ids collected so far.
		next := make([]uint64, 0, len(parents))
		for parent := range parents {
			next = append(next, parent)
		}
		// Map iteration order is random, so the frontier is sorted to keep the walk reproducible — the
		// result is returned to callers, and an unstable order would make a failure hard to reproduce.
		sort.Slice(next, func(i, j int) bool { return next[i] < next[j] })
		if depth+1 >= maxAncestorDepth && len(next) > 0 {
			// Still expanding at the bound means a cycle, which the schema forbids. Fail the chain closed by
			// naming one of its rows as its own ancestor: no undeleted, unmoderated row can be that, so the
			// visibility query below reports the whole chain unreachable.
			found = append(found, next[0])
			break
		}
		found = append(found, next...)
		frontier = next
	}
	return dedupe(found), nil
}

// splitIDs parses the comma-separated ancestor list the expression above builds. An entry it cannot read
// is an error rather than a skipped ancestor: the list is built out of ids by MySQL, so a malformed entry
// cannot come from a well-formed row, and dropping it would silently narrow the visibility check.
func splitIDs(raw string) ([]uint64, error) {
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	ids := make([]uint64, 0, len(parts))
	for _, part := range parts {
		id, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("visibility: unreadable ancestor id %q: %w", part, err)
		}
		ids = append(ids, id)
	}
	return ids, nil
}
