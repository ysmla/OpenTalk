package content

import (
	"context"
	"errors"
	"net/http"

	"opentalk/server/internal/auth"
	"opentalk/server/internal/platform/httpapi"
)

// ErrNotTopicOwner is returned when somebody other than the asker tries to switch the resolution state.
var ErrNotTopicOwner = errors.New("actor is not the topic owner")

// resolutionStatuses is the closed set the column and the contract both accept.
var resolutionStatuses = map[string]bool{"open": true, "solved": true}

// SetResolutionInput switches a question between open and solved.
type SetResolutionInput struct {
	ActorID uint64
	TopicID uint64
	Status  string
}

// SetResolution switches a question's resolution state. Only the asker may do it, and administrators are
// not exempt: judging whether a question is answered is the asker's call, not a moderation action.
//
// The switch takes the same root lock as new answers and comments, and that is the whole point. The
// guard that refuses new interaction reads resolution_status, so "mark solved" and "add an answer" must
// serialise against each other. With no lock, a submission could read the old status while the solve
// read no conflict, and both would commit — leaving a new answer on a question that is already solved.
func (s Store) SetResolution(ctx context.Context, in SetResolutionInput) error {
	if !resolutionStatuses[in.Status] {
		return ErrResolutionStatus
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	root, err := lockRootTopic(ctx, tx, in.TopicID)
	if err != nil {
		return err
	}
	// Deliberately not guardNewInteraction: a locked or already-solved question must still be reopenable,
	// otherwise "solved" would be a one-way door and the requirement to reopen it could not be met. Only
	// visibility is required here.
	if root.Deleted || root.Hidden {
		return ErrTopicNotFound
	}
	if !root.IsQuestion {
		return ErrNotAQuestion
	}
	if root.AuthorID != in.ActorID {
		return ErrNotTopicOwner
	}
	if root.Resolution == in.Status {
		// Idempotent: asking for the state it already has is not a conflict, and it keeps the client
		// simple when a double click or a retry re-sends the same body.
		return tx.Commit()
	}
	// resolved_at must be present exactly when the state is solved; the table's CHECK enforces the pair,
	// so both columns are written together.
	if _, err := tx.ExecContext(ctx, `UPDATE questions SET resolution_status=?,
 resolved_at=IF(?='solved',UTC_TIMESTAMP(6),NULL) WHERE topic_id=?`,
		in.Status, in.Status, in.TopicID); err != nil {
		return err
	}
	return tx.Commit()
}

// ErrResolutionStatus rejects a value outside the column's enum before it reaches the database.
var ErrResolutionStatus = errors.New("invalid resolution status")

// setResolutionRequest mirrors SetResolutionRequest.
type setResolutionRequest struct {
	ResolutionStatus string `json:"resolution_status"`
}

// setResolution switches a question's resolution state and returns the updated topic.
func (d Deps) setResolution(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.FromContext(r.Context())
	if !ok {
		httpapi.Fail(w, r, 401, "UNAUTHENTICATED", "请先登录")
		return
	}
	topicID, err := ParseID(r.PathValue("topic_id"))
	if err != nil {
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "问题ID无效")
		return
	}
	var request setResolutionRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	if !resolutionStatuses[request.ResolutionStatus] {
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "解决状态只能是 open 或 solved")
		return
	}
	if err := d.Store.SetResolution(r.Context(), SetResolutionInput{
		ActorID: principal.UserID,
		TopicID: topicID,
		Status:  request.ResolutionStatus,
	}); err != nil {
		if d.failCreate(w, r, err) {
			return
		}
		return
	}
	// The response is the whole topic, so the client sees exactly what was stored, including the
	// resolved_at the database computed.
	view, err := d.Store.TopicByContentID(r.Context(), topicID)
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, toTopicResponse(view))
}
