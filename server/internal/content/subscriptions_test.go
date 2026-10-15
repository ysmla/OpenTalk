package content

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The unsubscribe link is the only unauthenticated write in this package, so its tests try to use it the
// way an attacker would: edit the payload, reuse a token for another topic, extend the expiry, and present
// a token minted with a different subkey.

func decodeSubscription(t *testing.T, res response) subscriptionResponse {
	t.Helper()
	var subscription subscriptionResponse
	if err := json.Unmarshal(res.body, &subscription); err != nil {
		t.Fatalf("cannot decode %s: %v", string(res.body), err)
	}
	return subscription
}

func TestTopicSubscriptionDefaultsToInherit(t *testing.T) {
	env := newTopicEnv(t)
	topic := env.topic(env.boardID, "订阅测试")
	cookie, csrf := env.sessionCookie(env.authorID)

	// Never set: inherit the global preference, rather than a value invented here.
	res := env.getAs("/api/v1/me/subscriptions/"+itoa(topic), cookie)
	expectStatus(t, res, 200)
	if subscription := decodeSubscription(t, res); subscription.EmailOverride != overrideInherit || subscription.TopicID != itoa(topic) {
		t.Fatalf("default subscription is %+v", subscription)
	}

	// Each of the three values round-trips, and the last write wins rather than piling up rows.
	for _, value := range []string{overrideDisabled, overrideEnabled, overrideInherit} {
		body := `{"topic_id":"` + itoa(topic) + `","email_override":"` + value + `"}`
		expectStatus(t, env.do("PUT", "/api/v1/me/subscriptions/"+itoa(topic), body, cookie, csrf), 200)
		read := decodeSubscription(t, env.getAs("/api/v1/me/subscriptions/"+itoa(topic), cookie))
		if read.EmailOverride != value {
			t.Fatalf("after writing %q the read is %+v", value, read)
		}
	}
	var rows int
	if err := env.db.QueryRow(`SELECT COUNT(*) FROM topic_subscriptions WHERE user_id=? AND topic_id=?`,
		env.authorID, topic).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("repeated writes produced %d rows", rows)
	}
}

func TestTopicSubscriptionRejectsBadRequests(t *testing.T) {
	env := newTopicEnv(t)
	topic := env.topic(env.boardID, "订阅测试")
	other := env.topic(env.boardID, "另一个主题")
	cookie, csrf := env.sessionCookie(env.authorID)

	// The body's topic_id must match the path, so a client cannot change one topic while believing it
	// changed another.
	mismatch := `{"topic_id":"` + itoa(other) + `","email_override":"disabled"}`
	expectStatus(t, env.do("PUT", "/api/v1/me/subscriptions/"+itoa(topic), mismatch, cookie, csrf), 422)
	expectStatus(t, env.do("PUT", "/api/v1/me/subscriptions/"+itoa(topic), `{"email_override":"disabled"}`, cookie, csrf), 422)
	expectStatus(t, env.do("PUT", "/api/v1/me/subscriptions/"+itoa(topic),
		`{"topic_id":"`+itoa(topic)+`","email_override":"sometimes"}`, cookie, csrf), 422)
	expectStatus(t, env.do("PUT", "/api/v1/me/subscriptions/abc",
		`{"topic_id":"abc","email_override":"disabled"}`, cookie, csrf), 422)
	expectStatus(t, env.getAs("/api/v1/me/subscriptions/99999999", cookie), 404)

	// None of the refused writes stored anything.
	var rows int
	if err := env.db.QueryRow(`SELECT COUNT(*) FROM topic_subscriptions WHERE user_id=?`, env.authorID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("a rejected request stored %d rows", rows)
	}

	// A deleted or hidden topic is not something a reader can hold a mail setting for.
	hidden := env.topic(env.boardID, "隐藏的主题")
	env.hideContent(t, itoa(hidden))
	expectStatus(t, env.getAs("/api/v1/me/subscriptions/"+itoa(hidden), cookie), 404)
	expectStatus(t, env.do("PUT", "/api/v1/me/subscriptions/"+itoa(hidden),
		`{"topic_id":"`+itoa(hidden)+`","email_override":"enabled"}`, cookie, csrf), 404)

	// Session and CSRF still apply to the authenticated pair.
	expectStatus(t, env.get("/api/v1/me/subscriptions/"+itoa(topic)), 401)
	expectStatus(t, env.do("PUT", "/api/v1/me/subscriptions/"+itoa(topic),
		`{"topic_id":"`+itoa(topic)+`","email_override":"disabled"}`, cookie, ""), 403)
}

