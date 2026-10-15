package content

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"opentalk/server/internal/platform/httpapi"
)

// Search over public content.
//
// Two things are deliberately modest, and both are recorded rather than hidden:
//
//   - the match is a substring (`LIKE '%q%'`) over the stored Markdown, not a full-text index. The schema
//     has no full-text index and MySQL's ngram parser would have to be added to one, so this is the honest
//     first implementation: correct, visible-only, and linear. The contract already says the Chinese search
//     strategy still needs corpus validation, and this is what that note refers to.
//   - ordering is by content id descending, newest first, because there is no relevance signal to order by.
//     It is stable, which is what keyset pagination needs.
//
// Visibility is not re-implemented: the SQL narrows candidates to undeleted, unmoderated rows, the page is
// put through the shared ancestor rule, and each assembled item goes through the same reader every other
// endpoint uses — which checks again. The double check is intentional: the assembler is shared, so it must
// not trust its caller.

// searchMaxQuery bounds the pattern. A long pattern cannot match anything useful and makes the scan more
// expensive for every row, so it is refused rather than truncated.
const searchMaxQuery = 100

// ngramTokenSize mirrors MySQL's `ngram_token_size` (2 by default): a query shorter than one token produces
// no token at all, so the full-text index cannot answer it and the scan path is used instead. One-character
// searches therefore stay correct at the old cost instead of silently returning nothing.
const ngramTokenSize = 2

// booleanOperators are the characters `IN BOOLEAN MODE` gives meaning to. They are removed from user input
// so a query cannot become a different query: "-词" must not read as "exclude 词", nor "*" as a wildcard.
// Stripping is enough because the remaining term is then wrapped in quotes as a phrase.
const booleanOperators = `+-<>()~*"@`

// sanitizeBooleanTerm removes the boolean operators, leaving the words the reader actually typed.
func sanitizeBooleanTerm(query string) string {
	cleaned := strings.Map(func(character rune) rune {
		if strings.ContainsRune(booleanOperators, character) {
			return -1
		}
		return character
	}, query)
	return strings.TrimSpace(cleaned)
}

// searchFilter is the validated search input.
type searchFilter struct {
	Query   string
	BoardID uint64
	TagID   uint64
}

// cursorScope binds the cursor to the filters, so a cursor issued for one search is not replayed against
// another list. The separator is stripped from the free text because the scope is a single cursor
// component; this guards against cross-list replay rather than being a security boundary.
func (f searchFilter) cursorScope() string {
	scope := "search:" + strings.ReplaceAll(f.Query, "|", "")
	if f.BoardID != 0 {
		scope += ":board=" + strconv.FormatUint(f.BoardID, 10)
	}
	if f.TagID != 0 {
		scope += ":tag=" + strconv.FormatUint(f.TagID, 10)
	}
	return scope
}

