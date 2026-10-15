package content

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
)

// Notifications are written where the content is written, in the same transaction, and delivered later.
//
// That split is the whole point of the outbox. An SMTP outage, a slow worker or a crash between the two
// must not roll back the answer or the comment the author just published — requirements puts it plainly:
// a delivery failure never undoes posting. So the transaction only records *that* an event happened and
// who should hear about it; anything that leaves the process (email) reads that record afterwards, and
// re-checks visibility, identity display and preferences at that point rather than trusting this moment.
//
// Two tables carry it:
//
//   - `notifications` is the in-site list, one row per recipient per event;
//   - `outbox_events` is the durable handoff to delivery, one row per recipient per event.
//
// Both have a unique key over the event key, so recording the same event twice is a no-op instead of a
// second notification. The key is derived only from the content and the recipient — never from a
// timestamp or a counter — which is what makes a retried transaction idempotent.

// Notification event types, as the contract's `Notification.event_type` enum spells them.
const (
	eventNewComment = "new_comment"
	eventNewAnswer  = "new_answer"
	eventNewReply   = "new_reply"
)

// eventKey is the de-duplication key of one event for one recipient. It names both the content and the
// recipient, because the same comment legitimately notifies the topic author *and* the answer author:
// those are two events, not one.
//
// The key is ascii and bounded well below the column's 100 characters (three ids of at most 20 digits).
func eventKey(eventType string, contentID, recipientID uint64) string {
	return eventType + ":" + strconv.FormatUint(contentID, 10) + ":u" + strconv.FormatUint(recipientID, 10)
}

// notificationSummary is the display text of an in-site notification, and it is a function of the event
// type alone.
//
// That is deliberate. Nothing about the content's text is stored with the notification, so nothing that
// is later hidden or deleted can leak through a list row written before the change; the actor is attached
// at read time, under the same anonymity rules as every other read. The wording also never names the
// actor, so an anonymous answer stays untraceable through the notification that announced it.
func notificationSummary(eventType string) string {
	switch eventType {
	case eventNewAnswer:
		return "你的问题有了新的回答"
	case eventNewReply:
		return "你的评论收到了新的回复"
	case eventNewComment:
		return "你发布的内容有了新的评论"
	default:
		// A type this version does not know (an administrator action from a later one) still lists, with
		// wording that promises nothing about the content.
		return "你有一条新的通知"
	}
}

// notifyRecipients records one event for every recipient, in the caller's transaction.
//
// Two rules live here rather than at the call sites. The actor is dropped, because nobody is told about
// their own action — dropped rather than skipped while inserting, so an event whose only recipient is the
// actor writes nothing at all. And a recipient that is already recorded is left alone, because the unique
// keys make the event idempotent: a retried write must not produce a second notification.
//
// A notification that survived a rolled-back answer would announce something that does not exist, which
// is why this takes the transaction rather than the pool.
func notifyRecipients(ctx context.Context, tx *sql.Tx, eventType string, contentID, actorID uint64, recipients []uint64) error {
	filtered := make([]uint64, 0, len(recipients))
	for _, recipient := range dedupe(recipients) {
		if recipient != 0 && recipient != actorID {
			filtered = append(filtered, recipient)
		}
	}
	if len(filtered) == 0 {
		return nil
	}
	for _, recipient := range filtered {
		key := eventKey(eventType, contentID, recipient)
		if _, err := tx.ExecContext(ctx,
			`INSERT IGNORE INTO notifications (recipient_id,event_key,event_type,content_id) VALUES (?,?,?,?)`,
			recipient, key, eventType, contentID); err != nil {
			return fmt.Errorf("cannot record in-site notification: %w", err)
		}
		// `available_at` is now: when the mail actually goes out, and whether several events are coalesced
		// into one message, is the worker's decision and belongs to the parameters that still have to be
		// confirmed before notification mail is implemented.
		if _, err := tx.ExecContext(ctx,
			`INSERT IGNORE INTO outbox_events (event_key,event_type,content_id,recipient_id,status,available_at)
VALUES (?,?,?,?,'pending',UTC_TIMESTAMP(6))`,
			key, eventType, contentID, recipient); err != nil {
			return fmt.Errorf("cannot queue delivery event: %w", err)
		}
	}
	return nil
}

// voidPendingNotifications drops the queued delivery of everything the deleted rows would have announced.
//
// requirements 42 asks deletion to take effect on the notifications that have not gone out yet, without
// waiting for the cleanup task. Pending outbox rows are deleted outright: a queue entry announcing content
// that no longer exists has nothing left to say. Rows that a worker has already leased are left alone —
// cancelling a send that is in flight is a race — because the worker re-checks visibility immediately
// before sending and drops anything that has become unreachable.
//
// In-site notifications are deliberately *not* deleted here: they are the recipient's own history, and the
// reader renders them against the content's current visibility (the body is never copied into them, so
// nothing hidden can leak through the list).
func voidPendingNotifications(ctx context.Context, tx *sql.Tx, contentIDs []uint64) error {
	if len(contentIDs) == 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM outbox_events WHERE status='pending' AND content_id IN (`+placeholders(len(contentIDs))+`)`,
		toAny(contentIDs)...); err != nil {
		return fmt.Errorf("cannot void pending delivery events: %w", err)
	}
	// A delivery that a worker has already queued as a concrete message is cancelled for the same reason,
	// but only while it is still pending: `processing` means a send may be in flight.
	if _, err := tx.ExecContext(ctx,
		`UPDATE email_deliveries SET status='cancelled' WHERE status='pending' AND content_id IN (`+placeholders(len(contentIDs))+`)`,
		toAny(contentIDs)...); err != nil {
		return fmt.Errorf("cannot cancel pending deliveries: %w", err)
	}
	return nil
}
