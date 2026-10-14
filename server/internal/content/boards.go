package content

import (
	"errors"
	"net/http"
	"strconv"

	"opentalk/server/internal/platform/httpapi"
)

// boardView mirrors the Board model. Every BIGINT id is a string in JSON, and optional fields are
// omitted rather than sent as null.
type boardView struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Kind        string `json:"kind"`
	SortOrder   int    `json:"sort_order"`
	IsArchived  bool   `json:"is_archived"`
}

type boardPageView struct {
	Items      []boardView `json:"items"`
	HasMore    bool        `json:"has_more"`
	NextCursor string      `json:"next_cursor,omitempty"`
}

func toBoardView(b Board) boardView {
	return boardView{
		ID:          strconv.FormatUint(b.ID, 10),
		Name:        b.Name,
		Description: b.Description,
		Kind:        b.Kind,
		SortOrder:   b.SortOrder,
		IsArchived:  b.IsArchived,
	}
}

// listBoards serves the board list. It is public: guests may browse, and archived boards stay visible
// because the flag only stops new topics.
func (d Deps) listBoards(w http.ResponseWriter, r *http.Request) {
	page, err := parsePage(r, boardCursorScope)
	switch {
	case errors.Is(err, ErrInvalidCursor):
		httpapi.Fail(w, r, 400, "CURSOR_INVALID", "游标无效，请重新加载列表")
		return
	case errors.Is(err, ErrInvalidLimit):
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "分页数量必须在 1 到 100 之间")
		return
	case err != nil:
		d.failDependency(w, r, err)
		return
	}
	boards, hasMore, err := d.Store.ListBoards(r.Context(), page)
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	// A zero-length slice marshals as [] rather than null, so a client can iterate without a nil check.
	items := make([]boardView, 0, len(boards))
	for _, board := range boards {
		items = append(items, toBoardView(board))
	}
	view := boardPageView{Items: items, HasMore: hasMore}
	if hasMore && len(boards) > 0 {
		last := boards[len(boards)-1]
		view.NextCursor = encodeCursor(boardCursorScope, int64(last.SortOrder), int64(last.ID))
	}
	httpapi.JSON(w, http.StatusOK, view)
}
