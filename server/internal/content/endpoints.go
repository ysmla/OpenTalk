package content

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"opentalk/server/internal/auth"
	"opentalk/server/internal/markdown"
	"opentalk/server/internal/platform/httpapi"
)

// maxJSONBody bounds the request body. A topic body is capped at 20000 runes, which is at most 60 KiB
// in UTF-8 CJK plus escaping, so this leaves room without allowing an unbounded read.
const maxJSONBody = 256 << 10

// createTopicRequest mirrors CreateTopicRequest. Ids arrive as decimal strings because they are BIGINT
// UNSIGNED. is_anonymous and the two arrays are accepted when absent and treated as false and empty:
// that is a superset of the contract, so a client that always sends them behaves identically.
type createTopicRequest struct {
	BoardID       string   `json:"board_id"`
	Title         string   `json:"title"`
	BodyMD        string   `json:"body_md"`
	AttachmentIDs []string `json:"attachment_ids"`
	IsAnonymous   bool     `json:"is_anonymous"`
	TagIDs        []string `json:"tag_ids"`
	EmailOverride string   `json:"email_override"`
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			httpapi.Fail(w, r, 413, "PAYLOAD_TOO_LARGE", "请求体过大")
			return false
		}
		httpapi.Fail(w, r, 400, "INVALID_JSON", "请求格式无效")
		return false
	}
	return true
}

// createTopic publishes a discussion or a question. The board's kind decides which, so a client cannot
// ask a discussion board for a question.
func (d Deps) createTopic(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.FromContext(r.Context())
	if !ok {
		httpapi.Fail(w, r, 401, "UNAUTHENTICATED", "请先登录")
		return
	}
	var request createTopicRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	boardID, err := ParseID(request.BoardID)
	if err != nil {
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "板块ID无效")
		return
	}
	title, err := markdown.ValidateTitle(request.Title)
	if err != nil {
		code, message := titleFailure(err)
		httpapi.Fail(w, r, 422, code, message)
		return
	}
	body, err := markdown.ValidateBody(request.BodyMD)
	if err != nil {
		code, message := bodyFailure(err)
		httpapi.Fail(w, r, 422, code, message)
		return
	}
	tagIDs, err := ParseIDs(request.TagIDs)
	if err != nil {
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "标签ID无效")
		return
	}
	if len(tagIDs) > TagLimit {
		httpapi.Fail(w, r, 422, "TOO_MANY_TAGS", "每篇内容最多选择 5 个标签")
		return
	}
	attachmentIDs, err := ParseIDs(request.AttachmentIDs)
	if err != nil {
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "图片ID无效")
		return
	}
	if request.EmailOverride != "" && !emailOverrides[request.EmailOverride] {
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "邮件设置取值无效")
		return
	}
	// The per-content image limit is an administrator setting, so it is read here rather than assumed.
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
	topicID, err := d.Store.CreateTopic(r.Context(), NewTopic{
		AuthorID:       principal.UserID,
		BoardID:        boardID,
		Title:          title,
		BodyMD:         body,
		IsAnonymous:    request.IsAnonymous,
		TagIDs:         tagIDs,
		AttachmentIDs:  attachmentIDs,
		EmailOverride:  request.EmailOverride,
		MaxAttachments: current.MediaMaxPerContent,
	})
	if err != nil {
		if d.failCreate(w, r, err) {
			return
		}
		return
	}
	view, err := d.Store.TopicByContentID(r.Context(), topicID)
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	httpapi.JSON(w, http.StatusCreated, toTopicResponse(view))
}

