package content

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// The read endpoints are exercised against real MySQL for the same reason the write endpoints are: the
// cursor is a SQL tuple comparison, the visibility filter is a walk over real rows, and the tombstone
// depends on rows the delete cascade actually wrote. None of that exists in a fake.

// topic is a discussion created by the fixture author, with no tags, so a list test can tell filtering
// apart from "everything came back".
func (e *topicEnv) topic(boardID uint64, title string) uint64 {
	e.t.Helper()
	in := e.base()
	in.BoardID = boardID
	in.Title = title
	in.TagIDs = nil
	id, err := e.store.CreateTopic(context.Background(), in)
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *topicEnv) get(path string) response {
	e.t.Helper()
	return e.do("GET", path, "", "", "")
}

func (e *topicEnv) getAs(path, cookie string) response {
	e.t.Helper()
	return e.do("GET", path, "", cookie, "")
}

// readPage is the shared page envelope. Items stay raw so one test can assert on the discriminator of a
// mixed list.
type readPage struct {
	Items      []json.RawMessage `json:"items"`
	HasMore    bool              `json:"has_more"`
	NextCursor string            `json:"next_cursor"`
}

func decodeReadPage(t *testing.T, res response) readPage {
	t.Helper()
	var page readPage
	if err := json.Unmarshal(res.body, &page); err != nil {
		t.Fatalf("cannot decode %s: %v", string(res.body), err)
	}
	return page
}

// idsOf reads the "id" of every item of a page, in order.
func idsOf(t *testing.T, page readPage) []string {
	t.Helper()
	ids := make([]string, 0, len(page.Items))
	for _, item := range page.Items {
		ids = append(ids, fieldOf(t, item, "id"))
	}
	return ids
}

// fieldOf reads one field of a raw object. An absent field is the empty string, which is what lets a test
// assert that a privacy-sensitive key was *not* sent.
func fieldOf(t *testing.T, raw json.RawMessage, name string) string {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatalf("cannot decode %s: %v", string(raw), err)
	}
	value, ok := object[name]
	if !ok {
		return ""
	}
	var text string
	if err := json.Unmarshal(value, &text); err != nil {
		return string(value)
	}
	return text
}

func hasField(t *testing.T, raw json.RawMessage, name string) bool {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatalf("cannot decode %s: %v", string(raw), err)
	}
	_, ok := object[name]
	return ok
}

// oneItem decodes a single-object response.
func oneItem(t *testing.T, res response) json.RawMessage {
	t.Helper()
	return json.RawMessage(res.body)
}

