package content

import (
	"context"
	"database/sql"
	"testing"
)

// Notifications and the outbox are exercised against real MySQL like everything else here: the rules that
// matter are enforced by unique indexes and by sitting inside the same transaction as the content, and
// neither of those exists in a fake.

type notificationRow struct {
	ID        uint64
	EventType string
	EventKey  string
	ContentID uint64
}

type outboxRow struct {
	ID          uint64
	EventKey    string
	Status      string
	RecipientID uint64
	ContentID   uint64
}

func (e *topicEnv) notificationsFor(recipient uint64) []notificationRow {
	e.t.Helper()
	rows, err := e.db.Query(`SELECT id,event_type,event_key,content_id FROM notifications
 WHERE recipient_id=? ORDER BY id`, recipient)
	if err != nil {
		e.t.Fatal(err)
	}
	items, err := scanRows(rows, func(rows *sql.Rows) (notificationRow, error) {
		var row notificationRow
		err := rows.Scan(&row.ID, &row.EventType, &row.EventKey, &row.ContentID)
		return row, err
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return items
}

func (e *topicEnv) outboxFor(contentID uint64) []outboxRow {
	e.t.Helper()
	rows, err := e.db.Query(`SELECT id,event_key,status,COALESCE(recipient_id,0),COALESCE(content_id,0)
 FROM outbox_events WHERE content_id=? ORDER BY id`, contentID)
	if err != nil {
		e.t.Fatal(err)
	}
	items, err := scanRows(rows, func(rows *sql.Rows) (outboxRow, error) {
		var row outboxRow
		err := rows.Scan(&row.ID, &row.EventKey, &row.Status, &row.RecipientID, &row.ContentID)
		return row, err
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return items
}

func TestAnswerNotifiesTheAsker(t *testing.T) {
	env := newTopicEnv(t)
	question := env.newQuestion(false)
	answerer := env.newUser("answerer")

	answer, err := env.answer(question, answerer, false)
	if err != nil {
		t.Fatal(err)
	}
	notifications := env.notificationsFor(env.authorID)
	if len(notifications) != 1 {
		t.Fatalf("the asker received %d notifications, want 1: %+v", len(notifications), notifications)
	}
	if notifications[0].EventType != eventNewAnswer || notifications[0].ContentID != answer {
		t.Fatalf("notification is wrong: %+v", notifications[0])
	}
	// The in-site row and the queued delivery are recorded together, so a later mail failure cannot undo
	// the answer and a crash here cannot lose the notification.
	outbox := env.outboxFor(answer)
	if len(outbox) != 1 || outbox[0].Status != "pending" || outbox[0].RecipientID != env.authorID {
		t.Fatalf("outbox row is wrong: %+v", outbox)
	}
	// The answerer is not told about their own answer.
	if own := env.notificationsFor(answerer); len(own) != 0 {
		t.Fatalf("the author was notified about their own answer: %+v", own)
	}
}

func TestSelfAnswerNotifiesNobody(t *testing.T) {
	env := newTopicEnv(t)
	question := env.newQuestion(false)
	answer, err := env.answer(question, env.authorID, false)
	if err != nil {
		t.Fatal(err)
	}
	if notifications := env.notificationsFor(env.authorID); len(notifications) != 0 {
		t.Fatalf("the asker was notified about their own answer: %+v", notifications)
	}
	// Nothing at all is queued: the actor is filtered before the insert, not skipped during it.
	if outbox := env.outboxFor(answer); len(outbox) != 0 {
		t.Fatalf("a self-answer queued delivery: %+v", outbox)
	}
}

func TestCommentNotificationsFollowTheTarget(t *testing.T) {
	env := newTopicEnv(t)
	question := env.newQuestion(false) // asked by env.authorID
	answerer := env.newUser("answerer")
	answer, err := env.answer(question, answerer, false)
	if err != nil {
		t.Fatal(err)
	}
	commenter := env.newUser("commenter")

	// A comment on the topic addresses the topic author.
	topicComment, err := env.comment(NewComment{AuthorID: commenter, TopicID: question})
	if err != nil {
		t.Fatal(err)
	}
	askerNotifications := env.notificationsFor(env.authorID)
	if len(askerNotifications) != 2 {
		// one for the answer, one for the comment
		t.Fatalf("the asker received %d notifications, want 2: %+v", len(askerNotifications), askerNotifications)
	}
	if last := askerNotifications[len(askerNotifications)-1]; last.EventType != eventNewComment || last.ContentID != topicComment {
		t.Fatalf("comment notification is wrong: %+v", last)
	}

	// A comment on an answer addresses that answer's author, not the topic author.
	answerComment, err := env.comment(NewComment{AuthorID: commenter, TopicID: question, AnswerID: answer})
	if err != nil {
		t.Fatal(err)
	}
	answererNotifications := env.notificationsFor(answerer)
	if len(answererNotifications) != 1 || answererNotifications[0].ContentID != answerComment {
		t.Fatalf("the answerer received %+v, want only the answer comment", answererNotifications)
	}
	if len(env.notificationsFor(env.authorID)) != 2 {
		t.Fatalf("an answer comment reached the topic author")
	}

	// A reply addresses the author of the comment it answers.
	replier := env.newUser("replier")
	reply, err := env.comment(NewComment{AuthorID: replier, TopicID: question, AnswerID: answer, ReplyToCommentID: answerComment})
	if err != nil {
		t.Fatal(err)
	}
	afterReply := env.notificationsFor(commenter)
	if len(afterReply) != 1 {
		t.Fatalf("the comment author received %+v, want only the reply", afterReply)
	}
	last := afterReply[0]
	if last.EventType != eventNewReply || last.ContentID != reply {
		t.Fatalf("reply notification is wrong: %+v", last)
	}
	// The reply addresses the comment's author, not the answer's author: the latter already heard about the
	// comment itself, and telling them again would be the duplicate the requirement forbids.
	if notifications := env.notificationsFor(answerer); len(notifications) != 1 {
		t.Fatalf("a reply reached the answer author: %+v", notifications)
	}
	// The reply notifies exactly one person: each level above already heard about its own content.
	if notifications := env.notificationsFor(env.authorID); len(notifications) != 2 {
		t.Fatalf("a reply reached the topic author: %+v", notifications)
	}
}

func TestNotificationsAreIdempotent(t *testing.T) {
	env := newTopicEnv(t)
	question := env.newQuestion(false)
	answerer := env.newUser("answerer")
	answer, err := env.answer(question, answerer, false)
	if err != nil {
		t.Fatal(err)
	}
	// Re-recording the same event must not produce a second notification: the unique key is what makes a
	// retried transaction idempotent, and this assertion is what keeps it that way.
	tx, err := env.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := notifyRecipients(context.Background(), tx, eventNewAnswer, answer, answerer, []uint64{env.authorID}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if notifications := env.notificationsFor(env.authorID); len(notifications) != 1 {
		t.Fatalf("a repeated event produced %d notifications", len(notifications))
	}
	if outbox := env.outboxFor(answer); len(outbox) != 1 {
		t.Fatalf("a repeated event produced %d outbox rows", len(outbox))
	}
}

func TestDeleteVoidsPendingNotifications(t *testing.T) {
	env := newTopicEnv(t)
	question := env.newQuestion(false)
	answerer := env.newUser("answerer")
	answer, err := env.answer(question, answerer, false)
	if err != nil {
		t.Fatal(err)
	}
	// A delivery a worker has already leased must survive: cancelling a send in flight is a race, and the
	// worker re-checks visibility immediately before sending anyway.
	if _, err := env.db.Exec(`INSERT INTO outbox_events (event_key,event_type,content_id,recipient_id,status,available_at)
 VALUES (?,?,?,?,'processing',UTC_TIMESTAMP(6))`, "leased:"+itoa(answer), eventNewAnswer, answer, env.authorID); err != nil {
		t.Fatal(err)
	}
	// A concrete message the worker already queued is cancelled rather than sent.
	if _, err := env.db.Exec(`INSERT INTO email_deliveries (recipient_id,event_key,purpose,content_id,status,next_attempt_at)
 VALUES (?,?,?,?,'pending',UTC_TIMESTAMP(6))`, env.authorID, "mail:"+itoa(answer), "new_answer", answer); err != nil {
		t.Fatal(err)
	}

	env.deleteContent(answer)

	pending, leased := 0, 0
	for _, row := range env.outboxFor(answer) {
		if row.Status == "pending" {
			pending++
		}
		if row.Status == "processing" {
			leased++
		}
	}
	if pending != 0 {
		t.Fatalf("%d pending delivery events survived the deletion", pending)
	}
	if leased != 1 {
		t.Fatalf("the leased row was removed: %+v", env.outboxFor(answer))
	}
	var status string
	if err := env.db.QueryRow(`SELECT status FROM email_deliveries WHERE content_id=?`, answer).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "cancelled" {
		t.Fatalf("pending delivery status is %q, want cancelled", status)
	}
	// The in-site notification is the recipient's own history and is kept: the body was never copied into
	// it, so nothing hidden can leak through the list, and the reader renders it against live visibility.
	if notifications := env.notificationsFor(env.authorID); len(notifications) != 1 {
		t.Fatalf("the in-site notification was removed: %+v", notifications)
	}
}

func TestDeleteQuestionVoidsItsAnswersNotifications(t *testing.T) {
	env := newTopicEnv(t)
	question := env.newQuestion(false)
	first, err := env.answer(question, env.newUser("answerer"), false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := env.answer(question, env.newUser("second"), false)
	if err != nil {
		t.Fatal(err)
	}
	// Deleting the question cascades to both answers, so both queued announcements are voided in the same
	// transaction — the cascade is computed, not assumed.
	env.deleteContent(question)
	for _, answer := range []uint64{first, second} {
		for _, row := range env.outboxFor(answer) {
			if row.Status == "pending" {
				t.Fatalf("answer %d still has a pending delivery: %+v", answer, row)
			}
		}
	}
}
