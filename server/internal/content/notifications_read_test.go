package content

import (
	"encoding/json"
	"strings"
	"testing"
)

// The notification list is the only read in this package served behind a session, so the tests cover both
// halves: who may read which rows, and what a row is allowed to say about content the reader may no
// longer see. Both are privacy questions, which is why they are asserted against real MySQL rather than
// reasoned about.

type notificationItem struct {
	ID        string          `json:"id"`
	EventType string          `json:"event_type"`
	ContentID string          `json:"content_id"`
	Summary   string          `json:"summary"`
	Actor     json.RawMessage `json:"actor"`
	CreatedAt string          `json:"created_at"`
	ReadAt    string          `json:"read_at"`
}

type notificationPage struct {
	Items      []notificationItem `json:"items"`
	HasMore    bool               `json:"has_more"`
	NextCursor string             `json:"next_cursor"`
}

func decodeNotifications(t *testing.T, res response) notificationPage {
	t.Helper()
	var page notificationPage
	if err := json.Unmarshal(res.body, &page); err != nil {
		t.Fatalf("cannot decode %s: %v", string(res.body), err)
	}
	return page
}

// actorField reads one field of the actor object, or "" when there is no actor at all.
func actorField(t *testing.T, actor json.RawMessage, name string) string {
	t.Helper()
	if len(actor) == 0 {
		return ""
	}
	return fieldOf(t, actor, name)
}

func TestNotificationListShowsOnlyOwn(t *testing.T) {
	env := newTopicEnv(t)
	question := env.newQuestion(false)
	answerer := env.newUser("answerer")
	answer, err := env.answer(question, answerer, false)
	if err != nil {
		t.Fatal(err)
	}

	askerCookie, _ := env.sessionCookie(env.authorID)
	answererCookie, _ := env.sessionCookie(answerer)

	// A guest has no list at all.
	expectStatus(t, env.get("/api/v1/me/notifications"), 401)

	res := env.getAs("/api/v1/me/notifications", askerCookie)
	expectStatus(t, res, 200)
	page := decodeNotifications(t, res)
	if len(page.Items) != 1 {
		t.Fatalf("the asker sees %d notifications, want 1: %s", len(page.Items), string(res.body))
	}
	item := page.Items[0]
	if item.EventType != eventNewAnswer || item.ContentID != itoa(answer) {
		t.Fatalf("notification is wrong: %+v", item)
	}
	if item.Summary != "你的问题有了新的回答" {
		t.Fatalf("summary is %q", item.Summary)
	}
	if item.ReadAt != "" {
		t.Fatalf("a fresh notification should be unread: %+v", item)
	}
	if mode := actorField(t, item.Actor, "mode"); mode != "named" {
		t.Fatalf("actor mode is %q", mode)
	}
	var actor struct {
		User struct {
			Nickname string `json:"nickname"`
			PublicID string `json:"public_id"`
		} `json:"user"`
	}
	if err := json.Unmarshal(item.Actor, &actor); err != nil {
		t.Fatal(err)
	}
	if actor.User.Nickname != env.nicknameOf(answerer) {
		t.Fatalf("actor nickname is %q", actor.User.Nickname)
	}
	// The actor carries the *public* id, which is what the rest of the API exposes.
	if actor.User.PublicID != env.publicIDOf(answerer) {
		t.Fatalf("actor public id is wrong")
	}

	// The person who wrote the answer is not notified about it, so their inbox is empty.
	own := decodeNotifications(t, env.getAs("/api/v1/me/notifications", answererCookie))
	if len(own.Items) != 0 {
		t.Fatalf("the actor's own inbox has %d rows: %+v", len(own.Items), own.Items)
	}
}

