package content

import (
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"opentalk/server/internal/auth"
	"opentalk/server/internal/platform/httpapi"
)

// AdminBoardInput is the validated body of a board create or update.
type AdminBoardInput struct {
	Name        string
	Description string
	Kind        string
	SortOrder   int
	IsArchived  bool
}

// AdminTopicInput is the validated body of a topic moderation request. Every pointer is optional; at least
// one of them must be present, because a request that changes nothing is not a moderation decision and
// should not produce an audit row claiming one happened.
type AdminTopicInput struct {
	Reason   string
	IsLocked *bool
	IsPinned *bool
	BoardID  *uint64
}

// boardLengthMax matches the column: VARCHAR(100).
const boardLengthMax = 100

// tagLengthMax matches tags.name (VARCHAR(80)).
const tagLengthMax = 80

// decodeBoardInput validates a board body. The contract's required fields are enforced: name, description
// and kind carry meaning, so omitting one is refused rather than guessed. sort_order and is_archived are
// optional because their columns have defaults, and a default of 0/false is not a change anyone has to
// agree to.
func decodeBoardInput(w http.ResponseWriter, r *http.Request) (AdminBoardInput, bool) {
	var request struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
		Kind        string  `json:"kind"`
		SortOrder   *int    `json:"sort_order"`
		IsArchived  *bool   `json:"is_archived"`
	}
	if !decodeJSON(w, r, &request) {
		return AdminBoardInput{}, false
	}
	if request.Name == nil || request.Description == nil {
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "name 与 description 都必须提供")
		return AdminBoardInput{}, false
	}
	name := strings.TrimSpace(*request.Name)
	if name == "" {
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "请填写板块名称")
		return AdminBoardInput{}, false
	}
	if utf8.RuneCountInString(name) > boardLengthMax {
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "板块名称过长")
		return AdminBoardInput{}, false
	}
	if request.Kind != "discussion" && request.Kind != "qa" {
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "kind 只能是 discussion 或 qa")
		return AdminBoardInput{}, false
	}
	input := AdminBoardInput{Name: name, Description: strings.TrimSpace(*request.Description), Kind: request.Kind}
	if request.SortOrder != nil {
		if *request.SortOrder < 0 {
			httpapi.Fail(w, r, 422, "INVALID_REQUEST", "sort_order 不能为负")
			return AdminBoardInput{}, false
		}
		input.SortOrder = *request.SortOrder
	}
	if request.IsArchived != nil {
		input.IsArchived = *request.IsArchived
	}
	return input, true
}

// createBoard serves POST /api/v1/admin/boards.
func (d Deps) createBoard(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.FromContext(r.Context())
	if !ok {
		httpapi.Fail(w, r, 401, "UNAUTHENTICATED", "请先登录")
		return
	}
	input, ok := decodeBoardInput(w, r)
	if !ok {
		return
	}
	boardID, err := d.Store.SaveBoard(r.Context(), principal.UserID, nil, input)
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	view, err := d.Store.BoardViewByID(r.Context(), boardID)
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	httpapi.JSON(w, http.StatusCreated, view)
}

// updateBoard serves PUT /api/v1/admin/boards/{board_id}.
func (d Deps) updateBoard(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.FromContext(r.Context())
	if !ok {
		httpapi.Fail(w, r, 401, "UNAUTHENTICATED", "请先登录")
		return
	}
	boardID, ok := pathID(w, r, "board_id")
	if !ok {
		return
	}
	input, ok := decodeBoardInput(w, r)
	if !ok {
		return
	}
	_, err := d.Store.SaveBoard(r.Context(), principal.UserID, &boardID, input)
	switch {
	case errors.Is(err, ErrContentNotFound):
		httpapi.Fail(w, r, 404, "BOARD_NOT_FOUND", "板块不存在")
		return
	case errors.Is(err, ErrBoardKindLocked):
		// Not a validation slip: changing the kind would reinterpret every topic already in the board.
		httpapi.Fail(w, r, 409, "BOARD_KIND_LOCKED", "该板块已有主题，不能再更改类型")
		return
	case err != nil:
		d.failDependency(w, r, err)
		return
	}
	view, err := d.Store.BoardViewByID(r.Context(), boardID)
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, view)
}