func TestUnsubscribeTokenDisablesOnlyItsTopic(t *testing.T) {
	env := newTopicEnv(t)
	topic := env.topic(env.boardID, "订阅测试")
	otherTopic := env.topic(env.boardID, "另一个主题")
	reader := env.newUser("reader")

	// A token for one account and one topic, as the mail would carry it.
	token, err := env.deps.mintUnsubscribeToken(reader, topic, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// No session is needed: the signature is the credential.
	expectStatus(t, env.do("POST", "/api/v1/subscriptions/unsubscribe", `{"token":"`+token+`"}`, "", ""), 204)

	override, err := env.store.TopicSubscription(context.Background(), reader, topic)
	if err != nil {
		t.Fatal(err)
	}
	if override != overrideDisabled {
		t.Fatalf("after unsubscribing the override is %q", override)
	}
	// Exactly one topic is affected: the neighbouring discussion keeps inheriting.
	neighbour, err := env.store.TopicSubscription(context.Background(), reader, otherTopic)
	if err != nil {
		t.Fatal(err)
	}
	if neighbour != overrideInherit {
		t.Fatalf("the neighbouring topic was changed to %q", neighbour)
	}
	// The global preferences are untouched: a per-topic link cannot flip the account-wide switch.
	preferences, err := env.store.NotificationPreferences(context.Background(), reader)
	if err != nil {
		t.Fatal(err)
	}
	if preferences != (notificationPreferences{}) {
		t.Fatalf("the unsubscribe link changed global preferences: %+v", preferences)
	}
}

func TestUnsubscribeTokenRejectsForgery(t *testing.T) {
	env := newTopicEnv(t)
	topic := env.topic(env.boardID, "订阅测试")
	otherTopic := env.topic(env.boardID, "另一个主题")
	reader := env.newUser("reader")
	other := env.newUser("other")

	valid, err := env.deps.mintUnsubscribeToken(reader, topic, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	post := func(token string) response {
		return env.do("POST", "/api/v1/subscriptions/unsubscribe", `{"token":"`+token+`"}`, "", "")
	}

	// A valid token for another topic still only does what it says — but here the point is that the two
	// halves cannot be recombined: swapping in another payload breaks the signature.
	otherToken, err := env.deps.mintUnsubscribeToken(other, otherTopic, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(valid, ".")
	otherParts := strings.Split(otherToken, ".")
	expectStatus(t, post(parts[0]+"."+otherParts[1]), 400)

	// Editing the payload to name another topic or account fails the signature as well.
	expectStatus(t, post(otherParts[0]+"."+parts[1]), 400)

	// Garbage, an empty value, and a truncated token.
	expectStatus(t, post("not-a-token"), 400)
	expectCode(t, post("not-a-token"), "INVALID_TOKEN")
	expectStatus(t, post(""), 400)
	expectStatus(t, post(parts[0]), 400)
	expectStatus(t, post(parts[0]+"."), 400)

	// An expired token is refused even though its signature is genuine. The expiry is inside the signed
	// payload, so it cannot be pushed forward by editing the token.
	expired, err := env.deps.mintUnsubscribeToken(reader, topic, time.Now().Add(-unsubscribeTokenTTL-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	expectStatus(t, post(expired), 400)

	// A token minted with a different subkey — the classic confusion attack against a shared secret.
	stranger := Deps{Store: env.store, UnsubscribeKey: []byte(strings.Repeat("z", 32))}
	foreign, err := stranger.mintUnsubscribeToken(reader, topic, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	expectStatus(t, post(foreign), 400)

	// Nothing any of the forged tokens asked for happened.
	override, err := env.store.TopicSubscription(context.Background(), reader, topic)
	if err != nil {
		t.Fatal(err)
	}
	if override != overrideInherit {
		t.Fatalf("a rejected token still changed the subscription to %q", override)
	}
}
