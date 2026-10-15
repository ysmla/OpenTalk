package content

import (
	"context"
	"strconv"
	"strings"
	"testing"
)

// Search narrows candidates in SQL and then decides with the same rules everything else uses, so the tests
// that matter are the ones about what it must *not* return: hidden content, content deleted after it was
// written, descendants of a hidden topic, and the identity behind anonymous content.

func (e *topicEnv) search(query string) response {
	e.t.Helper()
	return e.get("/api/v1/search?q=" + urlEncode(query))
}

// searchIDs returns the ids in the order the page returned them.
func searchIDs(t *testing.T, res response) []string {
	t.Helper()
	return idsOf(t, decodeReadPage(t, res))
}

func urlEncode(value string) string {
	var builder strings.Builder
	for _, b := range []byte(value) {
		builder.WriteString("%")
		const hex = "0123456789ABCDEF"
		builder.WriteByte(hex[b>>4])
		builder.WriteByte(hex[b&0x0f])
	}
	return builder.String()
}

// atoi parses an id that arrived as a JSON string, for order comparisons in assertions.
func atoi(value string) uint64 {
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0
	}
	return parsed
}

func TestSearchFindsVisibleContent(t *testing.T) {
	env := newTopicEnv(t)
	// A distinctive token so the fixtures cannot collide with anything else in the schema.
	const token = "zzsearchtoken"
	question := env.newQuestion(false)
	answerer := env.newUser("answerer")
	answer, err := env.answer(question, answerer, false)
	if err != nil {
		t.Fatal(err)
	}
	// One topic matches by title, one by body, and the answer carries the token in its body only.
	titled := env.topic(env.boardID, "About "+token)
	bodied := env.topic(env.boardID, "subject line")
	if _, err := env.db.Exec(`UPDATE contents SET body_md=? WHERE id=?`, "body with "+token, bodied); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(`UPDATE contents SET body_md=? WHERE id=?`, "answer mentioning "+token, answer); err != nil {
		t.Fatal(err)
	}
	commenter := env.newUser("commenter")
	comment, err := env.comment(NewComment{AuthorID: commenter, TopicID: question, BodyMD: "comment with " + token})
	if err != nil {
		t.Fatal(err)
	}

	ids := searchIDs(t, env.search(token))
	// The question itself must NOT match: nothing in it carries the token. That negative half matters — a
	// search that returned everything would satisfy a positive-only assertion.
	want := map[string]bool{itoa(titled): false, itoa(bodied): false, itoa(answer): false, itoa(comment): false}
	for _, id := range ids {
		if id == itoa(question) {
			t.Fatalf("a topic without the token matched: %v", ids)
		}
		if _, expected := want[id]; expected {
			want[id] = true
		}
	}
	for id, seen := range want {
		if !seen {
			t.Fatalf("id %s is missing from the search page %v", id, ids)
		}
	}
	// Newest first, which is the documented order.
	for index := 1; index < len(ids); index++ {
		if atoi(ids[index-1]) < atoi(ids[index]) {
			t.Fatalf("results are not newest first: %v", ids)
		}
	}
}

