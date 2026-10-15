package content

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// The two lists in this file differ in exactly one way, and that difference is the point: my own list shows
// my anonymous posts, a public profile never does. These tests exist to hold that line.

func (e *topicEnv) createTopicBy(author uint64, anonymous bool, title string) uint64 {
	e.t.Helper()
	in := e.base()
	in.AuthorID = author
	in.IsAnonymous = anonymous
	in.Title = title
	in.TagIDs = nil
	id, err := e.store.CreateTopic(context.Background(), in)
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

func TestMyContentsIncludesOwnAnonymousPosts(t *testing.T) {
	env := newTopicEnv(t)
	me := env.newUser("mine")
	named := env.createTopicBy(me, false, "实名帖")
	anonymous := env.createTopicBy(me, true, "匿名帖")
	other := env.newUser("other")
	otherTopic := env.createTopicBy(other, false, "别人的帖")

	// A session is required: this list is private by definition.
	expectStatus(t, env.get("/api/v1/me/contents"), 401)

	cookie, _ := env.sessionCookie(me)
	page := env.getAs("/api/v1/me/contents", cookie)
	expectStatus(t, page, 200)
	ids := idsOf(t, decodeReadPage(t, page))
	seen := map[string]bool{}
	for _, id := range ids {
		seen[id] = true
	}
	// My anonymous post is mine: it belongs here, and it is rendered in anonymous mode rather than switching
	// to my name just because I am the one looking.
	if !seen[itoa(named)] || !seen[itoa(anonymous)] {
		t.Fatalf("my own content is incomplete: %v", ids)
	}
	if seen[itoa(otherTopic)] {
		t.Fatalf("somebody else's post is in my list: %v", ids)
	}
	if !strings.Contains(string(page.body), `"mode":"anonymous"`) {
		t.Fatalf("my anonymous post was not rendered as anonymous: %s", string(page.body))
	}
	// The other account's list is its own, which also proves the list is not global.
	otherCookie, _ := env.sessionCookie(other)
	otherIDs := idsOf(t, decodeReadPage(t, env.getAs("/api/v1/me/contents", otherCookie)))
	if len(otherIDs) != 1 || otherIDs[0] != itoa(otherTopic) {
		t.Fatalf("the other account's list is %v", otherIDs)
	}

	// Hidden content leaves the author's list too: the contract points them at their notifications for a
	// moderation outcome rather than at a row they cannot open.
	env.hideContent(t, itoa(named))
	after := idsOf(t, decodeReadPage(t, env.getAs("/api/v1/me/contents", cookie)))
	for _, id := range after {
		if id == itoa(named) {
			t.Fatalf("hidden content is still in the author's list: %v", after)
		}
	}
}

func TestPublicProfileNeverShowsAnonymousContent(t *testing.T) {
	env := newTopicEnv(t)
	author := env.newUser("someone")
	named := env.createTopicBy(author, false, "公开的帖子")
	anonymous := env.createTopicBy(author, true, "不该被关联的匿名帖")
	publicID := env.publicIDOf(author)

	// The profile itself: public fields only, never the email or the internal id.
	profile := env.get("/api/v1/users/" + publicID)
	expectStatus(t, profile, 200)
	var fields map[string]any
	if err := json.Unmarshal(profile.body, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["public_id"] == nil || fields["nickname"] == nil {
		t.Fatalf("the profile is missing its public fields: %v", fields)
	}
	for _, forbidden := range []string{"email", "email_normalized", "internal_user_id", "role", "account_status"} {
		if _, present := fields[forbidden]; present {
			t.Fatalf("the public profile exposes %q: %v", forbidden, fields)
		}
	}

	// The decisive assertion: the anonymous post is not in this list, and the response does not describe it
	// at all — not by id, not by title, not by body. Excluding it in SQL is what makes that true even if the
	// reader above changed.
	contents := env.get("/api/v1/users/" + publicID + "/contents")
	expectStatus(t, contents, 200)
	ids := idsOf(t, decodeReadPage(t, contents))
	if len(ids) != 1 || ids[0] != itoa(named) {
		t.Fatalf("the public content list is %v, want only the named post", ids)
	}
	body := string(contents.body)
	// The needle is the JSON field, not the bare id: a one-character id like "2" also occurs inside
	// timestamps, which would make this assertion pass or fail for the wrong reason.
	for _, secret := range []string{`"id":"` + itoa(anonymous) + `"`, "不该被关联的匿名帖"} {
		if strings.Contains(body, secret) {
			t.Fatalf("the public list leaked %q: %s", secret, body)
		}
	}
	// A hidden post also leaves the public list, so moderation is not bypassed by browsing a profile.
	env.hideContent(t, itoa(named))
	if after := idsOf(t, decodeReadPage(t, env.get("/api/v1/users/"+publicID+"/contents"))); len(after) != 0 {
		t.Fatalf("hidden content is still public on the profile: %v", after)
	}
}

func TestProfileAndListBoundaries(t *testing.T) {
	env := newTopicEnv(t)
	author := env.newUser("someone")
	topic := env.createTopicBy(author, false, "帖子")
	publicID := env.publicIDOf(author)

	// An id that cannot exist is answered as missing, not as a validation error: telling a caller which
	// strings are shaped like ids is a small gift to an enumerator.
	expectStatus(t, env.get("/api/v1/users/not-a-real-id"), 404)
	expectStatus(t, env.get("/api/v1/users/not-a-real-id/contents"), 404)
	expectCode(t, env.get("/api/v1/users/not-a-real-id"), "USER_NOT_FOUND")

	// Deleted content leaves both lists.
	env.deleteContent(topic)
	expectStatus(t, env.get("/api/v1/users/"+publicID+"/contents"), 200)
	if ids := idsOf(t, decodeReadPage(t, env.get("/api/v1/users/"+publicID+"/contents"))); len(ids) != 0 {
		t.Fatalf("deleted content is still public: %v", ids)
	}

	// Pagination and cursor scopes: both lists page, and a cursor from one is refused by the other.
	for index := 0; index < 3; index++ {
		env.createTopicBy(author, false, "分页 "+itoa(uint64(index)))
	}
	cookie, _ := env.sessionCookie(author)
	crossed := env.getAs("/api/v1/me/contents?cursor="+encodeCursor(userContentCursorScope, 1), cookie)
	expectStatus(t, crossed, 400)
	expectStatus(t, env.get("/api/v1/users/"+publicID+"/contents?limit=101"), 422)
}
