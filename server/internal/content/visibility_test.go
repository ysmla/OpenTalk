package content

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// These cases cover the ancestor-chain rule on the paths that are actually reachable today: the
// authenticated edit, delete and comment-target endpoints, plus the internal single-content reads those
// endpoints answer with. The public list and detail reads are still 'planned' in the contract, so they
// are exercised through the store functions that back them.
//
// Every case hides an *intermediate* ancestor and keeps the root topic visible. That is the shape that
// used to slip through: the root looked fine, and only the node in between was moderated.

// hideContent writes the moderation state an administrator's "hide" action would set. The moderation
// interface is not implemented yet, so the legal target state is written directly, exactly as the
// earlier image-privacy probes did.
func (e *topicEnv) hideContent(t *testing.T, contentID string) {
	t.Helper()
	if _, err := e.db.Exec(`UPDATE contents SET moderation_status='hidden' WHERE id=?`, contentID); err != nil {
		t.Fatal(err)
	}
}

// questionWithHiddenAnswer builds the fixture the cases share: a visible question, an answer that is then
// hidden, one comment hanging from that answer and one comment with an image on the answer's thread.
type hiddenAnswerFixture struct {
	questionID string
	answerID   string
	commentID  string
	imageKey   string
}

func newHiddenAnswerFixture(t *testing.T, env *topicEnv, cookie, csrf string) hiddenAnswerFixture {
	t.Helper()
	question := env.do("POST", "/api/v1/topics",
		`{"board_id":"`+itoa(env.qaBoardID)+`","title":"中间祖先可见性","body_md":"正文。","is_anonymous":false,"tag_ids":[]}`,
		cookie, csrf)
	expectStatus(t, question, 201)
	var root struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(question.body, &root); err != nil {
		t.Fatal(err)
	}
	created := env.do("POST", "/api/v1/answers",
		`{"topic_id":"`+root.ID+`","body_md":"回答。","is_anonymous":false}`, cookie, csrf)
	expectStatus(t, created, 201)
	var answer struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(created.body, &answer); err != nil {
		t.Fatal(err)
	}
	imageID, key := env.uploadPNG(8, 8, cookie, csrf)
	comment := env.do("POST", "/api/v1/comments",
		`{"topic_id":"`+root.ID+`","answer_id":"`+answer.ID+`","body_md":"带图评论。","is_anonymous":false,"attachment_ids":["`+itoa(imageID)+`"]}`,
		cookie, csrf)
	expectStatus(t, comment, 201)
	var commentBody struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(comment.body, &commentBody); err != nil {
		t.Fatal(err)
	}
	return hiddenAnswerFixture{questionID: root.ID, answerID: answer.ID, commentID: commentBody.ID, imageKey: key}
}

