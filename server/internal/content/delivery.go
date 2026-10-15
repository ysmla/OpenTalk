package content

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"opentalk/server/internal/mailer"
)

// Notification mail delivery: the half of the outbox that leaves the process.
//
// The transaction that recorded an event has long since committed, so everything that could have changed
// since is re-checked here, immediately before sending — that is the point of the outbox rather than a
// fire-and-forget send. Four questions are asked again:
//
//  1. is any of the content still reachable? (hidden or deleted content is not announced);
//  2. does the recipient still exist and have an address?
//  3. has the reader opted out — for this topic, or globally for this event type?
//  4. is this recipient under the hourly mail cap?
//
// The parameters are the ones confirmed for this stage: events about one topic within a ten-minute window
// leave as a single message, and one recipient receives at most ten messages an hour. A message that hits
// the cap is not dropped: its events go back to the queue with an availability time at the moment a slot
// frees, so the notification still arrives, later.
//
// Work is claimed with `FOR UPDATE SKIP LOCKED` plus a lease, so several workers can run at once without
// two of them sending the same event, and a worker that dies mid-send does not strand its batch — the
// lease expires and the rows become claimable again.

// Confirmed delivery parameters (docs/requirements.md, 通知阶段已确认参数).
const (
	// DeliveryWindow is the coalescing window: events about one topic inside it become one message.
	DeliveryWindow = 10 * time.Minute
	// DeliveryHourlyLimit is the per-recipient cap on messages per hour.
	DeliveryHourlyLimit = 10
)

const (
	deliveryLease      = 2 * time.Minute
	deliveryBatchSize  = 50
	deliveryMaxRetries = 5
	// purposeNotification labels the rows this package writes, so the rate limiter counts only mail it is
	// responsible for and does not have to guess at other purposes.
	purposeNotification = "notification"
)

// DeliveryOptions carries what the delivery pass needs beyond the database.
type DeliveryOptions struct {
	Now         time.Time
	LeaseToken  string
	BatchSize   int
	Window      time.Duration
	HourlyLimit int
	MaxRetries  int
	// UnsubscribeKey signs the unsubscribe link. It is the same subkey the API verifies with.
	UnsubscribeKey []byte
	// AppOrigin is the absolute prefix for links in the message, e.g. "https://forum.example".
	AppOrigin string
}

func (o DeliveryOptions) withDefaults() DeliveryOptions {
	if o.Now.IsZero() {
		o.Now = time.Now().UTC()
	}
	if o.LeaseToken == "" {
		o.LeaseToken = newLeaseToken()
	}
	if o.BatchSize <= 0 {
		o.BatchSize = deliveryBatchSize
	}
	if o.Window <= 0 {
		o.Window = DeliveryWindow
	}
	if o.HourlyLimit <= 0 {
		o.HourlyLimit = DeliveryHourlyLimit
	}
	if o.MaxRetries <= 0 {
		o.MaxRetries = deliveryMaxRetries
	}
	o.AppOrigin = strings.TrimRight(o.AppOrigin, "/")
	return o
}

// DeliveryReport summarises one pass. Counts are per message, except Claimed which is per event.
type DeliveryReport struct {
	Claimed  int
	Sent     int
	Skipped  int
	Failed   int
	Deferred int
	// Coalesced counts events that went out inside somebody else's message.
	Coalesced int
}

