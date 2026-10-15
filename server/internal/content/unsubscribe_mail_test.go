package content

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"opentalk/server/internal/mailer"
)

// The unsubscribe link is only useful if the token inside a *delivered* message works. The unit tests mint
// a token and present it, which proves the format; they do not prove that what the worker puts in the mail
// is what the endpoint accepts. So this test delivers through the real mailer over a real SMTP socket to a
// sink running in this process, takes the token out of the bytes that were actually sent, and presents it
// to the real handler.
//
// No credentials and no third party are involved: the sink speaks just enough SMTP for net/smtp, and the
// delivery is unauthenticated by design (`Security: none`, no username), which the mailer allows.

type smtpSink struct {
	listener net.Listener
	messages chan string
	wg       sync.WaitGroup
}

func startSMTPSink(t *testing.T) *smtpSink {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sink := &smtpSink{listener: listener, messages: make(chan string, 8)}
	sink.wg.Add(1)
	go func() {
		defer sink.wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			sink.wg.Add(1)
			go func() {
				defer sink.wg.Done()
				sink.serve(conn)
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		sink.wg.Wait()
	})
	return sink
}

func (s *smtpSink) port() int { return s.listener.Addr().(*net.TCPAddr).Port }

// serve is a deliberately minimal SMTP server: greeting, EHLO, envelope, DATA, QUIT. It advertises no
// extensions, so the client neither authenticates nor upgrades the connection.
func (s *smtpSink) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	reader := bufio.NewReader(conn)
	reply := func(text string) { _, _ = fmt.Fprintf(conn, "%s\r\n", text) }
	reply("220 sink ready")
	inData := false
	var data strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if inData {
			if trimmed == "." {
				inData = false
				select {
				case s.messages <- data.String():
				default:
				}
				reply("250 OK queued")
				continue
			}
			data.WriteString(trimmed + "\n")
			continue
		}
		upper := strings.ToUpper(trimmed)
		switch {
		case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
			reply("250 sink")
		case strings.HasPrefix(upper, "MAIL FROM"), strings.HasPrefix(upper, "RCPT TO"):
			reply("250 OK")
		case strings.HasPrefix(upper, "DATA"):
			inData = true
			data.Reset()
			reply("354 end with a line containing only a dot")
		case strings.HasPrefix(upper, "QUIT"):
			reply("221 bye")
			return
		default:
			reply("250 OK")
		}
	}
}

// unsubscribeTokenFrom pulls the token out of the link the message carries. The body is sent as 8bit, so
// the link is present verbatim and no decoding is needed.
func unsubscribeTokenFrom(t *testing.T, raw string) string {
	t.Helper()
	const marker = "unsubscribe?token="
	at := strings.Index(raw, marker)
	if at < 0 {
		t.Fatalf("the delivered message has no unsubscribe link:\n%s", raw)
	}
	rest := raw[at+len(marker):]
	end := strings.IndexAny(rest, " \t\r\n")
	if end < 0 {
		end = len(rest)
	}
	token := rest[:end]
	if token == "" {
		t.Fatal("the unsubscribe link carries an empty token")
	}
	return token
}

func TestUnsubscribeLinkFromTheDeliveredMessage(t *testing.T) {
	env := newTopicEnv(t)
	sink := startSMTPSink(t)

	// The reader asks a question; another account answers it, which queues the notification the worker will
	// put the link into.
	reader := env.newUser("reader")
	in := env.base()
	in.AuthorID = reader
	in.BoardID = env.qaBoardID
	in.Title = "unsubscribe check"
	in.TagIDs = nil
	topic, err := env.store.CreateTopic(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if err := env.store.SaveNotificationPreferences(context.Background(), reader, notificationPreferences{EmailOnAnswer: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.answer(topic, env.newUser("answerer"), false); err != nil {
		t.Fatal(err)
	}

	sender, err := mailer.New(mailer.Config{
		Host:        "127.0.0.1",
		Port:        sink.port(),
		Security:    "none",
		FromAddress: "noreply@example.test",
		FromName:    "OpenTalk",
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := env.store.DeliverNotifications(context.Background(), sender, env.deliveryOptions(time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if report.Sent != 1 {
		t.Fatalf("the delivery pass reported %+v", report)
	}

	// Wait for the message the sink actually received, then take the token out of it.
	var raw string
	select {
	case raw = <-sink.messages:
	case <-time.After(5 * time.Second):
		t.Fatal("the SMTP sink received no message")
	}
	if !strings.Contains(raw, "Subject: ") {
		t.Fatalf("the captured payload does not look like a message:\n%s", raw)
	}
	token := unsubscribeTokenFrom(t, raw)

	// Presenting what was delivered to the real endpoint, with no session: the signature is the credential.
	res := env.do("POST", "/api/v1/subscriptions/unsubscribe", `{"token":"`+token+`"}`, "", "")
	expectStatus(t, res, 204)

	override, err := env.store.TopicSubscription(context.Background(), reader, topic)
	if err != nil {
		t.Fatal(err)
	}
	if override != overrideDisabled {
		t.Fatalf("after following the link the override is %q, want %q", override, overrideDisabled)
	}
	// One topic, nothing else: the account-wide switch is untouched, which is the promise the link makes.
	preferences, err := env.store.NotificationPreferences(context.Background(), reader)
	if err != nil {
		t.Fatal(err)
	}
	if preferences != (notificationPreferences{EmailOnAnswer: true}) {
		t.Fatalf("following the link changed the global preferences: %+v", preferences)
	}

	// The same link presented twice is still accepted (it is idempotent) but cannot be edited into another
	// topic's link, which the forgery tests cover; here it only has to still work after a second click.
	expectStatus(t, env.do("POST", "/api/v1/subscriptions/unsubscribe", `{"token":"`+token+`"}`, "", ""), 204)
}
