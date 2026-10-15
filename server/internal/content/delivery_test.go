package content

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"opentalk/server/internal/mailer"
)

// The delivery pass decides what leaves the process, so its tests are about the four questions it asks
// again before sending, plus the two confirmed parameters (coalescing window, hourly cap) and the lease
// that keeps two workers off the same event. The sender is fake, so nothing is mailed; everything else —
// claiming, re-checking, bookkeeping — runs against real MySQL.

type fakeSender struct {
	mu       sync.Mutex
	messages []mailer.Message
	fail     bool
}

func (f *fakeSender) Send(_ context.Context, message mailer.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errors.New("smtp unavailable")
	}
	f.messages = append(f.messages, message)
	return nil
}

func (f *fakeSender) sent() []mailer.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]mailer.Message(nil), f.messages...)
}

// deliveryOptions is the pass configuration a test uses: a fixed "now" and a key so links are deterministic.
func (e *topicEnv) deliveryOptions(now time.Time) DeliveryOptions {
	return DeliveryOptions{
		Now:            now,
		UnsubscribeKey: e.deps.UnsubscribeKey,
		AppOrigin:      "https://forum.example",
	}
}

func TestDeliverySendsOneCoalescedMessagePerTopic(t *testing.T) {
	env := newTopicEnv(t)
	question := env.newQuestion(false)
	answerer := env.newUser("answerer")
	commenter := env.newUser("commenter")
	secondAnswerer := env.newUser("second")
	// Three events for the same recipient about the same topic. Only events addressed to the asker belong
	// in one message: a comment on somebody's answer notifies that answer's author instead, which is why the
	// third event here is a second answer rather than a comment under the first one.
	if _, err := env.answer(question, answerer, false); err != nil {
		t.Fatal(err)
	}
	if _, err := env.comment(NewComment{AuthorID: commenter, TopicID: question}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.answer(question, secondAnswerer, false); err != nil {
		t.Fatal(err)
	}

	sender := &fakeSender{}
	// Mail is opt-in, so the recipient has to have asked for these event types first.
	if err := env.store.SaveNotificationPreferences(context.Background(), env.authorID, notificationPreferences{
		EmailOnAnswer:  true,
		EmailOnComment: true,
	}); err != nil {
		t.Fatal(err)
	}
	report, err := env.store.DeliverNotifications(context.Background(), sender, env.deliveryOptions(time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if report.Claimed != 3 || report.Sent != 1 || report.Coalesced != 2 {
		t.Fatalf("report is %+v, want 3 claimed and 1 coalesced message", report)
	}
	messages := sender.sent()
	if len(messages) != 1 {
		t.Fatalf("sent %d messages, want 1", len(messages))
	}
	message := messages[0]
	if message.To != env.nicknameOf(env.authorID)+"@example.test" {
		// The fixture builds addresses from the nickname, so this also pins down which account was mailed.
		t.Fatalf("message went to %q", message.To)
	}
	if !strings.Contains(message.Body, "https://forum.example/#/topic/"+itoa(question)) {
		t.Fatalf("the message has no link to the topic: %q", message.Body)
	}
	if !strings.Contains(message.Body, "/#/unsubscribe?token=") {
		t.Fatalf("the message has no unsubscribe link: %q", message.Body)
	}
	// No author name anywhere: anonymity cannot leak through mail if mail never names an author.
	for _, name := range []string{env.nicknameOf(answerer), env.nicknameOf(commenter)} {
		if strings.Contains(message.Body, name) || strings.Contains(message.Subject, name) {
			t.Fatalf("the message names an author: %q", message.Body)
		}
	}
	// The queue is drained and every event is recorded as sent exactly once.
	for _, event := range claimedEvents(t, env, question) {
		if event.Status != "done" {
			t.Fatalf("event %d is %q after a successful pass", event.ID, event.Status)
		}
	}
	var sent int
	if err := env.db.QueryRow(`SELECT COUNT(*) FROM email_deliveries WHERE recipient_id=? AND status='sent'`,
		env.authorID).Scan(&sent); err != nil {
		t.Fatal(err)
	}
	if sent != 3 {
		t.Fatalf("%d deliveries recorded, want one per event", sent)
	}

	// A second pass has nothing to do: the events are done, so nothing is sent twice.
	again, err := env.store.DeliverNotifications(context.Background(), sender, env.deliveryOptions(time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if again.Claimed != 0 || len(sender.sent()) != 1 {
		t.Fatalf("the second pass did work: %+v", again)
	}
}

// claimedEvents reads the outbox rows for a topic's content.
func claimedEvents(t *testing.T, env *topicEnv, topicID uint64) []outboxRow {
	t.Helper()
	rows, err := env.db.Query(`SELECT id,event_key,status,COALESCE(recipient_id,0),COALESCE(content_id,0)
 FROM outbox_events WHERE content_id IN (SELECT content_id FROM topics WHERE content_id=?
 UNION SELECT content_id FROM answers WHERE question_id=?
 UNION SELECT content_id FROM comments WHERE topic_id=?) ORDER BY id`, topicID, topicID, topicID)
	if err != nil {
		t.Fatal(err)
	}
	items, err := scanRows(rows, func(rows *sql.Rows) (outboxRow, error) {
		var row outboxRow
		err := rows.Scan(&row.ID, &row.EventKey, &row.Status, &row.RecipientID, &row.ContentID)
		return row, err
	})
	if err != nil {
		t.Fatal(err)
	}
	return items
}

func TestDeliveryRewritesVisibilityBeforeSending(t *testing.T) {
	env := newTopicEnv(t)
	question := env.newQuestion(false)
	answerer := env.newUser("answerer")
	answer, err := env.answer(question, answerer, false)
	if err != nil {
		t.Fatal(err)
	}
	// The content is hidden between the event being written and the pass running — the exact race the
	// outbox exists to survive.
	env.hideContent(t, itoa(answer))

	sender := &fakeSender{}
	report, err := env.store.DeliverNotifications(context.Background(), sender, env.deliveryOptions(time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if report.Sent != 0 || report.Skipped != 1 {
		t.Fatalf("report is %+v, want the group skipped", report)
	}
	if len(sender.sent()) != 0 {
		t.Fatalf("a hidden answer was announced: %+v", sender.sent())
	}
	for _, event := range env.outboxFor(answer) {
		if event.Status == "pending" || event.Status == "processing" {
			t.Fatalf("the event was left to retry forever: %+v", event)
		}
	}
}

func TestDeliveryHonoursPreferencesAndSubscriptions(t *testing.T) {
	env := newTopicEnv(t)
	question := env.newQuestion(false)
	answerer := env.newUser("answerer")
	if _, err := env.answer(question, answerer, false); err != nil {
		t.Fatal(err)
	}
	sender := &fakeSender{}

	// Global preference off (the default) means no mail at all.
	report, err := env.store.DeliverNotifications(context.Background(), sender, env.deliveryOptions(time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if report.Sent != 0 || report.Skipped != 1 || len(sender.sent()) != 0 {
		t.Fatalf("mail went out while the global preference was off: %+v", report)
	}

	// Turning the answer-type flag on is what authorises this message.
	if err := env.store.SaveNotificationPreferences(context.Background(), env.authorID, notificationPreferences{EmailOnAnswer: true}); err != nil {
		t.Fatal(err)
	}
	// The queued row was finished, not retried, so this test queues a fresh event by answering again.
	if _, err := env.answer(question, env.newUser("second"), false); err != nil {
		t.Fatal(err)
	}
	report, err = env.store.DeliverNotifications(context.Background(), sender, env.deliveryOptions(time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if report.Sent != 1 {
		t.Fatalf("an enabled preference did not send: %+v", report)
	}

	// A topic-level "disabled" beats the global flag, and it beats "enabled" too: it is the strongest
	// statement a reader can make about one discussion.
	if err := env.store.SaveTopicSubscription(context.Background(), env.authorID, question, overrideDisabled); err != nil {
		t.Fatal(err)
	}
	if _, err := env.answer(question, env.newUser("third"), false); err != nil {
		t.Fatal(err)
	}
	before := len(sender.sent())
	report, err = env.store.DeliverNotifications(context.Background(), sender, env.deliveryOptions(time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if report.Sent != 0 || len(sender.sent()) != before {
		t.Fatalf("mail went out for a disabled topic: %+v", report)
	}
}

func TestDeliveryDefersAtTheHourlyCap(t *testing.T) {
	env := newTopicEnv(t)
	question := env.newQuestion(false)
	answerer := env.newUser("answerer")
	if _, err := env.answer(question, answerer, false); err != nil {
		t.Fatal(err)
	}
	if err := env.store.SaveNotificationPreferences(context.Background(), env.authorID, notificationPreferences{EmailOnAnswer: true}); err != nil {
		t.Fatal(err)
	}
	// The reader already received the hourly maximum, one minute ago.
	now := time.Now()
	for index := 0; index < DeliveryHourlyLimit; index++ {
		if _, err := env.db.Exec(`INSERT INTO email_deliveries
 (recipient_id,event_key,purpose,status,next_attempt_at,sent_at) VALUES (?,?,?,'sent',?,?)`,
			env.authorID, "prior:"+itoa(uint64(index)), purposeNotification, now.Add(-time.Minute), now.Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
	}

	sender := &fakeSender{}
	report, err := env.store.DeliverNotifications(context.Background(), sender, env.deliveryOptions(now))
	if err != nil {
		t.Fatal(err)
	}
	if report.Deferred != 1 || report.Sent != 0 || len(sender.sent()) != 0 {
		t.Fatalf("report is %+v, want the message deferred", report)
	}
	// Deferral is not a failure: the event stays queued, its attempt count is untouched, and it is due when
	// the window frees (one hour after the oldest message in it).
	var status string
	var attempts int
	var availableAt time.Time
	if err := env.db.QueryRow(`SELECT status,attempts,available_at FROM outbox_events ORDER BY id DESC LIMIT 1`).
		Scan(&status, &attempts, &availableAt); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || attempts != 0 {
		t.Fatalf("a deferred event is status=%q attempts=%d", status, attempts)
	}
	if availableAt.Before(now.Add(50 * time.Minute)) {
		t.Fatalf("deferred until %s, which is not near the end of the window", availableAt)
	}
}

func TestDeliveryRetriesAndGivesUp(t *testing.T) {
	env := newTopicEnv(t)
	question := env.newQuestion(false)
	answerer := env.newUser("answerer")
	if _, err := env.answer(question, answerer, false); err != nil {
		t.Fatal(err)
	}
	if err := env.store.SaveNotificationPreferences(context.Background(), env.authorID, notificationPreferences{EmailOnAnswer: true}); err != nil {
		t.Fatal(err)
	}
	sender := &fakeSender{fail: true}
	// The retry budget is spent across passes, each of which advances "now" past the backoff.
	now := time.Now()
	for attempt := 1; attempt <= deliveryMaxRetries; attempt++ {
		report, err := env.store.DeliverNotifications(context.Background(), sender, env.deliveryOptions(now))
		if err != nil {
			t.Fatal(err)
		}
		if report.Failed != 1 {
			t.Fatalf("attempt %d reported %+v", attempt, report)
		}
		now = now.Add(24 * time.Hour)
	}
	var status string
	var attempts int
	if err := env.db.QueryRow(`SELECT status,attempts FROM outbox_events ORDER BY id DESC LIMIT 1`).Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != "failed" {
		t.Fatalf("after the retry budget the event is %q", status)
	}
	if attempts < deliveryMaxRetries {
		t.Fatalf("attempts is %d after %d passes", attempts, deliveryMaxRetries)
	}
	// The failed attempt is recorded too, so the message is not counted as sent by the hourly cap.
	var sent int
	if err := env.db.QueryRow(`SELECT COUNT(*) FROM email_deliveries WHERE recipient_id=? AND status='sent'`,
		env.authorID).Scan(&sent); err != nil {
		t.Fatal(err)
	}
	if sent != 0 {
		t.Fatalf("%d deliveries were recorded as sent after failures", sent)
	}
	// And a later pass with a working sender does not silently resend: the row is failed, not pending.
	working := &fakeSender{}
	if _, err := env.store.DeliverNotifications(context.Background(), working, env.deliveryOptions(now)); err != nil {
		t.Fatal(err)
	}
	if len(working.sent()) != 0 {
		t.Fatalf("a given-up event was sent anyway: %+v", working.sent())
	}
}