// PendingNotificationCount reports how many notification events are waiting to go out. The worker uses it
// for its dry run and its configuration summary, and an operator uses those to tell "nothing to send" from
// "sending is broken".
func (s Store) PendingNotificationCount(ctx context.Context) (int, error) {
	var pending int
	err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM outbox_events WHERE status='pending' AND event_type IN (?,?,?)`,
		eventNewComment, eventNewAnswer, eventNewReply).Scan(&pending)
	return pending, err
}

// outboxEvent is one claimed event.
type outboxEvent struct {
	ID          uint64
	EventKey    string
	EventType   string
	ContentID   uint64
	RecipientID uint64
	Attempts    int
}

// deliveryGroup is one message: everything queued for one recipient about one topic.
type deliveryGroup struct {
	RecipientID uint64
	TopicID     uint64
	Events      []outboxEvent
}

// DeliverNotifications runs one delivery pass. A per-message failure is recorded on the row and does not
// abort the pass, because one undeliverable address must not stop everybody else's mail; a database failure
// does abort, because continuing would mean guessing about the queue.
func (s Store) DeliverNotifications(ctx context.Context, sender mailer.Sender, options DeliveryOptions) (DeliveryReport, error) {
	var report DeliveryReport
	options = options.withDefaults()
	if sender == nil {
		return report, errors.New("delivery: no mail sender configured")
	}

	// A worker that died holding a lease must not strand its batch: the lease expires and the rows are
	// claimable again. This is the only way a row leaves 'processing' without a decision.
	if _, err := s.DB.ExecContext(ctx, `UPDATE outbox_events
 SET status='pending', lease_token=NULL, lease_until=NULL
 WHERE status='processing' AND (lease_until IS NULL OR lease_until < ?)`, options.Now); err != nil {
		return report, err
	}

	claimed, err := s.claimOutbox(ctx, options)
	if err != nil {
		return report, err
	}
	report.Claimed = len(claimed)
	if len(claimed) == 0 {
		return report, nil
	}

	groups, err := s.groupForDelivery(ctx, claimed)
	if err != nil {
		return report, err
	}
	for _, group := range groups {
		outcome, err := s.deliverGroup(ctx, sender, group, options)
		if err != nil {
			return report, err
		}
		switch outcome {
		case outcomeSent:
			report.Sent++
			report.Coalesced += len(group.Events) - 1
		case outcomeSkipped:
			report.Skipped++
		case outcomeDeferred:
			report.Deferred++
		default:
			report.Failed++
		}
	}
	return report, nil
}

// claimOutbox takes a batch of due events under a lease. SKIP LOCKED is what makes concurrent workers safe:
// a row another worker is already claiming is skipped rather than waited for.
func (s Store) claimOutbox(ctx context.Context, options DeliveryOptions) ([]outboxEvent, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, `SELECT id,event_key,event_type,COALESCE(content_id,0),recipient_id,attempts
 FROM outbox_events
 WHERE status='pending' AND available_at<=?
 ORDER BY id LIMIT ? FOR UPDATE SKIP LOCKED`, options.Now, options.BatchSize)
	if err != nil {
		return nil, err
	}
	claimed, err := scanRows(rows, func(rows *sql.Rows) (outboxEvent, error) {
		var event outboxEvent
		var recipient sql.NullInt64
		if err := rows.Scan(&event.ID, &event.EventKey, &event.EventType, &event.ContentID, &recipient, &event.Attempts); err != nil {
			return event, err
		}
		// An event with no recipient cannot be delivered and must not sit in the queue forever.
		event.RecipientID = uint64(recipient.Int64)
		return event, nil
	})
	if err != nil {
		return nil, err
	}
	if len(claimed) == 0 {
		return nil, nil
	}
	ids := make([]uint64, 0, len(claimed))
	for _, event := range claimed {
		ids = append(ids, event.ID)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE outbox_events
 SET status='processing', lease_until=?, lease_token=?
 WHERE id IN (`+placeholders(len(ids))+`)`,
		append([]any{options.Now.Add(deliveryLease), options.LeaseToken}, toAny(ids)...)...); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return claimed, nil
}

// groupForDelivery folds the claimed events into one group per (recipient, topic), which is what a single
// message is about.
func (s Store) groupForDelivery(ctx context.Context, claimed []outboxEvent) ([]deliveryGroup, error) {
	// Events without a recipient are failed outright; there is nothing to decide and nothing to send.
	deliverable := make([]outboxEvent, 0, len(claimed))
	orphans := make([]uint64, 0)
	for _, event := range claimed {
		if event.RecipientID == 0 {
			orphans = append(orphans, event.ID)
			continue
		}
		deliverable = append(deliverable, event)
	}
	if len(orphans) > 0 {
		if _, err := s.DB.ExecContext(ctx, `UPDATE outbox_events
 SET status='failed', attempts=attempts+1, lease_token=NULL, lease_until=NULL
 WHERE id IN (`+placeholders(len(orphans))+`)`, toAny(orphans)...); err != nil {
			return nil, err
		}
	}
	if len(deliverable) == 0 {
		return nil, nil
	}

	contentIDs := make([]uint64, 0, len(deliverable))
	for _, event := range deliverable {
		if event.ContentID != 0 {
			contentIDs = append(contentIDs, event.ContentID)
		}
	}
	topics, err := s.rootTopicsOf(ctx, contentIDs)
	if err != nil {
		return nil, err
	}

	// Groups are collected through pointers so that appending to the order slice cannot move the group a map
	// entry already points at — the alias bug this package has hit before. The finished slice is a copy.
	byGroup := make(map[string]*deliveryGroup)
	order := make([]*deliveryGroup, 0)
	for _, event := range deliverable {
		topicID := topics[event.ContentID]
		key := fmt.Sprintf("%d:%d", event.RecipientID, topicID)
		group, exists := byGroup[key]
		if !exists {
			group = &deliveryGroup{RecipientID: event.RecipientID, TopicID: topicID}
			byGroup[key] = group
			order = append(order, group)
		}
		group.Events = append(group.Events, event)
	}
	groups := make([]deliveryGroup, 0, len(order))
	for _, group := range order {
		groups = append(groups, *group)
	}
	return groups, nil
}