// failCreate maps a write failure to the contract's error codes. It reports whether it wrote a
// response, so the caller never double-writes.
func (d Deps) failCreate(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case errors.Is(err, ErrBoardNotFound):
		httpapi.Fail(w, r, 404, "BOARD_NOT_FOUND", "板块不存在")
	case errors.Is(err, ErrBoardArchived):
		httpapi.Fail(w, r, 409, "BOARD_ARCHIVED", "板块已停止接收新主题")
	case errors.Is(err, ErrAccountMuted):
		httpapi.Fail(w, r, 403, "ACCOUNT_MUTED", "账号已被禁言，无法发布内容")
	case errors.Is(err, ErrTagNotFound):
		httpapi.Fail(w, r, 422, "TAG_NOT_FOUND", "所选标签不存在")
	case errors.Is(err, ErrTooManyTags):
		httpapi.Fail(w, r, 422, "TOO_MANY_TAGS", "每篇内容最多选择 5 个标签")
	case errors.Is(err, ErrAttachmentUnavailable):
		// One message for "missing", "not yours" and "already discarded", so an id cannot be probed.
		httpapi.Fail(w, r, 404, "ATTACHMENT_NOT_FOUND", "图片不可用")
	case errors.Is(err, ErrTooManyAttachments):
		httpapi.Fail(w, r, 422, "TOO_MANY_ATTACHMENTS", "关联的图片数量超出上限")
	case errors.Is(err, ErrEmailOverride):
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "邮件设置取值无效")
	case errors.Is(err, ErrTopicNotFound):
		httpapi.Fail(w, r, 404, "TOPIC_NOT_FOUND", "主题不存在或不可见")
	case errors.Is(err, ErrTopicLocked):
		httpapi.Fail(w, r, 409, "TOPIC_LOCKED", "主题已被锁定，无法新增内容")
	case errors.Is(err, ErrTopicSolved):
		httpapi.Fail(w, r, 409, "TOPIC_SOLVED", "问题已标记解决，暂不能新增回答或评论")
	case errors.Is(err, ErrNotAQuestion):
		httpapi.Fail(w, r, 409, "NOT_A_QUESTION", "该主题是讨论帖，不能回答")
	case errors.Is(err, ErrCommentTarget), errors.Is(err, ErrReplyTarget):
		httpapi.Fail(w, r, 422, "INVALID_COMMENT_TARGET", "评论或回复目标无效")
	case errors.Is(err, ErrNotTopicOwner):
		httpapi.Fail(w, r, 403, "NOT_TOPIC_OWNER", "只有提问者可以切换解决状态")
	case errors.Is(err, ErrResolutionStatus):
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "解决状态只能是 open 或 solved")
	case errors.Is(err, ErrContentNotFound):
		httpapi.Fail(w, r, 404, "CONTENT_NOT_FOUND", "内容不存在或不可见")
	case errors.Is(err, ErrNotContentOwner):
		httpapi.Fail(w, r, 403, "NOT_CONTENT_OWNER", "只能修改自己发布的内容")
	case errors.Is(err, ErrEditWindowClosed):
		httpapi.Fail(w, r, 409, "EDIT_WINDOW_CLOSED", "已超过 30 分钟编辑时限，只能删除后重发")
	case errors.Is(err, ErrLockedEdit):
		httpapi.Fail(w, r, 409, "TOPIC_LOCKED", "主题已被锁定，无法编辑正文")
	case errors.Is(err, ErrTitleOnNonTopic):
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "只有主题可以修改标题")
	default:
		return false
	}
	return true
}

func titleFailure(err error) (string, string) {
	switch {
	case errors.Is(err, markdown.ErrTitleRequired):
		return "TITLE_REQUIRED", "请填写标题"
	case errors.Is(err, markdown.ErrTitleTooLong):
		return "TITLE_TOO_LONG", "标题不能超过 " + strconv.Itoa(markdown.TitleMaxRunes) + " 个字符"
	case errors.Is(err, markdown.ErrTitleMultiline):
		return "TITLE_INVALID", "标题必须是单行，且不能包含控制字符"
	default:
		return "TITLE_INVALID", "标题无效"
	}
}