func TestReadTopicList(t *testing.T) {
	env := newTopicEnv(t)
	ctx := context.Background()

	discussion := env.topic(env.boardID, "普通帖子")
	tagged, err := env.store.CreateTopic(ctx, env.base())
	if err != nil {
		t.Fatal(err)
	}
	question := env.newQuestion(false)

	t.Run("列表返回公开主题", func(t *testing.T) {
		res := env.get("/api/v1/topics")
		expectStatus(t, res, 200)
		page := decodeReadPage(t, res)
		if page.HasMore {
			t.Fatalf("three topics must not report a next page: %+v", page)
		}
		seen := map[string]bool{}
		for _, item := range page.Items {
			seen[fieldOf(t, item, "id")] = true
		}
		for _, id := range []uint64{discussion, tagged, question} {
			if !seen[itoa(id)] {
				t.Fatalf("topic %d missing from the list: %s", id, string(res.body))
			}
		}
		// The question must carry its resolution status; the discussion must not be reported as a question.
		for _, item := range page.Items {
			kind := fieldOf(t, item, "kind")
			switch fieldOf(t, item, "id") {
			case itoa(question):
				if kind != "question" || fieldOf(t, item, "resolution_status") != "open" {
					t.Fatalf("question item is wrong: %s", string(item))
				}
			case itoa(discussion):
				if kind != "discussion" || hasField(t, item, "resolution_status") {
					t.Fatalf("discussion item is wrong: %s", string(item))
				}
			}
		}
	})

	t.Run("按板块筛选", func(t *testing.T) {
		res := env.get("/api/v1/topics?board_id=" + itoa(env.boardID))
		expectStatus(t, res, 200)
		for _, id := range idsOf(t, decodeReadPage(t, res)) {
			if id == itoa(question) {
				t.Fatalf("a question leaked into the discussion board filter")
			}
		}
		qa := env.get("/api/v1/topics?board_id=" + itoa(env.qaBoardID))
		expectStatus(t, qa, 200)
		ids := idsOf(t, decodeReadPage(t, qa))
		if len(ids) != 1 || ids[0] != itoa(question) {
			t.Fatalf("qa board filter returned %v", ids)
		}
		// A board that does not exist is reported as missing rather than as an empty board.
		expectStatus(t, env.get("/api/v1/topics?board_id=999999"), 404)
		expectCode(t, env.get("/api/v1/topics?board_id=999999"), "BOARD_NOT_FOUND")
		expectStatus(t, env.get("/api/v1/topics?board_id=abc"), 422)
		expectStatus(t, env.get("/api/v1/topics?board_id=0"), 422)
	})

	t.Run("按标签筛选", func(t *testing.T) {
		res := env.get("/api/v1/topics?tag_id=" + itoa(env.tagID))
		expectStatus(t, res, 200)
		ids := idsOf(t, decodeReadPage(t, res))
		if len(ids) != 1 || ids[0] != itoa(tagged) {
			t.Fatalf("tag filter returned %v, want only %d", ids, tagged)
		}
		expectStatus(t, env.get("/api/v1/topics?tag_id=999999"), 404)
		expectCode(t, env.get("/api/v1/topics?tag_id=999999"), "TAG_NOT_FOUND")
		expectStatus(t, env.get("/api/v1/topics?tag_id=abc"), 422)
	})

	t.Run("按解决状态筛选", func(t *testing.T) {
		// A discussion has no questions row, so it can never match a resolution filter.
		res := env.get("/api/v1/topics?resolution_status=open")
		expectStatus(t, res, 200)
		ids := idsOf(t, decodeReadPage(t, res))
		if len(ids) != 1 || ids[0] != itoa(question) {
			t.Fatalf("open filter returned %v", ids)
		}
		empty := env.get("/api/v1/topics?resolution_status=solved")
		expectStatus(t, empty, 200)
		if items := idsOf(t, decodeReadPage(t, empty)); len(items) != 0 {
			t.Fatalf("solved filter returned %v", items)
		}
		env.setSolved(question)
		solved := env.get("/api/v1/topics?resolution_status=solved")
		expectStatus(t, solved, 200)
		if items := idsOf(t, decodeReadPage(t, solved)); len(items) != 1 || items[0] != itoa(question) {
			t.Fatalf("solved filter returned %v", items)
		}
		env.reopen(question)
		expectStatus(t, env.get("/api/v1/topics?resolution_status=bogus"), 422)
	})

	t.Run("隐藏与删除的主题不出现在列表", func(t *testing.T) {
		hidden := env.topic(env.boardID, "将被隐藏")
		removed := env.topic(env.boardID, "将被删除")
		env.hideContent(t, itoa(hidden))
		if _, err := env.store.DeleteContent(ctx, DeleteContentInput{ActorID: env.authorID, ContentID: removed}); err != nil {
			t.Fatal(err)
		}
		ids := idsOf(t, decodeReadPage(t, env.get("/api/v1/topics?limit=100")))
		for _, id := range ids {
			if id == itoa(hidden) || id == itoa(removed) {
				t.Fatalf("removed content is still listed: %v", ids)
			}
		}
	})

	t.Run("非法排序与游标被拒", func(t *testing.T) {
		expectStatus(t, env.get("/api/v1/topics?sort=bogus"), 422)
		expectStatus(t, env.get("/api/v1/topics?cursor=not-a-cursor"), 400)
		expectCode(t, env.get("/api/v1/topics?cursor=not-a-cursor"), "CURSOR_INVALID")
		expectStatus(t, env.get("/api/v1/topics?limit=0"), 422)
		expectStatus(t, env.get("/api/v1/topics?limit=101"), 422)
		// A cursor issued by another list must not be replayed here: the scope is part of the payload. The
		// board list is the other list that hands out cursors.
		expectStatus(t, env.get("/api/v1/topics?cursor="+encodeCursor(boardCursorScope, 10, 1)), 400)
	})

	t.Run("游标分页不重不漏", func(t *testing.T) {
		var collected []string
		cursor := ""
		for pages := 0; ; pages++ {
			if pages > 10 {
				t.Fatal("pagination did not terminate")
			}
			path := "/api/v1/topics?limit=5"
			if cursor != "" {
				path += "&cursor=" + cursor
			}
			res := env.get(path)
			expectStatus(t, res, 200)
			page := decodeReadPage(t, res)
			collected = append(collected, idsOf(t, page)...)
			if !page.HasMore {
				break
			}
			if page.NextCursor == "" {
				t.Fatal("has_more without a cursor cannot be followed")
			}
			cursor = page.NextCursor
		}
		seen := map[string]bool{}
		for _, id := range collected {
			if seen[id] {
				t.Fatalf("id %s appeared twice across pages: %v", id, collected)
			}
			seen[id] = true
		}
		all := idsOf(t, decodeReadPage(t, env.get("/api/v1/topics?limit=100")))
		if len(collected) != len(all) {
			t.Fatalf("paged read returned %d rows, single page returned %d", len(collected), len(all))
		}
		for _, id := range all {
			if !seen[id] {
				t.Fatalf("id %s was skipped by pagination", id)
			}
		}
	})

	t.Run("置顶主题排在前面且可分页", func(t *testing.T) {
		pinned := env.topic(env.boardID, "置顶帖")
		env.setTopicFlag(pinned, "is_pinned", true)
		for _, sort := range []string{"newest", "active"} {
			res := env.get("/api/v1/topics?sort=" + sort + "&limit=1")
			expectStatus(t, res, 200)
			page := decodeReadPage(t, res)
			if len(page.Items) != 1 || fieldOf(t, page.Items[0], "id") != itoa(pinned) {
				t.Fatalf("%s sort did not put the pinned topic first: %s", sort, string(res.body))
			}
			// The cursor has to carry the pinned flag too, or the page after it would repeat or skip.
			next := env.get("/api/v1/topics?sort=" + sort + "&limit=1&cursor=" + page.NextCursor)
			expectStatus(t, next, 200)
			for _, id := range idsOf(t, decodeReadPage(t, next)) {
				if id == itoa(pinned) {
					t.Fatalf("%s sort repeated the pinned topic on page two", sort)
				}
			}
		}
		env.setTopicFlag(pinned, "is_pinned", false)
	})

	t.Run("活动时间排序跟随最后互动", func(t *testing.T) {
		older := env.topic(env.boardID, "较早的帖子")
		newer := env.topic(env.boardID, "较晚的帖子")
		// The ordering key is second-granular, so the difference is made explicit rather than raced: the
		// older topic is pushed a day back and then touched, which must move it to the front of "active".
		if _, err := env.db.Exec(`UPDATE topics SET last_activity_at=UTC_TIMESTAMP(6)-INTERVAL 1 DAY WHERE content_id IN (?,?)`,
			older, newer); err != nil {
			t.Fatal(err)
		}
		if _, err := env.store.CreateComment(ctx, NewComment{
			AuthorID: env.authorID, TopicID: older, BodyMD: "顶一下。", MaxAttachments: 9,
		}); err != nil {
			t.Fatal(err)
		}
		active := idsOf(t, decodeReadPage(t, env.get("/api/v1/topics?sort=active&limit=100")))
		position := func(id uint64) int {
			for index, seen := range active {
				if seen == itoa(id) {
					return index
				}
			}
			t.Fatalf("topic %d missing from the active list", id)
			return -1
		}
		if position(older) > position(newer) {
			t.Fatalf("active sort ignored the new comment: %v", active)
		}
		env.deleteContent(older)
		env.deleteContent(newer)
	})
}