func TestNotificationActorFollowsAnonymity(t *testing.T) {
	env := newTopicEnv(t)
	// An anonymous question: the asker is the topic's anonymous owner.
	question := env.newQuestion(true)
	answerer := env.newUser("answerer")
	answer, err := env.answer(question, answerer, false)
	if err != nil {
		t.Fatal(err)
	}
	// The answerer sees the owner as 楼主 when the owner comments on their answer, and the owner's real
	// account is nowhere in the payload.
	if _, err := env.comment(NewComment{AuthorID: env.authorID, TopicID: question, AnswerID: answer}); err != nil {
		t.Fatal(err)
	}
	answererCookie, _ := env.sessionCookie(answerer)
	res := env.getAs("/api/v1/me/notifications", answererCookie)
	page := decodeNotifications(t, res)
	if len(page.Items) != 1 {
		t.Fatalf("the answerer sees %d notifications, want 1: %s", len(page.Items), string(res.body))
	}
	item := page.Items[0]
	if mode := actorField(t, item.Actor, "mode"); mode != "anonymous" {
		t.Fatalf("actor mode is %q, want anonymous: %s", mode, string(item.Actor))
	}
	if name := actorField(t, item.Actor, "display_name"); name != "楼主" {
		t.Fatalf("anonymous owner rendered as %q", name)
	}
	if name := env.nicknameOf(env.authorID); strings.Contains(string(res.body), name) {
		t.Fatalf("the anonymous owner's nickname leaked into the notification list: %s", string(res.body))
	}
	if publicID := env.publicIDOf(env.authorID); strings.Contains(string(res.body), publicID) {
		t.Fatalf("the anonymous owner's public id leaked into the notification list")
	}
	// The actor object must not carry a user sub-object on the anonymous branch at all.
	if hasField(t, item.Actor, "user") {
		t.Fatalf("the anonymous actor carried a user object: %s", string(item.Actor))
	}

	// An anonymous participant who is not the owner is shown by their discussion-stable number.
	participant := env.newUser("participant")
	secondQuestion := env.newQuestion(false)
	secondAnswer, err := env.answer(secondQuestion, participant, true)
	if err != nil {
		t.Fatal(err)
	}
	askerCookie, _ := env.sessionCookie(env.authorID)
	second := decodeNotifications(t, env.getAs("/api/v1/me/notifications", askerCookie))
	// The asker's inbox now holds both events, so the anonymous answer is found by its content id rather
	// than by position.
	var participantItem *notificationItem
	for index := range second.Items {
		if second.Items[index].ContentID == itoa(secondAnswer) {
			participantItem = &second.Items[index]
		}
	}
	if participantItem == nil {
		t.Fatalf("no notification for the anonymous participant's answer: %+v", second.Items)
	}
	if name := actorField(t, participantItem.Actor, "display_name"); name != "匿名#1" {
		t.Fatalf("anonymous participant rendered as %q", name)
	}
}

func TestNotificationWithoutVisibleContentOmitsActor(t *testing.T) {
	env := newTopicEnv(t)
	question := env.newQuestion(false)
	answerer := env.newUser("answerer")
	answer, err := env.answer(question, answerer, false)
	if err != nil {
		t.Fatal(err)
	}
	askerCookie, _ := env.sessionCookie(env.authorID)
	before := decodeNotifications(t, env.getAs("/api/v1/me/notifications", askerCookie))
	if len(before.Items) != 1 || actorField(t, before.Items[0].Actor, "mode") == "" {
		t.Fatalf("expected one notification with an actor: %s", string(env.getAs("/api/v1/me/notifications", askerCookie).body))
	}

	// Hiding the answer makes it unreachable for the asker too, and the notification must stop describing
	// who wrote it — the event happened, but the actor is no longer something the reader may know.
	env.hideContent(t, itoa(answer))

	res := env.getAs("/api/v1/me/notifications", askerCookie)
	expectStatus(t, res, 200)
	page := decodeNotifications(t, res)
	if len(page.Items) != 1 {
		t.Fatalf("the notification disappeared with its content: %s", string(res.body))
	}
	item := page.Items[0]
	if len(item.Actor) != 0 {
		t.Fatalf("an unreachable content still carried an actor: %s", string(item.Actor))
	}
	if item.Summary != "该内容已被删除或不可见" {
		t.Fatalf("summary for unreachable content is %q", item.Summary)
	}
	if item.ContentID != itoa(answer) {
		t.Fatalf("the content id is needed to locate the event: %+v", item)
	}
	// The decisive assertion: none of the author's identifiers survive.
	for _, secret := range []string{env.nicknameOf(answerer), env.publicIDOf(answerer)} {
		if strings.Contains(string(res.body), secret) {
			t.Fatalf("a hidden answer's author leaked through the notification: %s", string(res.body))
		}
	}

	// The same holds for a deletion, which is the other way content becomes unreachable.
	second := env.newQuestion(false)
	secondAnswer, err := env.answer(second, answerer, false)
	if err != nil {
		t.Fatal(err)
	}
	env.deleteContent(secondAnswer)
	afterDelete := decodeNotifications(t, env.getAs("/api/v1/me/notifications", askerCookie))
	for _, candidate := range afterDelete.Items {
		if candidate.ContentID == itoa(secondAnswer) {
			if len(candidate.Actor) != 0 || candidate.Summary != "该内容已被删除或不可见" {
				t.Fatalf("a deleted answer was still described: %+v", candidate)
			}
		}
	}
}