func bodyFailure(err error) (string, string) {
	switch {
	case errors.Is(err, markdown.ErrBodyRequired):
		return "BODY_REQUIRED", "请填写正文"
	case errors.Is(err, markdown.ErrBodyTooLong):
		return "BODY_TOO_LONG", "正文不能超过 " + strconv.Itoa(markdown.BodyMaxRunes) + " 个字符"
	case errors.Is(err, markdown.ErrRawHTML):
		return "BODY_RAW_HTML", "正文不接受原始HTML；如需展示代码请放进代码块"
	case errors.Is(err, markdown.ErrUnsafeLink):
		return "BODY_UNSAFE_LINK", "正文中的链接协议不被允许"
	default:
		return "BODY_INVALID", "正文无效"
	}
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// mediaURL is the controlled image address. The bytes are served by a separate media route, so the
// storage key never appears in a public payload directly.
func mediaURL(key string) string { return "/media/" + key }

type publicUserResponse struct {
	PublicID  string `json:"public_id"`
	Nickname  string `json:"nickname"`
	AvatarURL string `json:"avatar_url,omitempty"`
	CreatedAt string `json:"created_at"`
}

// authorResponse is the union of NamedAuthor and AnonymousAuthor. Only the fields of the active mode
// are emitted, so the anonymous branch can never carry a user object: both schemas forbid additional
// properties, and leaking one would be a privacy defect rather than a cosmetic one.
type authorResponse struct {
	Mode          string              `json:"mode"`
	User          *publicUserResponse `json:"user,omitempty"`
	DisplayName   string              `json:"display_name,omitempty"`
	DisplayNumber int                 `json:"display_number,omitempty"`
	IsTopicOwner  bool                `json:"is_topic_owner"`
}

type tagResponse struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

type attachmentResponse struct {
	ID       string `json:"id"`
	URL      string `json:"url"`
	MIMEType string `json:"mime_type"`
	ByteSize int64  `json:"byte_size"`
}

// topicResponse covers both DiscussionTopic and QuestionTopic. The two question-only fields are
// omitted for a discussion because both schemas set additionalProperties: false.
type topicResponse struct {
	ID               string               `json:"id"`
	State            string               `json:"state"`
	Kind             string               `json:"kind"`
	Author           authorResponse       `json:"author"`
	BodyMD           string               `json:"body_md"`
	Attachments      []attachmentResponse `json:"attachments"`
	CreatedAt        string               `json:"created_at"`
	EditedAt         string               `json:"edited_at,omitempty"`
	BoardID          string               `json:"board_id"`
	Title            string               `json:"title"`
	Tags             []tagResponse        `json:"tags"`
	IsLocked         bool                 `json:"is_locked"`
	IsPinned         bool                 `json:"is_pinned"`
	LastActivityAt   string               `json:"last_activity_at"`
	ResolutionStatus string               `json:"resolution_status,omitempty"`
	ResolvedAt       string               `json:"resolved_at,omitempty"`
}

// toTopicResponse assembles the public shape. The viewer is the topic author's own request here, so an
// anonymous author is labelled 楼主; a reply from another anonymous participant would be numbered
// instead.
func toTopicResponse(view TopicView) topicResponse {
	response := topicResponse{
		ID:             strconv.FormatUint(view.ContentID, 10),
		State:          "published",
		Kind:           "discussion",
		BodyMD:         view.BodyMD,
		Attachments:    make([]attachmentResponse, 0, len(view.Attachments)),
		CreatedAt:      formatTime(view.CreatedAt),
		BoardID:        strconv.FormatUint(view.BoardID, 10),
		Title:          view.Title,
		Tags:           make([]tagResponse, 0, len(view.Tags)),
		IsLocked:       view.IsLocked,
		IsPinned:       view.IsPinned,
		LastActivityAt: formatTime(view.LastActivityAt),
	}
	if view.BoardKind == "qa" {
		response.Kind = "question"
		response.ResolutionStatus = view.ResolutionState.String
		if view.ResolutionState.String == "" {
			response.ResolutionStatus = "open"
		}
		if view.ResolvedAt.Valid {
			response.ResolvedAt = formatTime(view.ResolvedAt.Time)
		}
	}
	if view.EditedAt.Valid {
		response.EditedAt = formatTime(view.EditedAt.Time)
	}
	// In this response the author is by definition the topic owner, so the marker is always shown.
	response.Author = renderAuthor(view.IsAnonymous, true, view.AnonymousNumber,
		view.AuthorPublicID, view.AuthorNickname, view.AuthorAvatarKey, view.AuthorCreatedAt)
	for _, tag := range view.Tags {
		response.Tags = append(response.Tags, tagResponse{
			ID:          strconv.FormatUint(tag.ID, 10),
			Name:        tag.Name,
			Description: tag.Description,
		})
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

func optionalMediaURL(key string) string {
	if key == "" {
		return ""
	}
	return mediaURL(key)
}