func TestSearchNeverWidensVisibility(t *testing.T) {
	env := newTopicEnv(t)
	const token = "zzhiddentoken"
	hiddenTopic := env.newQuestion(false)
	if _, err := env.db.Exec(`UPDATE contents SET body_md=? WHERE id=?`, "question with "+token, hiddenTopic); err != nil {
		t.Fatal(err)
	}
	// A descendant whose own row stays visible: hiding the question must take it out of search too, which
	// only the shared ancestor rule can decide — the SQL sees an undeleted, unmoderated answer.
	answer, err := env.answer(hiddenTopic, env.newUser("answerer"), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(`UPDATE contents SET body_md=? WHERE id=?`, "answer with "+token, answer); err != nil {
		t.Fatal(err)
	}
	env.hideContent(t, itoa(hiddenTopic))

	// A topic deleted after the fact.
	deleted := env.topic(env.boardID, "deleted "+token)
	env.deleteContent(deleted)

	ids := searchIDs(t, env.search(token))
	for _, id := range ids {
		if id == itoa(hiddenTopic) || id == itoa(answer) || id == itoa(deleted) {
			t.Fatalf("search returned %s, which is not visible: %v", id, ids)
		}
	}

	// The decisive direction: the payload must not describe the hidden answer's author either.
	res := env.search(token)
	for _, id := range searchIDs(t, res) {
		if id == itoa(answer) {
			t.Fatalf("a hidden question's answer appeared: %s", string(res.body))
		}
	}
}

func TestSearchDoesNotLeakAnonymousAuthors(t *testing.T) {
	env := newTopicEnv(t)
	const token = "zzanontoken"
	anonymousAuthor := env.newUser("secretive")
	in := env.base()
	in.AuthorID = anonymousAuthor
	in.BoardID = env.qaBoardID
	in.IsAnonymous = true
	in.Title = "anonymous " + token
	in.TagIDs = nil
	if _, err := env.store.CreateTopic(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	res := env.search(token)
	expectStatus(t, res, 200)
	if len(searchIDs(t, res)) != 1 {
		t.Fatalf("expected one result: %s", string(res.body))
	}
	// Neither the nickname nor the public id of the account behind the anonymous post may appear.
	for _, secret := range []string{env.nicknameOf(anonymousAuthor), env.publicIDOf(anonymousAuthor)} {
		if strings.Contains(string(res.body), secret) {
			t.Fatalf("search leaked %q: %s", secret, string(res.body))
		}
	}
	if !strings.Contains(string(res.body), `"mode":"anonymous"`) {
		t.Fatalf("the anonymous author was not rendered as anonymous: %s", string(res.body))
	}
}

func TestSearchFiltersAndBoundaries(t *testing.T) {
	env := newTopicEnv(t)
	const token = "zzfiltertoken"
	discussion := env.topic(env.boardID, "in discussion "+token)
	question := env.newQuestion(false)
	if _, err := env.db.Exec(`UPDATE contents SET body_md=? WHERE id=?`, "question with "+token, question); err != nil {
		t.Fatal(err)
	}
	tagged, err := env.store.CreateTopic(context.Background(), func() NewTopic {
		in := env.base()
		in.Title = "tagged " + token
		return in
	}())
	if err != nil {
		t.Fatal(err)
	}

	// board_id restricts to that board's topics and their descendants. Both `discussion` and `tagged` live in
	// the discussion board; the question does not.
	inDiscussion := searchIDs(t, env.get("/api/v1/search?q="+token+"&board_id="+itoa(env.boardID)))
	if len(inDiscussion) != 2 {
		t.Fatalf("board filter returned %v, want the two discussion topics", inDiscussion)
	}
	seenDiscussion := map[string]bool{}
	for _, id := range inDiscussion {
		if id == itoa(question) {
			t.Fatalf("the question leaked into the discussion board filter: %v", inDiscussion)
		}
		seenDiscussion[id] = true
	}
	for _, id := range []uint64{discussion, tagged} {
		if !seenDiscussion[itoa(id)] {
			t.Fatalf("topic %d is missing from the board filter: %v", id, inDiscussion)
		}
	}
	inQA := searchIDs(t, env.get("/api/v1/search?q="+token+"&board_id="+itoa(env.qaBoardID)))
	if len(inQA) != 1 || inQA[0] != itoa(question) {
		t.Fatalf("qa board filter returned %v", inQA)
	}
	// tag_id restricts to topics carrying it, which is only the one the fixture tagged.
	taggedIDs := searchIDs(t, env.get("/api/v1/search?q="+token+"&tag_id="+itoa(env.tagID)))
	if len(taggedIDs) != 1 || taggedIDs[0] != itoa(tagged) {
		t.Fatalf("tag filter returned %v", taggedIDs)
	}

	// A missing or blank query is refused: the parameter is required, and scanning for everything is not a
	// search.
	expectStatus(t, env.get("/api/v1/search"), 422)
	expectStatus(t, env.get("/api/v1/search?q="), 422)
	expectStatus(t, env.get("/api/v1/search?q=%20%20"), 422)
	expectCode(t, env.get("/api/v1/search?q="), "INVALID_REQUEST")
	// A pattern longer than the bound is refused rather than truncated.
	long := strings.Repeat("z", searchMaxQuery+1)
	expectStatus(t, env.get("/api/v1/search?q="+long), 422)
	expectStatus(t, env.get("/api/v1/search?q="+token+"&board_id=999999"), 404)
	expectStatus(t, env.get("/api/v1/search?q="+token+"&tag_id=999999"), 404)
	expectStatus(t, env.get("/api/v1/search?q="+token+"&board_id=abc"), 422)
	expectStatus(t, env.get("/api/v1/search?q="+token+"&limit=101"), 422)
	expectStatus(t, env.get("/api/v1/search?q="+token+"&cursor=garbage"), 400)

	// LIKE wildcards in the query are literal, so "%" searches for a percent sign and finds nothing here.
	if ids := searchIDs(t, env.search("%")); len(ids) != 0 {
		t.Fatalf("a wildcard query matched %v", ids)
	}
	if ids := searchIDs(t, env.search("_")); len(ids) != 0 {
		t.Fatalf("an underscore query matched %v", ids)
	}
}

func TestSearchPaginationIsStable(t *testing.T) {
	env := newTopicEnv(t)
	const token = "zzpagetoken"
	for index := 0; index < 5; index++ {
		env.topic(env.boardID, "paged "+itoa(uint64(index))+" "+token)
	}
	all := searchIDs(t, env.get("/api/v1/search?q="+token+"&limit=100"))
	if len(all) != 5 {
		t.Fatalf("expected five results, got %v", all)
	}

	var collected []string
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
		path := "/api/v1/search?q=" + token + "&limit=2"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		page := decodeReadPage(t, env.get(path))
		collected = append(collected, idsOf(t, page)...)
		if !page.HasMore {
			break
		}
		if page.NextCursor == "" {
			t.Fatal("has_more without a cursor cannot be followed")
		}
		cursor = page.NextCursor
	}
	if len(collected) != len(all) {
		t.Fatalf("paged %v, single page %v", collected, all)
	}
	for index := range all {
		if collected[index] != all[index] {
			t.Fatalf("paged %v differ from a single page %v", collected, all)
		}
	}
	// A cursor from another list is refused rather than silently mispaging.
	expectStatus(t, env.get("/api/v1/search?q="+token+"&cursor="+encodeCursor(boardCursorScope, 1, 1)), 400)
}