// rootTopicsOf resolves the discussion each content row belongs to, in one query. Unlike visibility this is
// a single-valued relation — every row has exactly one root — so a COALESCE is right here.
func (s Store) rootTopicsOf(ctx context.Context, contentIDs []uint64) (map[uint64]uint64, error) {
	topics := make(map[uint64]uint64, len(contentIDs))
	unique := dedupe(contentIDs)
	if len(unique) == 0 {
		return topics, nil
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT c.id,COALESCE(t.content_id,a.question_id,cm.topic_id)
 FROM contents c
 LEFT JOIN topics t ON t.content_id=c.id
 LEFT JOIN answers a ON a.content_id=c.id
 LEFT JOIN comments cm ON cm.content_id=c.id
 WHERE c.id IN (`+placeholders(len(unique))+`)`, toAny(unique)...)
	if err != nil {
		return nil, err
	}
	_, err = scanRows(rows, func(rows *sql.Rows) (struct{}, error) {
		var contentID uint64
		var topicID sql.NullInt64
		if err := rows.Scan(&contentID, &topicID); err != nil {
			return struct{}{}, err
		}
		if topicID.Valid {
			topics[contentID] = uint64(topicID.Int64)
		}
		return struct{}{}, nil
	})
	if err != nil {
		return nil, err
	}
	return topics, nil
}

// Delivery outcomes for one group.
const (
	outcomeSent     = "sent"
	outcomeSkipped  = "skipped"
	outcomeDeferred = "deferred"
	outcomeFailed   = "failed"
)

// deliverGroup re-checks everything, then sends at most one message and records the result.
func (s Store) deliverGroup(ctx context.Context, sender mailer.Sender, group deliveryGroup, options DeliveryOptions) (string, error) {
	ids := make([]uint64, 0, len(group.Events))
	contentIDs := make([]uint64, 0, len(group.Events))
	for _, event := range group.Events {
		ids = append(ids, event.ID)
		if event.ContentID != 0 {
			contentIDs = append(contentIDs, event.ContentID)
		}
	}

	// 1. Reachability, asked now rather than trusted from when the event was queued. A group whose content
	// has all become unreachable is finished: there is nothing left to announce, and the row must not
	// retry forever.
	visible := map[uint64]bool{}
	if len(contentIDs) > 0 {
		var err error
		visible, err = visibleContentIDs(ctx, s.DB, contentIDs)
		if err != nil {
			return "", err
		}
	}
	sendable := make([]outboxEvent, 0, len(group.Events))
	for _, event := range group.Events {
		if event.ContentID != 0 && visible[event.ContentID] {
			sendable = append(sendable, event)
		}
	}
	if len(sendable) == 0 {
		return outcomeSkipped, s.finishOutbox(ctx, ids, "done", options.Now)
	}

	// 2. The address, re-read because an account may be gone since the event was written.
	var address string
	err := s.DB.QueryRowContext(ctx, `SELECT email_normalized FROM users WHERE id=?`, group.RecipientID).Scan(&address)
	if errors.Is(err, sql.ErrNoRows) {
		return outcomeFailed, s.finishOutbox(ctx, ids, "failed", options.Now)
	}
	if err != nil {
		return "", err
	}

	// 3. The reader's choices. A topic-level `disabled` ends the matter; otherwise the global preference
	// for the event's type decides, and `enabled` overrides a global "off" for this one topic. A coalesced
	// message goes out when at least one of its events is allowed, and drops the ones that are not.
	override, err := s.TopicSubscription(ctx, group.RecipientID, group.TopicID)
	if err != nil {
		return "", err
	}
	if override == overrideDisabled {
		return outcomeSkipped, s.finishOutbox(ctx, ids, "done", options.Now)
	}
	preferences, err := s.NotificationPreferences(ctx, group.RecipientID)
	if err != nil {
		return "", err
	}
	allowed := make([]outboxEvent, 0, len(sendable))
	for _, event := range sendable {
		if override == overrideEnabled || preferenceAllows(preferences, event.EventType) {
			allowed = append(allowed, event)
		}
	}
	if len(allowed) == 0 {
		// The reader has email off for these event types. The events are done — they must not be retried —
		// and the in-site notification they already have is unaffected.
		return outcomeSkipped, s.finishOutbox(ctx, ids, "done", options.Now)
	}

	// 4. The hourly cap. Hitting it defers rather than drops: the events go back to the queue, available at
	// the moment a slot frees, so the mail still arrives once the hour turns over.
	sent, oldest, err := s.recentDeliveryCount(ctx, group.RecipientID, options.Now)
	if err != nil {
		return "", err
	}
	if sent >= options.HourlyLimit {
		return outcomeDeferred, s.deferOutbox(ctx, ids, oldest.Add(time.Hour))
	}

	// 5. Compose and send. The message names the topic and links to it, and carries no author name at all:
	// an anonymous author therefore cannot leak through mail, because mail never mentions an author.
	title, err := s.topicTitle(ctx, group.TopicID)
	if err != nil {
		return "", err
	}
	token, err := mintUnsubscribeToken(options.UnsubscribeKey, group.RecipientID, group.TopicID, options.Now)
	if err != nil {
		return "", err
	}
	subject, body := composeNotification(allowed, title, group.TopicID, options.AppOrigin, token)
	if err := sender.Send(ctx, mailer.Message{To: address, Subject: subject, Body: body}); err != nil {
		// A delivery failure never touches the content: the answer or comment is already committed. The row
		// is retried with a growing delay, and given up on after the retry budget is spent so one dead
		// mailbox cannot keep a worker busy forever.
		status := "pending"
		availableAt := options.Now.Add(retryDelay(group.attempts()+1, options.Window))
		if group.attempts()+1 >= options.MaxRetries {
			status = "failed"
		}
		if err := s.recordDeliveries(ctx, allowed, group.RecipientID, group.TopicID, "failed", options.Now); err != nil {
			return "", err
		}
		return outcomeFailed, s.finishOutbox(ctx, ids, status, availableAt)
	}
	if err := s.recordDeliveries(ctx, allowed, group.RecipientID, group.TopicID, "sent", options.Now); err != nil {
		return "", err
	}
	if err := s.finishOutbox(ctx, ids, "done", options.Now); err != nil {
		return "", err
	}
	return outcomeSent, nil
}

// attempts is the highest attempt count in the group, so a message that failed once is retried on the same
// schedule as its events.
func (g deliveryGroup) attempts() int {
	highest := 0
	for _, event := range g.Events {
		if event.Attempts > highest {
			highest = event.Attempts
		}
	}
	return highest
}

// preferenceAllows maps an event type onto the global flag the reader set.
func preferenceAllows(preferences notificationPreferences, eventType string) bool {
	switch eventType {
	case eventNewAnswer:
		return preferences.EmailOnAnswer
	case eventNewReply:
		return preferences.EmailOnReply
	case eventNewComment:
		return preferences.EmailOnComment
	default:
		// An event type this version does not know is not mailed: guessing which switch authorises it would
		// be inventing consent.
		return false
	}
}

// recentDeliveryCount reports how many messages this recipient already received in the last hour, and when
// the oldest of them was sent — the moment the window starts to free up.
func (s Store) recentDeliveryCount(ctx context.Context, recipientID uint64, now time.Time) (int, time.Time, error) {
	since := now.Add(-time.Hour)
	var count int
	var oldest sql.NullTime
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*),MIN(sent_at) FROM email_deliveries
 WHERE recipient_id=? AND purpose=? AND status='sent' AND sent_at>=?`,
		recipientID, purposeNotification, since).Scan(&count, &oldest); err != nil {
		return 0, time.Time{}, err
	}
	return count, oldest.Time, nil
}

