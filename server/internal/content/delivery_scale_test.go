package content

import (
	"context"
	"sync"
	"testing"
	"time"
)

// What the earlier delivery tests did not cover: several workers running at once, a worker that died
// holding a lease, and whether one pass drains a batch instead of one message per invocation. The first is
// the property the whole design leans on — SKIP LOCKED plus a lease — and it is the one a fake in-memory
// queue could never prove.

// queueAnswersFor creates one question per asker and has distinct answerers answer each of them, so every
// answer queues a notification for its asker. Returns the number of events queued.
func queueAnswersFor(t *testing.T, env *topicEnv, askers []uint64, answersEach int) int {
	t.Helper()
	queued := 0
	for index, asker := range askers {
		in := env.base()
		in.AuthorID = asker
		in.BoardID = env.qaBoardID
		in.Title = "concurrency check " + itoa(uint64(index))
		in.TagIDs = nil
		topic, err := env.store.CreateTopic(context.Background(), in)
		if err != nil {
			t.Fatal(err)
		}
		// Mail is opt-in: without this the worker would skip every event and prove nothing.
		if err := env.store.SaveNotificationPreferences(context.Background(), asker, notificationPreferences{EmailOnAnswer: true}); err != nil {
			t.Fatal(err)
		}
		for answerIndex := 0; answerIndex < answersEach; answerIndex++ {
			answerer := env.newUser("answerer")
			if _, err := env.answer(topic, answerer, false); err != nil {
				t.Fatal(err)
			}
			queued++
		}
	}
	return queued
}

func TestConcurrentWorkersDoNotDoubleSend(t *testing.T) {
	env := newTopicEnv(t)
	askers := []uint64{env.newUser("asker"), env.newUser("asker"), env.newUser("asker"), env.newUser("asker")}
	const answersEach = 3
	const workers = 4
	queued := queueAnswersFor(t, env, askers, answersEach)

	sender := &fakeSender{}
	// Every worker starts at the same moment, which is what makes a missing lock show up as a duplicate
	// message rather than as a slow test.
	start := make(chan struct{})
	var wg sync.WaitGroup
	reports := make([]DeliveryReport, workers)
	begun := time.Now()
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			report, err := env.store.DeliverNotifications(context.Background(), sender, env.deliveryOptions(time.Now()))
			if err != nil {
				t.Errorf("worker %d failed: %v", index, err)
				return
			}
			reports[index] = report
		}(worker)
	}
	close(start)
	wg.Wait()
	elapsed := time.Since(begun)

	// One coalesced message per asker, and never two: a duplicate would mean two workers claimed the same
	// event, which is the failure the lease exists to prevent.
	messages := sender.sent()
	perRecipient := map[string]int{}
	for _, message := range messages {
		perRecipient[message.To]++
	}
	if len(messages) != len(askers) {
		t.Fatalf("%d messages for %d askers: %+v", len(messages), len(askers), perRecipient)
	}
	for recipient, count := range perRecipient {
		if count != 1 {
			t.Fatalf("recipient %s received %d messages", recipient, count)
		}
	}

	// Every claimed event is accounted for exactly once across the workers.
	claimed := 0
	for _, report := range reports {
		claimed += report.Claimed
	}
	if claimed != queued {
		t.Fatalf("%d events claimed across %d workers, want %d (%+v)", claimed, workers, queued, reports)
	}

	// The queue is empty and the per-event ledger has one sent row per event, with no duplicates.
	var pending, done int
	if err := env.db.QueryRow(`SELECT
 COALESCE(SUM(status='pending'),0), COALESCE(SUM(status='done'),0) FROM outbox_events`).Scan(&pending, &done); err != nil {
		t.Fatal(err)
	}
	if pending != 0 || done != queued {
		t.Fatalf("after the pass: %d pending, %d done, want 0 and %d", pending, done, queued)
	}
	var sentRows, distinctSent int
	if err := env.db.QueryRow(`SELECT COUNT(*), COUNT(DISTINCT recipient_id, event_key, purpose) FROM email_deliveries WHERE status='sent'`).
		Scan(&sentRows, &distinctSent); err != nil {
		t.Fatal(err)
	}
	if sentRows != queued || distinctSent != queued {
		t.Fatalf("%d sent rows (%d distinct) for %d events", sentRows, distinctSent, queued)
	}

	// A coarse observation, not a benchmark: it only shows the pass is batched rather than one round trip
	// per message. The number is machine dependent and is not a capacity claim.
	t.Logf("observation: %d events, %d recipients, %d concurrent workers, one pass, %s wall clock",
		queued, len(messages), workers, elapsed.Round(time.Millisecond))
}