func TestReadContentByID(t *testing.T) {
	env := newTopicEnv(t)
	ctx := context.Background()

	question := env.newQuestion(false)
	answer, err := env.answer(question, env.newUser("answerer"), false)
	if err != nil {
		t.Fatal(err)
	}
	topicComment, err := env.comment(NewComment{AuthorID: env.newUser("commenter"), TopicID: question})
	if err != nil {
		t.Fatal(err)
	}
	reply, err := env.comment(NewComment{AuthorID: env.newUser("replier"), TopicID: question, ReplyToCommentID: topicComment})
	if err != nil {
		t.Fatal(err)
	}
	answerComment, err := env.comment(NewComment{AuthorID: env.newUser("answercommenter"), TopicID: question, AnswerID: answer})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("按类型返回内容", func(t *testing.T) {
		topic := oneItem(t, env.get("/api/v1/contents/"+itoa(question)))
		if fieldOf(t, topic, "kind") != "question" || fieldOf(t, topic, "state") != "published" {
			t.Fatalf("question read is wrong: %s", string(topic))
		}
		if fieldOf(t, topic, "resolution_status") != "open" {
			t.Fatalf("question must report its resolution status: %s", string(topic))
		}
		answerBody := oneItem(t, env.get("/api/v1/contents/"+itoa(answer)))
		if fieldOf(t, answerBody, "kind") != "answer" || fieldOf(t, answerBody, "topic_id") != itoa(question) {
			t.Fatalf("answer read is wrong: %s", string(answerBody))
		}
		commentBody := oneItem(t, env.get("/api/v1/contents/"+itoa(topicComment)))
		if fieldOf(t, commentBody, "kind") != "comment" || fieldOf(t, commentBody, "topic_id") != itoa(question) {
			t.Fatalf("comment read is wrong: %s", string(commentBody))
		}
		if hasField(t, commentBody, "answer_id") || hasField(t, commentBody, "parent_comment_id") {
			t.Fatalf("a top-level comment must omit the thread pointers: %s", string(commentBody))
		}
		replyBody := oneItem(t, env.get("/api/v1/contents/"+itoa(reply)))
		if fieldOf(t, replyBody, "parent_comment_id") != itoa(topicComment) ||
			fieldOf(t, replyBody, "reply_to_comment_id") != itoa(topicComment) {
			t.Fatalf("reply read is wrong: %s", string(replyBody))
		}
		answerCommentBody := oneItem(t, env.get("/api/v1/contents/"+itoa(answerComment)))
		if fieldOf(t, answerCommentBody, "answer_id") != itoa(answer) {
			t.Fatalf("answer comment must name its answer: %s", string(answerCommentBody))
		}
		if fieldOf(t, answerCommentBody, "topic_id") != itoa(question) {
			t.Fatalf("answer comment must name its topic: %s", string(answerCommentBody))
		}
	})

	t.Run("不存在的ID与非法ID", func(t *testing.T) {
		expectStatus(t, env.get("/api/v1/contents/99999999"), 404)
		expectCode(t, env.get("/api/v1/contents/99999999"), "CONTENT_NOT_FOUND")
		expectStatus(t, env.get("/api/v1/contents/abc"), 422)
		expectStatus(t, env.get("/api/v1/contents/0"), 422)
	})

	t.Run("隐藏内容统一404", func(t *testing.T) {
		hiddenTopic := env.topic(env.boardID, "隐藏的帖子")
		hiddenAnswerQuestion := env.newQuestion(false)
		hiddenAnswer, err := env.answer(hiddenAnswerQuestion, env.authorID, false)
		if err != nil {
			t.Fatal(err)
		}
		env.hideContent(t, itoa(hiddenTopic))
		env.hideContent(t, itoa(hiddenAnswer))
		expectStatus(t, env.get("/api/v1/contents/"+itoa(hiddenTopic)), 404)
		// Hiding an answer must hide it from the single-content reader even though its own row is visible.
		expectStatus(t, env.get("/api/v1/contents/"+itoa(hiddenAnswer)), 404)
		expectStatus(t, env.get("/api/v1/contents/"+itoa(hiddenAnswerQuestion)), 200)
	})

	t.Run("匿名作者的读出不携带身份", func(t *testing.T) {
		// The nickname must not collide with a JSON field name, or the substring check below would pass for
		// the wrong reason: "author" appears in every payload as the name of the author object.
		secretive := env.newUser("秘密发问者")
		in := env.base()
		in.AuthorID = secretive
		in.BoardID = env.qaBoardID
		in.IsAnonymous = true
		in.TagIDs = nil
		anonymous, err := env.store.CreateTopic(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		raw := oneItem(t, env.get("/api/v1/contents/"+itoa(anonymous)))
		var body struct {
			Author struct {
				Mode        string          `json:"mode"`
				DisplayName string          `json:"display_name"`
				User        json.RawMessage `json:"user"`
			} `json:"author"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		if body.Author.Mode != "anonymous" || body.Author.DisplayName != "楼主" {
			t.Fatalf("anonymous author rendered as %+v", body.Author)
		}
		if len(body.Author.User) != 0 {
			t.Fatalf("anonymous author leaked a user object: %s", string(raw))
		}
		// Neither the nickname nor the public id of the real account may appear anywhere in the payload.
		if name := env.nicknameOf(secretive); strings.Contains(string(raw), name) {
			t.Fatalf("anonymous read contains the account nickname %q: %s", name, string(raw))
		}
		if publicID := env.publicIDOf(secretive); publicID != "" && strings.Contains(string(raw), publicID) {
			t.Fatalf("anonymous read contains the account public id: %s", string(raw))
		}
	})

	t.Run("删除内容返回不含原文的占位", func(t *testing.T) {
		comment := env.deletableComment(question)
		env.deleteContent(comment)
		res := env.get("/api/v1/contents/" + itoa(comment))
		expectStatus(t, res, 200)
		raw := oneItem(t, res)
		if fieldOf(t, raw, "state") != "deleted" || fieldOf(t, raw, "kind") != "comment" {
			t.Fatalf("tombstone is wrong: %s", string(raw))
		}
		if fieldOf(t, raw, "message") != "该评论已被删除" {
			t.Fatalf("tombstone message is wrong: %s", string(raw))
		}
		if fieldOf(t, raw, "topic_id") != itoa(question) {
			t.Fatalf("tombstone must keep the locating relationship: %s", string(raw))
		}
		// The contract forbids returning the original text, the author or the images.
		for _, forbidden := range []string{"body_md", "author", "attachments", "title"} {
			if hasField(t, raw, forbidden) {
				t.Fatalf("tombstone carries %q: %s", forbidden, string(raw))
			}
		}
		if fieldOf(t, raw, "deleted_at") == "" {
			t.Fatalf("tombstone must date the deletion: %s", string(raw))
		}
	})

	t.Run("删除的问题返回问题占位", func(t *testing.T) {
		removed := env.newQuestion(false)
		env.deleteContent(removed)
		raw := oneItem(t, env.get("/api/v1/contents/"+itoa(removed)))
		if fieldOf(t, raw, "kind") != "question" || fieldOf(t, raw, "message") != "该问题已被删除" {
			t.Fatalf("question tombstone is wrong: %s", string(raw))
		}
		if hasField(t, raw, "topic_id") {
			t.Fatalf("a deleted topic must omit topic_id: %s", string(raw))
		}
	})

	t.Run("级联删除后子内容不返回占位", func(t *testing.T) {
		// Deleting a question also deletes its answers. The answer's chain is gone, so asking for it must
		// not confirm that it existed — the tombstone is for a row whose thread is still readable.
		removed := env.newQuestion(false)
		removedAnswer, err := env.answer(removed, env.authorID, false)
		if err != nil {
			t.Fatal(err)
		}
		env.deleteContent(removed)
		expectStatus(t, env.get("/api/v1/contents/"+itoa(removedAnswer)), 404)
		expectCode(t, env.get("/api/v1/contents/"+itoa(removedAnswer)), "CONTENT_NOT_FOUND")
	})
}

// deletableComment posts a comment the fixture author owns, so a test can delete it without needing a
// session.
func (e *topicEnv) deletableComment(topicID uint64) uint64 {
	e.t.Helper()
	id, err := e.comment(NewComment{AuthorID: e.authorID, TopicID: topicID, BodyMD: "将被删除的评论。"})
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *topicEnv) deleteContent(contentID uint64) {
	e.t.Helper()
	if _, err := e.store.DeleteContent(context.Background(), DeleteContentInput{
		ActorID: e.authorOfContent(contentID), ContentID: contentID,
	}); err != nil {
		e.t.Fatal(err)
	}
}

// authorOfContent reads the stored author, which is what makes a delete by the fixture possible without a
// session for every participant.
func (e *topicEnv) authorOfContent(contentID uint64) uint64 {
	e.t.Helper()
	var author uint64
	if err := e.db.QueryRow(`SELECT author_id FROM contents WHERE id=?`, contentID).Scan(&author); err != nil {
		e.t.Fatal(err)
	}
	return author
}

func (e *topicEnv) nicknameOf(userID uint64) string {
	e.t.Helper()
	var nickname string
	if err := e.db.QueryRow(`SELECT nickname FROM users WHERE id=?`, userID).Scan(&nickname); err != nil {
		e.t.Fatal(err)
	}
	return nickname
}

func (e *topicEnv) publicIDOf(userID uint64) string {
	e.t.Helper()
	var publicID string
	if err := e.db.QueryRow(`SELECT public_id FROM users WHERE id=?`, userID).Scan(&publicID); err != nil {
		e.t.Fatal(err)
	}
	return publicID
}

func TestReadContentPermissions(t *testing.T) {
	env := newTopicEnv(t)
	other := env.newUser("other")

	question := env.newQuestion(false)
	discussion := env.topic(env.boardID, "普通帖子")

	t.Run("游客没有任何操作权限", func(t *testing.T) {
		res := env.get("/api/v1/contents/" + itoa(question) + "/permissions")
		expectStatus(t, res, 200)
		var permissions ContentPermissions
		if err := json.Unmarshal(res.body, &permissions); err != nil {
			t.Fatal(err)
		}
		if permissions != (ContentPermissions{}) {
			t.Fatalf("a guest was granted %+v", permissions)
		}
	})

	t.Run("作者可编辑可删除并可切换解决状态", func(t *testing.T) {
		cookie, _ := env.sessionCookie(env.authorID)
		res := env.getAs("/api/v1/contents/"+itoa(question)+"/permissions", cookie)
		expectStatus(t, res, 200)
		var permissions ContentPermissions
		if err := json.Unmarshal(res.body, &permissions); err != nil {
			t.Fatal(err)
		}
		if !permissions.CanEdit || !permissions.CanDelete || !permissions.CanSetResolution {
			t.Fatalf("the asker was not granted their own actions: %+v", permissions)
		}
		// Nothing forbids the asker from answering their own open question; only an *anonymous* asker is
		// forced anonymous while doing it.
		if !permissions.CanCreateAnswer || !permissions.CanCreateComment {
			t.Fatalf("the asker must be able to answer and comment on an open question: %+v", permissions)
		}
		if permissions.ForceAnonymousComment {
			t.Fatalf("a named asker is not forced anonymous: %+v", permissions)
		}
	})

	t.Run("他人不能编辑或删除但可以互动", func(t *testing.T) {
		cookie, _ := env.sessionCookie(other)
		var permissions ContentPermissions
		if err := json.Unmarshal(env.getAs("/api/v1/contents/"+itoa(question)+"/permissions", cookie).body, &permissions); err != nil {
			t.Fatal(err)
		}
		if permissions.CanEdit || permissions.CanDelete || permissions.CanSetResolution {
			t.Fatalf("another account was granted owner actions: %+v", permissions)
		}
		if !permissions.CanCreateAnswer || !permissions.CanCreateComment {
			t.Fatalf("an open question must accept an answer and a comment: %+v", permissions)
		}
	})

	t.Run("讨论帖不能新增回答", func(t *testing.T) {
		cookie, _ := env.sessionCookie(other)
		var permissions ContentPermissions
		if err := json.Unmarshal(env.getAs("/api/v1/contents/"+itoa(discussion)+"/permissions", cookie).body, &permissions); err != nil {
			t.Fatal(err)
		}
		if permissions.CanCreateAnswer {
			t.Fatalf("a discussion accepted an answer: %+v", permissions)
		}
		if !permissions.CanCreateComment {
			t.Fatalf("a discussion must accept a comment: %+v", permissions)
		}
	})

	t.Run("已解决与锁定主题禁止新增互动", func(t *testing.T) {
		cookie, _ := env.sessionCookie(other)
		solved := env.newQuestion(false)
		env.setSolved(solved)
		var permissions ContentPermissions
		if err := json.Unmarshal(env.getAs("/api/v1/contents/"+itoa(solved)+"/permissions", cookie).body, &permissions); err != nil {
			t.Fatal(err)
		}
		if permissions.CanCreateAnswer || permissions.CanCreateComment {
			t.Fatalf("a solved question accepted interaction: %+v", permissions)
		}
		locked := env.newQuestion(false)
		env.setTopicFlag(locked, "is_locked", true)
		if err := json.Unmarshal(env.getAs("/api/v1/contents/"+itoa(locked)+"/permissions", cookie).body, &permissions); err != nil {
			t.Fatal(err)
		}
		if permissions.CanCreateAnswer || permissions.CanCreateComment {
			t.Fatalf("a locked topic accepted interaction: %+v", permissions)
		}
	})

	t.Run("管理员没有捷径", func(t *testing.T) {
		admin := env.newUser("admin")
		if _, err := env.db.Exec(`UPDATE users SET role='admin' WHERE id=?`, admin); err != nil {
			t.Fatal(err)
		}
		cookie, _ := env.sessionCookie(admin)
		var permissions ContentPermissions
		if err := json.Unmarshal(env.getAs("/api/v1/contents/"+itoa(question)+"/permissions", cookie).body, &permissions); err != nil {
			t.Fatal(err)
		}
		if permissions.CanEdit || permissions.CanDelete || permissions.CanSetResolution {
			t.Fatalf("an administrator was granted a shortcut: %+v", permissions)
		}
	})

	t.Run("编辑时限不滑动", func(t *testing.T) {
		cookie, _ := env.sessionCookie(env.authorID)
		fresh := env.topic(env.boardID, "刚发布")
		var permissions ContentPermissions
		if err := json.Unmarshal(env.getAs("/api/v1/contents/"+itoa(fresh)+"/permissions", cookie).body, &permissions); err != nil {
			t.Fatal(err)
		}
		if !permissions.CanEdit {
			t.Fatalf("a fresh topic must be editable: %+v", permissions)
		}
		if _, err := env.db.Exec(`UPDATE contents SET created_at=UTC_TIMESTAMP(6)-INTERVAL 31 MINUTE WHERE id=?`, fresh); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(env.getAs("/api/v1/contents/"+itoa(fresh)+"/permissions", cookie).body, &permissions); err != nil {
			t.Fatal(err)
		}
		if permissions.CanEdit {
			t.Fatalf("the edit window did not close: %+v", permissions)
		}
		// The window closing must not take the delete right with it.
		if !permissions.CanDelete {
			t.Fatalf("delete must survive the edit window: %+v", permissions)
		}
	})

	t.Run("禁言取消发言但保留删除", func(t *testing.T) {
		muted := env.newUser("muted")
		if _, err := env.db.Exec(`INSERT INTO user_restrictions (user_id,kind,reason,starts_at,created_by)
 VALUES (?,'mute','测试禁言',UTC_TIMESTAMP(6),?)`, muted, env.authorID); err != nil {
			t.Fatal(err)
		}
		own := env.topic(env.boardID, "禁言者自己的帖子")
		if _, err := env.db.Exec(`UPDATE contents SET author_id=? WHERE id=?`, muted, own); err != nil {
			t.Fatal(err)
		}
		cookie, _ := env.sessionCookie(muted)
		var permissions ContentPermissions
		if err := json.Unmarshal(env.getAs("/api/v1/contents/"+itoa(own)+"/permissions", cookie).body, &permissions); err != nil {
			t.Fatal(err)
		}
		if permissions.CanEdit || permissions.CanCreateComment || permissions.CanCreateAnswer {
			t.Fatalf("a muted account still had speech: %+v", permissions)
		}
		if !permissions.CanDelete {
			t.Fatalf("muting must not remove the right to delete your own content: %+v", permissions)
		}
	})

	t.Run("匿名楼主被强制匿名", func(t *testing.T) {
		anonymous := env.newQuestion(true)
		cookie, _ := env.sessionCookie(env.authorID)
		var permissions ContentPermissions
		if err := json.Unmarshal(env.getAs("/api/v1/contents/"+itoa(anonymous)+"/permissions", cookie).body, &permissions); err != nil {
			t.Fatal(err)
		}
		if !permissions.ForceAnonymousComment {
			t.Fatalf("the anonymous asker was not forced anonymous: %+v", permissions)
		}
		// Another participant is not the owner, so they are not forced into the owner's identity.
		otherCookie, _ := env.sessionCookie(other)
		if err := json.Unmarshal(env.getAs("/api/v1/contents/"+itoa(anonymous)+"/permissions", otherCookie).body, &permissions); err != nil {
			t.Fatal(err)
		}
		if permissions.ForceAnonymousComment {
			t.Fatalf("a non-owner was reported as the anonymous owner: %+v", permissions)
		}
	})

	t.Run("隐藏内容没有权限可报告", func(t *testing.T) {
		hidden := env.topic(env.boardID, "隐藏的帖子")
		env.hideContent(t, itoa(hidden))
		cookie, _ := env.sessionCookie(env.authorID)
		expectStatus(t, env.getAs("/api/v1/contents/"+itoa(hidden)+"/permissions", cookie), 404)
	})
}

func TestReadAnswerAndCommentLists(t *testing.T) {
	env := newTopicEnv(t)

	question := env.newQuestion(false)
	first, err := env.answer(question, env.newUser("first"), false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := env.answer(question, env.newUser("second"), false)
	if err != nil {
		t.Fatal(err)
	}
	hidden, err := env.answer(question, env.newUser("hidden"), false)
	if err != nil {
		t.Fatal(err)
	}
	env.hideContent(t, itoa(hidden))

	t.Run("回答列表按时间顺序且过滤隐藏", func(t *testing.T) {
		res := env.get("/api/v1/questions/" + itoa(question) + "/answers")
		expectStatus(t, res, 200)
		ids := idsOf(t, decodeReadPage(t, res))
		if len(ids) != 2 || ids[0] != itoa(first) || ids[1] != itoa(second) {
			t.Fatalf("answer list is %v, want [%d %d]", ids, first, second)
		}
	})

	t.Run("回答列表分页", func(t *testing.T) {
		page := decodeReadPage(t, env.get("/api/v1/questions/"+itoa(question)+"/answers?limit=1"))
		if !page.HasMore || page.NextCursor == "" || len(page.Items) != 1 {
			t.Fatalf("first answer page is wrong: %+v", page)
		}
		if fieldOf(t, page.Items[0], "id") != itoa(first) {
			t.Fatalf("first answer page returned %s", string(page.Items[0]))
		}
		next := decodeReadPage(t, env.get("/api/v1/questions/"+itoa(question)+"/answers?limit=1&cursor="+page.NextCursor))
		if len(next.Items) != 1 || fieldOf(t, next.Items[0], "id") != itoa(second) {
			t.Fatalf("second answer page is %s", string(next.Items[0]))
		}
		if next.HasMore {
			t.Fatalf("a two-answer question reported more pages: %+v", next)
		}
	})

	t.Run("回答列表的边界", func(t *testing.T) {
		// A discussion has no answers to list, and accepting the request would suggest otherwise.
		discussion := env.topic(env.boardID, "普通帖子")
		expectStatus(t, env.get("/api/v1/questions/"+itoa(discussion)+"/answers"), 409)
		expectCode(t, env.get("/api/v1/questions/"+itoa(discussion)+"/answers"), "NOT_A_QUESTION")
		expectStatus(t, env.get("/api/v1/questions/99999999/answers"), 404)
		expectStatus(t, env.get("/api/v1/questions/abc/answers"), 422)
		hiddenQuestion := env.newQuestion(false)
		env.hideContent(t, itoa(hiddenQuestion))
		expectStatus(t, env.get("/api/v1/questions/"+itoa(hiddenQuestion)+"/answers"), 404)
	})

	answer, err := env.answer(question, env.newUser("commented"), false)
	if err != nil {
		t.Fatal(err)
	}
	top, err := env.comment(NewComment{AuthorID: env.newUser("top"), TopicID: question, BodyMD: "顶层评论。"})
	if err != nil {
		t.Fatal(err)
	}
	secondTop, err := env.comment(NewComment{AuthorID: env.newUser("secondtop"), TopicID: question, BodyMD: "第二条顶层评论。"})
	if err != nil {
		t.Fatal(err)
	}
	reply, err := env.comment(NewComment{AuthorID: env.newUser("reply"), TopicID: question, ReplyToCommentID: top})
	if err != nil {
		t.Fatal(err)
	}
	nested, err := env.comment(NewComment{AuthorID: env.newUser("nested"), TopicID: question, ReplyToCommentID: reply})
	if err != nil {
		t.Fatal(err)
	}
	answerScoped, err := env.comment(NewComment{AuthorID: env.newUser("answercomment"), TopicID: question, AnswerID: answer})
	if err != nil {
		t.Fatal(err)
	}
	hiddenTop, err := env.comment(NewComment{AuthorID: env.newUser("hiddentop"), TopicID: question})
	if err != nil {
		t.Fatal(err)
	}
	env.hideContent(t, itoa(hiddenTop))

	t.Run("主题评论只返回顶层", func(t *testing.T) {
		res := env.get("/api/v1/topics/" + itoa(question) + "/comments")
		expectStatus(t, res, 200)
		ids := idsOf(t, decodeReadPage(t, res))
		// Two visible top-level comments, in publication order; the hidden one and every reply stay out.
		if len(ids) != 2 || ids[0] != itoa(top) || ids[1] != itoa(secondTop) {
			t.Fatalf("topic comments are %v, want [%d %d]", ids, top, secondTop)
		}
		// A comment scoped to an answer must not appear in the topic-level list.
		for _, id := range ids {
			if id == itoa(answerScoped) {
				t.Fatalf("an answer comment leaked into the topic-level list")
			}
		}
	})

	t.Run("回答评论只返回该回答下的顶层", func(t *testing.T) {
		res := env.get("/api/v1/topics/" + itoa(question) + "/comments?answer_id=" + itoa(answer))
		expectStatus(t, res, 200)
		ids := idsOf(t, decodeReadPage(t, res))
		if len(ids) != 1 || ids[0] != itoa(answerScoped) {
			t.Fatalf("answer comments are %v, want [%d]", ids, answerScoped)
		}
	})

	t.Run("串根评论返回其全部回复", func(t *testing.T) {
		res := env.get("/api/v1/topics/" + itoa(question) + "/comments?parent_comment_id=" + itoa(top))
		expectStatus(t, res, 200)
		ids := idsOf(t, decodeReadPage(t, res))
		if len(ids) != 2 || ids[0] != itoa(reply) || ids[1] != itoa(nested) {
			t.Fatalf("thread replies are %v, want [%d %d]", ids, reply, nested)
		}
		// The reply must name both its display root and the row it actually answers, or the interface
		// cannot say "回复 @某人" while keeping the two-level shape.
		raw := decodeReadPage(t, res).Items[0]
		if fieldOf(t, raw, "parent_comment_id") != itoa(top) || fieldOf(t, raw, "reply_to_comment_id") != itoa(top) {
			t.Fatalf("first reply is wrong: %s", string(raw))
		}
		var nestedBody string
		for _, item := range decodeReadPage(t, res).Items {
			if fieldOf(t, item, "id") == itoa(nested) {
				nestedBody = string(item)
			}
		}
		if !strings.Contains(nestedBody, `"reply_to_comment_id":"`+itoa(reply)+`"`) {
			t.Fatalf("nested reply must point at the reply it answers: %s", nestedBody)
		}
	})

	t.Run("无效的评论目标被拒", func(t *testing.T) {
		// A hidden thread root must not become a way to read its replies.
		expectStatus(t, env.get("/api/v1/topics/"+itoa(question)+"/comments?parent_comment_id="+itoa(hiddenTop)), 422)
		expectCode(t, env.get("/api/v1/topics/"+itoa(question)+"/comments?parent_comment_id="+itoa(hiddenTop)), "INVALID_COMMENT_TARGET")
		// A reply is not itself a thread root, so it cannot be used as one.
		expectStatus(t, env.get("/api/v1/topics/"+itoa(question)+"/comments?parent_comment_id="+itoa(reply)), 422)
		// An answer from another topic does not belong here.
		otherQuestion := env.newQuestion(false)
		otherAnswer, err := env.answer(otherQuestion, env.authorID, false)
		if err != nil {
			t.Fatal(err)
		}
		expectStatus(t, env.get("/api/v1/topics/"+itoa(question)+"/comments?answer_id="+itoa(otherAnswer)), 422)
		expectStatus(t, env.get("/api/v1/topics/"+itoa(question)+"/comments?answer_id=abc"), 422)
		expectStatus(t, env.get("/api/v1/topics/"+itoa(question)+"/comments?parent_comment_id=abc"), 422)
		expectStatus(t, env.get("/api/v1/topics/abc/comments"), 422)
		expectStatus(t, env.get("/api/v1/topics/99999999/comments"), 404)
	})

	t.Run("评论列表分页", func(t *testing.T) {
		page := decodeReadPage(t, env.get("/api/v1/topics/"+itoa(question)+"/comments?limit=1"))
		if !page.HasMore || page.NextCursor == "" || len(page.Items) != 1 {
			t.Fatalf("comment page is wrong: %+v", page)
		}
		if fieldOf(t, page.Items[0], "id") != itoa(top) {
			t.Fatalf("first comment page returned %s", string(page.Items[0]))
		}
		next := decodeReadPage(t, env.get("/api/v1/topics/"+itoa(question)+"/comments?limit=1&cursor="+page.NextCursor))
		if len(next.Items) != 1 || fieldOf(t, next.Items[0], "id") != itoa(secondTop) {
			t.Fatalf("second comment page returned %v", idsOf(t, next))
		}
		if next.HasMore {
			t.Fatalf("two top-level comments reported a third page: %+v", next)
		}
		// A cursor issued for the topic-level list must not be accepted by the thread list, because the two
		// are different orderings of different sets.
		expectStatus(t, env.get("/api/v1/topics/"+itoa(question)+"/comments?limit=1&cursor="+page.NextCursor+
			"&parent_comment_id="+itoa(top)), 400)
	})

	t.Run("列表按祖先闭包过滤而不是只信SQL", func(t *testing.T) {
		// Hiding a thread root does not rewrite its replies: their own rows stay 'visible', so the SQL
		// narrowing returns them. The store-level list must still refuse them, because the shared rule —
		// not the query — decides what a reader can reach. Removing reachableComments would make this fail.
		env.hideContent(t, itoa(top))
		hiddenRootReplies, _, _, err := env.store.ListComments(context.Background(),
			commentFilter{TopicID: question, ParentCommentID: top}, pageRequest{Limit: 20})
		if err != nil {
			t.Fatal(err)
		}
		if len(hiddenRootReplies) != 0 {
			t.Fatalf("replies under a hidden thread root were returned: %+v", hiddenRootReplies)
		}
		// The endpoint refuses the target outright, which is the same answer in the shape a client sees.
		expectStatus(t, env.get("/api/v1/topics/"+itoa(question)+"/comments?parent_comment_id="+itoa(top)), 422)
		// Restoring the root restores its replies, so the filter is about reachability and not about a
		// cache that latched on the first answer.
		if _, err := env.db.Exec(`UPDATE contents SET moderation_status='visible' WHERE id=?`, top); err != nil {
			t.Fatal(err)
		}
		restored, _, _, err := env.store.ListComments(context.Background(),
			commentFilter{TopicID: question, ParentCommentID: top}, pageRequest{Limit: 20})
		if err != nil {
			t.Fatal(err)
		}
		if len(restored) != 2 {
			t.Fatalf("restoring the root did not restore its replies: %+v", restored)
		}
	})

	// The question's last activity must have moved with the comments, which is what the active sort reads.
	var activity string
	if err := env.db.QueryRow(`SELECT CAST(last_activity_at AS CHAR) FROM topics WHERE content_id=?`, question).Scan(&activity); err != nil {
		t.Fatal(err)
	}
	if activity == "" {
		t.Fatal("last_activity_at is empty")
	}
}

func TestReadTagList(t *testing.T) {
	env := newTopicEnv(t)
	env.createTag("帮助")
	env.createTag("测试")
	env.createTag("alpha")
	env.createTag("beta")

	t.Run("按名称排序并可搜索", func(t *testing.T) {
		res := env.get("/api/v1/tags")
		expectStatus(t, res, 200)
		page := decodeReadPage(t, res)
		names := make([]string, 0, len(page.Items))
		position := map[string]int{}
		for index, item := range page.Items {
			name := fieldOf(t, item, "name")
			names = append(names, name)
			position[name] = index
		}
		if len(names) != 5 {
			t.Fatalf("tag list is %v, want five tags", names)
		}
		// Ordering is asserted on the ASCII names: the database collation and Go's byte ordering agree
		// there, while for CJK they are free to disagree and this test is not about the collation.
		if position["alpha"] > position["beta"] {
			t.Fatalf("tags are not ordered by name: %v", names)
		}
		found := decodeReadPage(t, env.get("/api/v1/tags?q=%E5%B8%AE%E5%8A%A9"))
		if ids := idsOf(t, found); len(ids) != 1 {
			t.Fatalf("search returned %v", ids)
		}
		if fieldOf(t, found.Items[0], "name") != "帮助" {
			t.Fatalf("search returned the wrong tag: %s", string(found.Items[0]))
		}
		if items := idsOf(t, decodeReadPage(t, env.get("/api/v1/tags?q=zzz"))); len(items) != 0 {
			t.Fatalf("an unmatched search returned %v", items)
		}
	})

	t.Run("通配符被转义", func(t *testing.T) {
		// A query of "%" must look for a literal percent sign, not match every tag.
		items := idsOf(t, decodeReadPage(t, env.get("/api/v1/tags?q=%25")))
		if len(items) != 0 {
			t.Fatalf("a wildcard query matched %d tags", len(items))
		}
	})

	t.Run("分页不重不漏", func(t *testing.T) {
		all := idsOf(t, decodeReadPage(t, env.get("/api/v1/tags?limit=100")))
		var collected []string
		cursor := ""
		for pages := 0; ; pages++ {
			if pages > 10 {
				t.Fatal("tag pagination did not terminate")
			}
			path := "/api/v1/tags?limit=1"
			if cursor != "" {
				path += "&cursor=" + cursor
			}
			page := decodeReadPage(t, env.get(path))
			collected = append(collected, idsOf(t, page)...)
			if !page.HasMore {
				break
			}
			cursor = page.NextCursor
		}
		if len(collected) != len(all) {
			t.Fatalf("paged tags %v, single page %v", collected, all)
		}
		for index := range all {
			if collected[index] != all[index] {
				t.Fatalf("paged tags %v differ from a single page %v", collected, all)
			}
		}
	})

	t.Run("非法参数被拒", func(t *testing.T) {
		expectStatus(t, env.get("/api/v1/tags?limit=101"), 422)
		expectStatus(t, env.get("/api/v1/tags?cursor=garbage"), 400)
	})
}