// retryDelay backs off with the attempt count, starting from the coalescing window so retries are not
// faster than the batching they belong to.
func retryDelay(attempt int, window time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 6 {
		attempt = 6
	}
	return window * time.Duration(1<<(attempt-1))
}

// finishOutbox moves a group's rows to their final state and clears the lease.
func (s Store) finishOutbox(ctx context.Context, ids []uint64, status string, availableAt time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE outbox_events
 SET status=?, available_at=?, attempts=attempts+1, lease_token=NULL, lease_until=NULL
 WHERE id IN (`+placeholders(len(ids))+`)`,
		append([]any{status, availableAt}, toAny(ids)...)...)
	return err
}

// deferOutbox puts a group back in the queue without spending a retry: the reader is over the hourly cap,
// which is a reason to wait, not a failure.
func (s Store) deferOutbox(ctx context.Context, ids []uint64, availableAt time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE outbox_events
 SET status='pending', available_at=?, lease_token=NULL, lease_until=NULL
 WHERE id IN (`+placeholders(len(ids))+`)`,
		append([]any{availableAt}, toAny(ids)...)...)
	return err
}

// recordDeliveries writes the per-event record the hourly cap counts and the deletion path cancels. The
// unique key (recipient, event_key, purpose) keeps a retried pass from double counting.
func (s Store) recordDeliveries(ctx context.Context, events []outboxEvent, recipientID, topicID uint64, status string, now time.Time) error {
	for _, event := range events {
		var sentAt any
		if status == "sent" {
			sentAt = now
		}
		if _, err := s.DB.ExecContext(ctx, `INSERT INTO email_deliveries
 (recipient_id,event_key,purpose,content_id,status,next_attempt_at,sent_at)
VALUES (?,?,?,?,?,?,?) AS incoming
ON DUPLICATE KEY UPDATE status=incoming.status, sent_at=incoming.sent_at,
 next_attempt_at=incoming.next_attempt_at, attempts=attempts+1`,
			recipientID, event.EventKey, purposeNotification, nullZero(event.ContentID), status, now, sentAt); err != nil {
			return err
		}
	}
	return nil
}

