package content

import (
	"encoding/json"
	"testing"
)

// The preferences endpoint is small, but its two decisions are the kind that are expensive to get wrong:
// what an account that never chose anything gets, and what a partial write is allowed to do.

func decodePreferences(t *testing.T, res response) notificationPreferences {
	t.Helper()
	var preferences notificationPreferences
	if err := json.Unmarshal(res.body, &preferences); err != nil {
		t.Fatalf("cannot decode %s: %v", string(res.body), err)
	}
	return preferences
}

func TestNotificationPreferencesDefaultToOff(t *testing.T) {
	env := newTopicEnv(t)
	cookie, _ := env.sessionCookie(env.authorID)

	// No row has ever been written, and no row must read as "all off": inventing a default that starts
	// mailing someone who never asked is the one direction this setting must not fail in.
	res := env.getAs("/api/v1/me/notification-preferences", cookie)
	expectStatus(t, res, 200)
	if preferences := decodePreferences(t, res); preferences != (notificationPreferences{}) {
		t.Fatalf("an unset account read as %+v, want all false", preferences)
	}
}

func TestNotificationPreferencesRoundTrip(t *testing.T) {
	env := newTopicEnv(t)
	cookie, csrf := env.sessionCookie(env.authorID)

	body := `{"email_on_comment":true,"email_on_answer":false,"email_on_reply":true}`
	res := env.do("PUT", "/api/v1/me/notification-preferences", body, cookie, csrf)
	expectStatus(t, res, 200)
	if preferences := decodePreferences(t, res); !preferences.EmailOnComment || preferences.EmailOnAnswer || !preferences.EmailOnReply {
		t.Fatalf("the response did not echo the stored flags: %+v", preferences)
	}

	// A second write replaces the row rather than adding one, and the read agrees with it.
	second := env.do("PUT", "/api/v1/me/notification-preferences",
		`{"email_on_comment":false,"email_on_answer":false,"email_on_reply":false}`, cookie, csrf)
	expectStatus(t, second, 200)
	read := env.getAs("/api/v1/me/notification-preferences", cookie)
	if preferences := decodePreferences(t, read); preferences != (notificationPreferences{}) {
		t.Fatalf("after turning everything off the read is %+v", preferences)
	}

	// A partial body is refused rather than silently treated as false for the missing fields: otherwise
	// "turn replies on" would quietly turn comments and answers off.
	expectStatus(t, env.do("PUT", "/api/v1/me/notification-preferences", `{"email_on_reply":true}`, cookie, csrf), 422)
	expectStatus(t, env.do("PUT", "/api/v1/me/notification-preferences",
		`{"email_on_comment":true,"email_on_answer":true}`, cookie, csrf), 422)
	expectCode(t, env.do("PUT", "/api/v1/me/notification-preferences", `{}`, cookie, csrf), "INVALID_REQUEST")
	// The refused writes changed nothing.
	third := decodePreferences(t, env.getAs("/api/v1/me/notification-preferences", cookie))
	if third != (notificationPreferences{}) {
		t.Fatalf("a rejected write changed the stored flags: %+v", third)
	}
}

func TestNotificationPreferencesArePrivate(t *testing.T) {
	env := newTopicEnv(t)
	other := env.newUser("other")
	ownerCookie, ownerCSRF := env.sessionCookie(env.authorID)
	otherCookie, otherCSRF := env.sessionCookie(other)

	expectStatus(t, env.do("PUT", "/api/v1/me/notification-preferences",
		`{"email_on_comment":true,"email_on_answer":true,"email_on_reply":true}`, ownerCookie, ownerCSRF), 200)

	// Each account reads its own row: there is no id in the path, and the write above must not have leaked
	// into anybody else's settings.
	if preferences := decodePreferences(t, env.getAs("/api/v1/me/notification-preferences", otherCookie)); preferences != (notificationPreferences{}) {
		t.Fatalf("another account's flags changed: %+v", preferences)
	}
	expectStatus(t, env.do("PUT", "/api/v1/me/notification-preferences",
		`{"email_on_comment":false,"email_on_answer":false,"email_on_reply":false}`, otherCookie, otherCSRF), 200)
	if preferences := decodePreferences(t, env.getAs("/api/v1/me/notification-preferences", ownerCookie)); !preferences.EmailOnComment || !preferences.EmailOnAnswer || !preferences.EmailOnReply {
		t.Fatalf("the first account's flags changed with the second's write: %+v", preferences)
	}

	// Session and CSRF are required, like every other private endpoint.
	expectStatus(t, env.get("/api/v1/me/notification-preferences"), 401)
	expectStatus(t, env.do("PUT", "/api/v1/me/notification-preferences",
		`{"email_on_comment":true,"email_on_answer":true,"email_on_reply":true}`, ownerCookie, ""), 403)
	// An unknown field is a client bug, not something to ignore silently.
	expectStatus(t, env.do("PUT", "/api/v1/me/notification-preferences",
		`{"email_on_comment":true,"email_on_answer":true,"email_on_reply":true,"email_on_mention":true}`,
		ownerCookie, ownerCSRF), 400)
}