func TestAncestorVisibilityClosesIntermediateAncestors(t *testing.T) {
	env := newTopicEnv(t)
	cookie, csrf := env.sessionCookie(env.authorID)
	ctx := context.Background()

	t.Run("隐藏回答后，作者不能编辑或删除该回答", func(t *testing.T) {
		fixture := newHiddenAnswerFixture(t, env, cookie, csrf)
		path := "/api/v1/contents/" + fixture.answerID

		// The answer is editable while its question is public, so the refusal below is caused by the
		// hidden answer and not by the edit window closing.
		before := env.do("PATCH", path, `{"body_md":"编辑前的正文。"}`, cookie, csrf)
		expectStatus(t, before, 200)

		env.hideContent(t, fixture.answerID)

		edit := env.do("PATCH", path, `{"body_md":"编辑后的正文。"}`, cookie, csrf)
		expectStatus(t, edit, 404)
		expectCode(t, edit, "CONTENT_NOT_FOUND")
		del := env.do("DELETE", path, "", cookie, csrf)
		expectStatus(t, del, 404)
		expectCode(t, del, "CONTENT_NOT_FOUND")

		// The body must be untouched: a 404 that still wrote would be worse than no check at all.
		var body string
		if err := env.db.QueryRow(`SELECT body_md FROM contents WHERE id=?`, fixture.answerID).Scan(&body); err != nil {
			t.Fatal(err)
		}
		if body != "编辑前的正文。" {
			t.Fatalf("a refused edit still changed the body: %q", body)
		}
		// Same for deletion: it must not have archived or cleared the row.
		var deleted *string
		if err := env.db.QueryRow(`SELECT deleted_at FROM contents WHERE id=?`, fixture.answerID).Scan(&deleted); err != nil {
			t.Fatal(err)
		}
		if deleted != nil {
			t.Fatalf("a refused delete still soft-deleted the answer: %v", *deleted)
		}
	})

	t.Run("隐藏回答后，不能再往它下面发评论或回复", func(t *testing.T) {
		fixture := newHiddenAnswerFixture(t, env, cookie, csrf)
		env.hideContent(t, fixture.answerID)

		comment := env.do("POST", "/api/v1/comments",
			`{"topic_id":"`+fixture.questionID+`","answer_id":"`+fixture.answerID+`","body_md":"新评论。","is_anonymous":false}`,
			cookie, csrf)
		expectStatus(t, comment, 422)
		expectCode(t, comment, "INVALID_COMMENT_TARGET")
	})

	t.Run("隐藏串根评论后，不能再回复它", func(t *testing.T) {
		question := env.do("POST", "/api/v1/topics",
			`{"board_id":"`+itoa(env.qaBoardID)+`","title":"隐藏串根","body_md":"正文。","is_anonymous":false,"tag_ids":[]}`,
			cookie, csrf)
		expectStatus(t, question, 201)
		var root struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(question.body, &root); err != nil {
			t.Fatal(err)
		}
		created := env.do("POST", "/api/v1/comments",
			`{"topic_id":"`+root.ID+`","body_md":"顶层评论。","is_anonymous":false}`, cookie, csrf)
		expectStatus(t, created, 201)
		var comment struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(created.body, &comment); err != nil {
			t.Fatal(err)
		}
		env.hideContent(t, comment.ID)

		// A reply must not be attachable to a thread a reader cannot reach.
		reply := env.do("POST", "/api/v1/comments",
			`{"topic_id":"`+root.ID+`","reply_to_comment_id":"`+comment.ID+`","body_md":"回复。","is_anonymous":false}`,
			cookie, csrf)
		expectStatus(t, reply, 422)
		expectCode(t, reply, "INVALID_COMMENT_TARGET")
	})

	t.Run("隐藏中间祖先后，单个内容的读取与图片一致地拒绝", func(t *testing.T) {
		fixture := newHiddenAnswerFixture(t, env, cookie, csrf)
		answerID := parseUint(t, fixture.answerID)
		commentID := parseUint(t, fixture.commentID)

		// Before hiding, the answer and the comment are both reachable, and so is the comment's image.
		if visible, err := env.store.contentVisible(ctx, commentID); err != nil || !visible {
			t.Fatalf("a comment under a visible answer must be reachable: %v %v", visible, err)
		}
		if got := env.fetchImage(fixture.imageKey); got.status != 200 {
			t.Fatalf("a reachable comment image must be served: %d", got.status)
		}

		env.hideContent(t, fixture.answerID)

		// The store reads and the image route have to agree: the same ancestor decides both, so a caller
		// cannot learn more from one than from the other.
		for name, id := range map[string]uint64{"回答": answerID, "评论": commentID} {
			if visible, err := env.store.contentVisible(ctx, id); err != nil || visible {
				t.Fatalf("%s must stop being reachable once its ancestor is hidden: %v %v", name, visible, err)
			}
			if _, err := env.store.PublicContentByID(ctx, id); !errors.Is(err, ErrContentNotFound) {
				t.Fatalf("%s must read as not found through the public reader: %v", name, err)
			}
		}
		if _, err := env.store.AnswerByContentID(ctx, answerID); !errors.Is(err, ErrContentNotFound) {
			t.Fatalf("a hidden answer must read as not found: %v", err)
		}
		if _, err := env.store.CommentByContentID(ctx, commentID); !errors.Is(err, ErrContentNotFound) {
			t.Fatalf("a comment under a hidden answer must read as not found: %v", err)
		}
		if got := env.fetchImage(fixture.imageKey); got.status != 404 {
			t.Fatalf("the image must agree with the reads and be refused: %d", got.status)
		}
	})

	t.Run("隐藏根主题的行为保持不变", func(t *testing.T) {
		// The shared rule must not narrow the older, broader guarantee: hiding the root still hides
		// everything under it, including content whose own row stays visible.
		fixture := newHiddenAnswerFixture(t, env, cookie, csrf)
		env.hideContent(t, fixture.questionID)

		if _, err := env.store.PublicContentByID(ctx, parseUint(t, fixture.answerID)); !errors.Is(err, ErrContentNotFound) {
			t.Fatalf("hiding the question must hide its answer: %v", err)
		}
		if _, err := env.store.PublicContentByID(ctx, parseUint(t, fixture.commentID)); !errors.Is(err, ErrContentNotFound) {
			t.Fatalf("hiding the question must hide its comment: %v", err)
		}
		if got := env.fetchImage(fixture.imageKey); got.status != 404 {
			t.Fatalf("hiding the question must hide its comment image: %d", got.status)
		}
	})

	// A reply under an answer carries two ancestors: the answer it hangs from and the thread root it is
	// displayed under. They are independent, so both have to be walked. A predicate that resolves "the"
	// ancestor — a COALESCE over answer_id and parent_comment_id — takes the answer and never looks at the
	// thread root, so hiding that root leaves the reply, and its image, reachable.
	t.Run("隐藏回答下评论的串根时，该串回复与图片一并失效而回答仍公开", func(t *testing.T) {
		fixture := newHiddenAnswerFixture(t, env, cookie, csrf)
		replyImageID, replyKey := env.uploadPNG(8, 8, cookie, csrf)
		reply := env.do("POST", "/api/v1/comments",
			`{"topic_id":"`+fixture.questionID+`","answer_id":"`+fixture.answerID+`","reply_to_comment_id":"`+fixture.commentID+`","body_md":"回答下的带图回复。","is_anonymous":false,"attachment_ids":["`+itoa(replyImageID)+`"]}`,
			cookie, csrf)
		expectStatus(t, reply, 201)
		var replyBody struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(reply.body, &replyBody); err != nil {
			t.Fatal(err)
		}
		replyID := parseUint(t, replyBody.ID)

		// Before moderation the reply is reachable through both of its ancestors.
		if visible, err := env.store.contentVisible(ctx, replyID); err != nil || !visible {
			t.Fatalf("a reply under a visible comment and answer must be reachable: %v %v", visible, err)
		}
		if got := env.fetchImage(replyKey); got.status != 200 {
			t.Fatalf("a reachable reply image must be served: %d", got.status)
		}

		env.hideContent(t, fixture.commentID)

		// The answer and the root question are still visible, so only the thread root can refuse these.
		if visible, err := env.store.contentVisible(ctx, parseUint(t, fixture.answerID)); err != nil || !visible {
			t.Fatalf("the answer itself must stay reachable: %v %v", visible, err)
		}
		if visible, err := env.store.contentVisible(ctx, replyID); err != nil || visible {
			t.Fatalf("a reply under a hidden thread root must not be reachable: %v %v", visible, err)
		}
		if _, err := env.store.CommentByContentID(ctx, replyID); !errors.Is(err, ErrContentNotFound) {
			t.Fatalf("a reply under a hidden thread root must read as not found: %v", err)
		}
		if got := env.fetchImage(replyKey); got.status != 404 {
			t.Fatalf("the reply image must agree with the reads and be refused: %d", got.status)
		}
	})
}