// topicTitle reads the discussion's title, which the message names so the reader knows what it is about.
func (s Store) topicTitle(ctx context.Context, topicID uint64) (string, error) {
	var title sql.NullString
	if err := s.DB.QueryRowContext(ctx, `SELECT title FROM topics WHERE content_id=?`, topicID).Scan(&title); err != nil {
		return "", err
	}
	return title.String, nil
}

// composeNotification is the message text. It is plain text on purpose: no markup means nothing to render
// wrongly, and the two links are the only things a mail client has to understand.
func composeNotification(events []outboxEvent, title string, topicID uint64, origin, token string) (string, string) {
	subject := notificationSummary(events[0].EventType)
	if len(events) > 1 {
		subject = "你关注的主题有新的动态"
	}
	var body strings.Builder
	body.WriteString("你好，\r\n\r\n")
	for _, event := range events {
		body.WriteString("- " + notificationSummary(event.EventType) + "\r\n")
	}
	body.WriteString("\r\n主题：" + title + "\r\n")
	body.WriteString("查看：" + origin + "/#/topic/" + strconv.FormatUint(topicID, 10) + "\r\n\r\n")
	if token != "" {
		body.WriteString("不再接收这个主题的邮件：" + origin + "/#/unsubscribe?token=" + token + "\r\n\r\n")
	}
	body.WriteString("（这是一封自动发送的通知邮件，请勿直接回复。）\r\n")
	return subject, body.String()
}

// nullZero keeps the nullable content_id nullable instead of writing a zero that would violate the
// foreign key.
func nullZero(value uint64) any {
	if value == 0 {
		return nil
	}
	return value
}

// newLeaseToken identifies one worker's claim. It is random per pass so a stale lease cannot be mistaken
// for a live one, and it fits the column's CHAR(36).
func newLeaseToken() string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		// A clock-based fallback is still unique per pass, and the lease is only ever compared with itself.
		return "lease-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return hex.EncodeToString(buffer)
}
