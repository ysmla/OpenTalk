package content

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"opentalk/server/internal/auth"
	"opentalk/server/internal/platform/httpapi"
)

// The reader's own content list, and another reader's public profile with their public content.
//
// These two lists look similar and must behave differently, which is the whole point of this file:
//
//   - **my own list includes my anonymous posts.** I wrote them, I know they are mine, and hiding them from
//     me would make my own history unusable. They are still rendered in anonymous mode, so the list is not a
//     place where anonymity quietly stops applying.
//   - **a public profile excludes them.** The contract says so in one line ("必须排除匿名、隐藏、删除内容；
//     禁止跨身份关联"), and this is the rule that makes "可追溯匿名" meaningful: anonymity is traceable through
//     the audit process, never by browsing somebody's page. Excluding them is done in SQL (`is_anonymous =
//     FALSE`) rather than by filtering afterwards, so a mistake in the reader above cannot leak them.
//
// Both lists answer only for visible content, through the same ancestor rule everything else uses.

// publicProfileResponse is the PublicUser schema. It is a separate type from the author object embedded in
// content payloads because only this one carries the bio: adding a field to the shared author object would
// change every topic, answer and comment payload to say the same thing.
type publicProfileResponse struct {
	PublicID  string `json:"public_id"`
	Nickname  string `json:"nickname"`
	AvatarURL string `json:"avatar_url,omitempty"`
	Bio       string `json:"bio,omitempty"`
	CreatedAt string `json:"created_at"`
}

// PublicUserByPublicID reads one account's public profile.
//
// A malformed public id cannot match anything, so it is answered as missing rather than as a validation
// error: the distinction would only tell a caller which strings are shaped like ids. The response carries
// no email, no internal id and no account state — the contract's PublicUser has no place for them.
func (s Store) PublicUserByPublicID(ctx context.Context, publicID string) (publicProfileResponse, error) {
	var profile publicProfileResponse
	var avatarKey sql.NullString
	var createdAt time.Time
	err := s.DB.QueryRowContext(ctx, `SELECT public_id,nickname,avatar_attachment_id,bio,created_at
 FROM users WHERE public_id=?`, publicID).
		Scan(&profile.PublicID, &profile.Nickname, &avatarKey, &profile.Bio, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return publicProfileResponse{}, ErrContentNotFound
	}
	if err != nil {
		return publicProfileResponse{}, err
	}
	if avatarKey.Valid {
		profile.AvatarURL = mediaURL(avatarKey.String)
	}
	profile.CreatedAt = formatTime(createdAt)
	return profile, nil
}

// userContentIDs is the shared query behind both lists. `anonymous` selects whether the author's anonymous
// rows are included, and it is the only difference between "my content" and "their public content".
func (s Store) userContentIDs(ctx context.Context, userID uint64, includeAnonymous bool, page pageRequest) ([]uint64, bool, uint64, error) {
	conditions := []string{
		`c.author_id=?`,
		`c.deleted_at IS NULL`,
		// Hidden content is not listed for its author either: the contract points them at their
		// notifications for moderation outcomes instead of leaving a row they cannot open.
		`c.moderation_status='visible'`,
	}
	args := []any{userID}
	if !includeAnonymous {
		conditions = append(conditions, `c.is_anonymous=FALSE`)
	}
	if len(page.After) == 1 {
		conditions = append(conditions, `c.id<?`)
		args = append(args, page.After[0])
	}
	args = append(args, page.Limit+1)
	rows, err := s.DB.QueryContext(ctx, `SELECT c.id FROM contents c WHERE `+strings.Join(conditions, " AND ")+`
 ORDER BY c.id DESC LIMIT ?`, args...)
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
	last := uint64(0)
	if len(candidates) > 0 {
		last = candidates[len(candidates)-1]
	}
	if len(candidates) == 0 {
		return nil, hasMore, last, nil
	}
	// A row can be visible itself while its topic or an intermediate ancestor is hidden; the ancestor rule
	// decides, exactly as it does for every other read.
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
	return found, hasMore, last, nil
}

// ListUserContentsIDs returns one account's public content ids. Anonymous rows are excluded in SQL.
func (s Store) ListUserContentsIDs(ctx context.Context, userID uint64, page pageRequest) ([]uint64, bool, uint64, error) {
	return s.userContentIDs(ctx, userID, false, page)
}

// userIDByPublicID resolves a public id to the internal one, or reports it as missing.
func (s Store) userIDByPublicID(ctx context.Context, publicID string) (uint64, error) {
	var userID uint64
	err := s.DB.QueryRowContext(ctx, `SELECT id FROM users WHERE public_id=?`, publicID).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrContentNotFound
	}
	if err != nil {
		return 0, err
	}
	return userID, nil
}

// contentPageOf assembles one page of mixed content, dropping anything that became unreachable between the
// ranking query and the read. Shared by the lists added here so they cannot diverge in how they render or
// in what they refuse to show.
func (d Deps) contentPageOf(ctx context.Context, ids []uint64, hasMore bool, last uint64, scope string) (contentPageView, error) {
	view := contentPageView{Items: make([]any, 0, len(ids)), HasMore: hasMore}
	for _, id := range ids {
		item, err := d.Store.PublicContentByID(ctx, id)
		if errors.Is(err, ErrContentNotFound) || errors.Is(err, ErrTopicNotFound) {
			continue
		}
		if err != nil {
			return contentPageView{}, err
		}
		view.Items = append(view.Items, item)
	}
	if hasMore && last != 0 {
		view.NextCursor = encodeCursor(scope, int64(last))
	}
	return view, nil
}

// listMyContents serves GET /api/v1/me/contents: my own content, anonymous posts included.
func (d Deps) listMyContents(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.FromContext(r.Context())
	if !ok {
		httpapi.Fail(w, r, 401, "UNAUTHENTICATED", "请先登录")
		return
	}
	page, ok := d.parsePageOrFail(w, r, myContentCursorScope)
	if !ok {
		return
	}
	ids, hasMore, last, err := d.Store.userContentIDs(r.Context(), principal.UserID, true, page)
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	view, err := d.contentPageOf(r.Context(), ids, hasMore, last, myContentCursorScope)
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, view)
}

// getUserProfile serves GET /api/v1/users/{public_id}.
func (d Deps) getUserProfile(w http.ResponseWriter, r *http.Request) {
	publicID := strings.TrimSpace(r.PathValue("public_id"))
	profile, err := d.Store.PublicUserByPublicID(r.Context(), publicID)
	if errors.Is(err, ErrContentNotFound) {
		httpapi.Fail(w, r, 404, "USER_NOT_FOUND", "用户不存在")
		return
	}
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, profile)
}

// listUserContents serves GET /api/v1/users/{public_id}/contents: public content only.
func (d Deps) listUserContents(w http.ResponseWriter, r *http.Request) {
	publicID := strings.TrimSpace(r.PathValue("public_id"))
	userID, err := d.Store.userIDByPublicID(r.Context(), publicID)
	if errors.Is(err, ErrContentNotFound) {
		httpapi.Fail(w, r, 404, "USER_NOT_FOUND", "用户不存在")
		return
	}
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	page, ok := d.parsePageOrFail(w, r, userContentCursorScope)
	if !ok {
		return
	}
	ids, hasMore, last, err := d.Store.ListUserContentsIDs(r.Context(), userID, page)
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	view, err := d.contentPageOf(r.Context(), ids, hasMore, last, userContentCursorScope)
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, view)
}

// Cursor scopes: separate, so a cursor from one list cannot page another.
const (
	myContentCursorScope   = "my-contents"
	userContentCursorScope = "user-contents"
)