// decodeTagInput validates a tag body. The contract lists both fields as required, and they are enforced:
// a defaulted description would be a change nobody asked for, while sort_order and is_archived on a board
// are left optional because the columns have defaults of their own.
func decodeTagInput(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	var request struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
	}
	if !decodeJSON(w, r, &request) {
		return "", "", false
	}
	if request.Name == nil || request.Description == nil {
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "name 与 description 都必须提供")
		return "", "", false
	}
	name := strings.TrimSpace(*request.Name)
	if name == "" {
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "请填写标签名")
		return "", "", false
	}
	if utf8.RuneCountInString(name) > tagLengthMax {
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "标签名过长")
		return "", "", false
	}
	if normalizeTagName(name) == "" {
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "标签名不能只由空白组成")
		return "", "", false
	}
	return name, strings.TrimSpace(*request.Description), true
}

// createTag serves POST /api/v1/admin/tags.
func (d Deps) createTag(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.FromContext(r.Context())
	if !ok {
		httpapi.Fail(w, r, 401, "UNAUTHENTICATED", "请先登录")
		return
	}
	name, description, ok := decodeTagInput(w, r)
	if !ok {
		return
	}
	tagID, err := d.Store.SaveTag(r.Context(), principal.UserID, nil, name, description)
	if errors.Is(err, ErrDuplicateTagName) {
		httpapi.Fail(w, r, 409, "TAG_NAME_TAKEN", "已存在同名标签")
		return
	}
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	view, err := d.Store.TagViewByID(r.Context(), tagID)
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	httpapi.JSON(w, http.StatusCreated, view)
}

// updateTag serves PUT /api/v1/admin/tags/{tag_id}.
func (d Deps) updateTag(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.FromContext(r.Context())
	if !ok {
		httpapi.Fail(w, r, 401, "UNAUTHENTICATED", "请先登录")
		return
	}
	tagID, ok := pathID(w, r, "tag_id")
	if !ok {
		return
	}
	name, description, ok := decodeTagInput(w, r)
	if !ok {
		return
	}
	_, err := d.Store.SaveTag(r.Context(), principal.UserID, &tagID, name, description)
	switch {
	case errors.Is(err, ErrDuplicateTagName):
		httpapi.Fail(w, r, 409, "TAG_NAME_TAKEN", "已存在同名标签")
		return
	case errors.Is(err, ErrContentNotFound):
		httpapi.Fail(w, r, 404, "TAG_NOT_FOUND", "标签不存在")
		return
	case err != nil:
		d.failDependency(w, r, err)
		return
	}
	view, err := d.Store.TagViewByID(r.Context(), tagID)
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, view)
}

// moderateTopic serves PATCH /api/v1/admin/topics/{topic_id}.
func (d Deps) moderateTopic(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.FromContext(r.Context())
	if !ok {
		httpapi.Fail(w, r, 401, "UNAUTHENTICATED", "请先登录")
		return
	}
	topicID, ok := pathID(w, r, "topic_id")
	if !ok {
		return
	}
	var request struct {
		Reason   string  `json:"reason"`
		IsLocked *bool   `json:"is_locked"`
		IsPinned *bool   `json:"is_pinned"`
		BoardID  *string `json:"board_id"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	reason := strings.TrimSpace(request.Reason)
	if reason == "" {
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "请填写操作原因")
		return
	}
	if request.IsLocked == nil && request.IsPinned == nil && request.BoardID == nil {
		// The contract's minProperties: a decision has to decide something.
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "至少要修改锁定、置顶或所属板块之一")
		return
	}
	input := AdminTopicInput{Reason: reason, IsLocked: request.IsLocked, IsPinned: request.IsPinned}
	if request.BoardID != nil {
		boardID, err := ParseID(*request.BoardID)
		if err != nil {
			httpapi.Fail(w, r, 422, "INVALID_REQUEST", "board_id 无效")
			return
		}
		input.BoardID = &boardID
	}
	err := d.Store.ModerateTopic(r.Context(), principal.UserID, topicID, input)
	switch {
	case errors.Is(err, ErrContentNotFound):
		httpapi.Fail(w, r, 404, "CONTENT_NOT_FOUND", "内容不存在或已删除")
		return
	case errors.Is(err, ErrBoardKindMismatch):
		httpapi.Fail(w, r, 409, "BOARD_KIND_MISMATCH", "只能移动到同类型板块")
		return
	case err != nil:
		d.failDependency(w, r, err)
		return
	}
	item, err := d.Store.PublicContentByID(r.Context(), topicID)
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, item)
}