// SearchContent returns one page of content ids matching the filter, newest first, plus the id the page
// should be resumed from.
//
// The resume id is the last row *examined*, not the last row returned: a row the visibility rule refuses
// still has to move the cursor, or a page with nothing visible on it could never be left behind.
func (s Store) SearchContent(ctx context.Context, filter searchFilter, page pageRequest) ([]uint64, bool, uint64, error) {
	conditions := []string{
		`c.deleted_at IS NULL`,
		`c.moderation_status='visible'`,
	}
	args := []any{}
	// The joins are shared by both shapes: the filters below and the reader above them both need to reach the
	// root topic of a row.
	const joins = `
LEFT JOIN topics t ON t.content_id=c.id
LEFT JOIN answers a ON a.content_id=c.id
LEFT JOIN comments cm ON cm.content_id=c.id
LEFT JOIN topics rt ON rt.content_id=COALESCE(t.content_id,a.question_id,cm.topic_id)`

	from := `FROM contents c` + joins
	term := sanitizeBooleanTerm(filter.Query)
	// Nothing searchable survived sanitising (the query was only operators). Answering "no matches" is right,
	// and checking it before the branch below matters: an empty term would otherwise become the pattern `%%`,
	// which matches everything.
	if term == "" {
		return nil, false, 0, nil
	}
	// The decision is made on the sanitised term, not on what was typed: "-词" is one word after the operator
	// is stripped, so it must take the scan path rather than be sent to an index that cannot tokenise it.
	if utf8.RuneCountInString(term) >= ngramTokenSize {
		// The candidates come from two index lookups, unioned, and only then joined back to `contents`.
		//
		// This shape is deliberate and measured: writing the same two MATCHes as an OR in one WHERE clause
		// prevents either index from being used at all (the planner falls back to a scan), while the union of
		// two index-backed subqueries reads only the matching rows. The phrase form (`"term"`) is what makes an
		// ngram match behave like the substring search it replaces.
		phrase := `"` + term + `"`
		from = `FROM (
 SELECT c0.id AS id FROM contents c0 WHERE MATCH(c0.body_md) AGAINST (? IN BOOLEAN MODE)
 UNION
 SELECT t0.content_id AS id FROM topics t0 WHERE MATCH(t0.title) AGAINST (? IN BOOLEAN MODE)
) m
JOIN contents c ON c.id=m.id` + joins
		args = append(args, phrase, phrase)
	} else {
		// Shorter than one ngram token, so there is no index to use: match the body for every kind and the
		// title where there is one, which is what a reader expects from a forum search.
		pattern := "%" + escapeLike(term) + "%"
		conditions = append(conditions, `(c.body_md LIKE ? ESCAPE '\\' OR (t.title IS NOT NULL AND t.title LIKE ? ESCAPE '\\'))`)
		args = append(args, pattern, pattern)
	}
	if filter.BoardID != 0 {
		conditions = append(conditions, `rt.board_id=?`)
		args = append(args, filter.BoardID)
	}
	if filter.TagID != 0 {
		conditions = append(conditions, `EXISTS(SELECT 1 FROM topic_tags tt WHERE tt.topic_id=rt.content_id AND tt.tag_id=?)`)
		args = append(args, filter.TagID)
	}
	if len(page.After) == 1 {
		conditions = append(conditions, `c.id<?`)
		args = append(args, page.After[0])
	}
	query := `SELECT c.id ` + from + `
WHERE ` + strings.Join(conditions, ` AND `) + `
ORDER BY c.id DESC LIMIT ?`
	args = append(args, page.Limit+1)

	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, 0, err
	}
	candidates, err := scanRows(rows, func(rows *sql.Rows) (uint64, error) {
		var id uint64
		return id, rows.Scan(&id)
	})
	if err != nil {
		return nil, false, 0, err
	}
	hasMore := len(candidates) > page.Limit
	if hasMore {
		candidates = candidates[:page.Limit]
	}
	lastExamined := uint64(0)
	if len(candidates) > 0 {
		lastExamined = candidates[len(candidates)-1]
	}
	if len(candidates) == 0 {
		return nil, hasMore, lastExamined, nil
	}

	visible, err := visibleContentIDs(ctx, s.DB, candidates)
	if err != nil {
		return nil, false, 0, err
	}
	found := make([]uint64, 0, len(candidates))
	for _, id := range candidates {
		if visible[id] {
			found = append(found, id)
		}
	}
	return found, hasMore, lastExamined, nil
}

// searchContents serves the public search endpoint.
func (d Deps) searchContents(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "请填写搜索词")
		return
	}
	if utf8.RuneCountInString(query) > searchMaxQuery {
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "搜索词过长")
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
	filter := searchFilter{Query: query, BoardID: boardID, TagID: tagID}
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
	ids, hasMore, lastExamined, err := d.Store.SearchContent(r.Context(), filter, page)
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	view := contentPageView{Items: make([]any, 0, len(ids)), HasMore: hasMore}
	for _, id := range ids {
		item, err := d.Store.PublicContentByID(r.Context(), id)
		if errors.Is(err, ErrContentNotFound) || errors.Is(err, ErrTopicNotFound) {
			// It became unreachable between the ranking query and this read. Dropping it is right: a search
			// result must never be the one place removed content still shows.
			continue
		}
		if err != nil {
			d.failDependency(w, r, err)
			return
		}
		view.Items = append(view.Items, item)
	}
	if hasMore && lastExamined != 0 {
		view.NextCursor = encodeCursor(filter.cursorScope(), int64(lastExamined))
	}
	httpapi.JSON(w, http.StatusOK, view)
}