func TestStaleLeaseIsReclaimed(t *testing.T) {
	env := newTopicEnv(t)
	asker := env.newUser("asker")
	queued := queueAnswersFor(t, env, []uint64{asker}, 1)
	if queued != 1 {
		t.Fatalf("expected one event, queued %d", queued)
	}
	// A worker that died between claiming the row and finishing it leaves exactly this behind: a lease in
	// the past. Nothing else may move the row out of `processing`, so this is the recovery path.
	if _, err := env.db.Exec(`UPDATE outbox_events SET status='processing', lease_token='dead-worker',
 lease_until=UTC_TIMESTAMP(6)-INTERVAL 5 MINUTE, attempts=1`); err != nil {
		t.Fatal(err)
	}

	sender := &fakeSender{}
	report, err := env.store.DeliverNotifications(context.Background(), sender, env.deliveryOptions(time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if report.Claimed != 1 || report.Sent != 1 {
		t.Fatalf("a stale lease was not reclaimed: %+v", report)
	}
	if len(sender.sent()) != 1 {
		t.Fatalf("the reclaimed event was not delivered: %+v", sender.sent())
	}
	var status string
	var lease any
	if err := env.db.QueryRow(`SELECT status,lease_token FROM outbox_events ORDER BY id DESC LIMIT 1`).Scan(&status, &lease); err != nil {
		t.Fatal(err)
	}
	if status != "done" || lease != nil {
		t.Fatalf("after reclaiming: status=%q lease=%v", status, lease)
	}

	// A lease that has not expired belongs to a live worker and must be left alone.
	if _, err := env.db.Exec(`UPDATE outbox_events SET status='processing', lease_token='live-worker',
 lease_until=UTC_TIMESTAMP(6)+INTERVAL 5 MINUTE`); err != nil {
		t.Fatal(err)
	}
	before := len(sender.sent())
	if _, err := env.store.DeliverNotifications(context.Background(), sender, env.deliveryOptions(time.Now())); err != nil {
		t.Fatal(err)
	}
	if len(sender.sent()) != before {
		t.Fatalf("an unexpired lease was stolen: %+v", sender.sent())
	}
}

func TestOnePassDrainsABatch(t *testing.T) {
	env := newTopicEnv(t)
	// Six askers with ten answers each: sixty events in one queue. This is not a load test — it is the
	// assertion that one pass takes a batch through the whole pipeline rather than stopping after the first
	// message, and it is what keeps a backlog from draining at one mail per worker round.
	var askers []uint64
	for index := 0; index < 6; index++ {
		askers = append(askers, env.newUser("asker"))
	}
	queued := queueAnswersFor(t, env, askers, 10)
	if queued != 60 {
		t.Fatalf("queued %d events, want 60", queued)
	}

	sender := &fakeSender{}
	begun := time.Now()
	first, err := env.store.DeliverNotifications(context.Background(), sender, env.deliveryOptions(time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	// One pass takes at most `deliveryBatchSize` events: the batch is what bounds how long a single pass can
	// hold its leases, so a backlog drains over several passes rather than in one unbounded claim.
	if first.Claimed != deliveryBatchSize {
		t.Fatalf("the first pass claimed %d events, want the batch bound %d", first.Claimed, deliveryBatchSize)
	}
	var pending int
	if err := env.db.QueryRow(`SELECT COUNT(*) FROM outbox_events WHERE status='pending'`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != queued-deliveryBatchSize {
		t.Fatalf("%d events left after the first pass, want %d", pending, queued-deliveryBatchSize)
	}

	second, err := env.store.DeliverNotifications(context.Background(), sender, env.deliveryOptions(time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(begun)
	if second.Claimed != queued-deliveryBatchSize {
		t.Fatalf("the second pass claimed %d events, want %d", second.Claimed, queued-deliveryBatchSize)
	}
	// Coalescing means six messages across the two passes, not sixty.
	if len(sender.sent()) != len(askers) {
		t.Fatalf("the passes sent %d messages, want %d", len(sender.sent()), len(askers))
	}
	if total := first.Coalesced + second.Coalesced; total != queued-len(askers) {
		t.Fatalf("coalesced %d events in total, want %d", total, queued-len(askers))
	}
	if err := env.db.QueryRow(`SELECT COUNT(*) FROM outbox_events WHERE status='pending'`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("%d events were left pending after two passes", pending)
	}
	t.Logf("observation: %d events coalesced into %d messages over two passes (%d then %d claimed), %s wall clock",
		queued, len(askers), first.Claimed, second.Claimed, elapsed.Round(time.Millisecond))
}