func TestMarkNotificationRead(t *testing.T) {
	env := newTopicEnv(t)
	question := env.newQuestion(false)
	answerer := env.newUser("answerer")
	if _, err := env.answer(question, answerer, false); err != nil {
		t.Fatal(err)
	}
	askerCookie, askerCSRF := env.sessionCookie(env.authorID)
	answererCookie, answererCSRF := env.sessionCookie(answerer)

	page := decodeNotifications(t, env.getAs("/api/v1/me/notifications", askerCookie))
	if len(page.Items) != 1 {
		t.Fatalf("expected one notification, got %d", len(page.Items))
	}
	id := page.Items[0].ID

	// Somebody else's notification is reported as missing, not as forbidden: the endpoint must not confirm
	// that a notification exists for another account.
	expectStatus(t, env.do("PUT", "/api/v1/me/notifications/"+id+"/read", "", answererCookie, answererCSRF), 404)
	expectCode(t, env.do("PUT", "/api/v1/me/notifications/"+id+"/read", "", answererCookie, answererCSRF), "NOTIFICATION_NOT_FOUND")

	expectStatus(t, env.do("PUT", "/api/v1/me/notifications/"+id+"/read", "", askerCookie, askerCSRF), 204)
	read := decodeNotifications(t, env.getAs("/api/v1/me/notifications", askerCookie))
	if read.Items[0].ReadAt == "" {
		t.Fatalf("read_at was not set: %+v", read.Items[0])
	}
	firstReadAt := read.Items[0].ReadAt

	// Marking it again is not an error, and does not move the timestamp.
	expectStatus(t, env.do("PUT", "/api/v1/me/notifications/"+id+"/read", "", askerCookie, askerCSRF), 204)
	again := decodeNotifications(t, env.getAs("/api/v1/me/notifications", askerCookie))
	if again.Items[0].ReadAt != firstReadAt {
		t.Fatalf("a second mark moved read_at from %q to %q", firstReadAt, again.Items[0].ReadAt)
	}

	// A missing id, a malformed id, and a write without a CSRF token.
	expectStatus(t, env.do("PUT", "/api/v1/me/notifications/99999999/read", "", askerCookie, askerCSRF), 404)
	expectStatus(t, env.do("PUT", "/api/v1/me/notifications/abc/read", "", askerCookie, askerCSRF), 422)
	expectStatus(t, env.do("PUT", "/api/v1/me/notifications/"+id+"/read", "", askerCookie, ""), 403)
	expectStatus(t, env.do("PUT", "/api/v1/me/notifications/"+id+"/read", "", "", ""), 401)
}

func TestNotificationPagination(t *testing.T) {
	env := newTopicEnv(t)
	question := env.newQuestion(false)
	answerer := env.newUser("answerer")
	commenter := env.newUser("commenter")
	answer, err := env.answer(question, answerer, false)
	if err != nil {
		t.Fatal(err)
	}
	comment, err := env.comment(NewComment{AuthorID: commenter, TopicID: question})
	if err != nil {
		t.Fatal(err)
	}
	askerCookie, _ := env.sessionCookie(env.authorID)

	page := decodeNotifications(t, env.getAs("/api/v1/me/notifications?limit=1", askerCookie))
	if !page.HasMore || page.NextCursor == "" || len(page.Items) != 1 {
		t.Fatalf("first page is wrong: %+v", page)
	}
	// Newest first: the comment came after the answer.
	if page.Items[0].ContentID != itoa(comment) {
		t.Fatalf("first page returned %s, want the newest (%d)", page.Items[0].ContentID, comment)
	}
	next := decodeNotifications(t, env.getAs("/api/v1/me/notifications?limit=1&cursor="+page.NextCursor, askerCookie))
	if len(next.Items) != 1 || next.Items[0].ContentID != itoa(answer) {
		t.Fatalf("second page returned %+v, want the answer %d", next.Items, answer)
	}
	if next.HasMore {
		t.Fatalf("a two-row list reported a third page: %+v", next)
	}
	// A cursor is bound to this list; a foreign one is refused rather than silently mispaging.
	expectStatus(t, env.getAs("/api/v1/me/notifications?cursor="+encodeCursor(boardCursorScope, 1, 1), askerCookie), 400)
}
