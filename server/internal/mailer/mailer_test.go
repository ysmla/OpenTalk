package mailer

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/mail"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeOptions pins the server behaviour a test cares about. A real relay cannot
// be asked to forget STARTTLS or to reject the envelope sender on demand, and
// those are exactly the paths worth testing.
type fakeOptions struct {
	startTLS     bool   // advertise STARTTLS in the EHLO response
	auth         bool   // advertise AUTH PLAIN in the EHLO response
	mailFromLine string // reply to MAIL FROM; empty means "250 2.1.0 sender ok"
	silent       bool   // accept the connection but never send the 220 greeting
}

// fakeServer is a hand-written SMTP server. It speaks just enough of the
// protocol for net/smtp and records what it received, so the package never needs
// a real relay or any network access beyond loopback.
type fakeServer struct {
	opt fakeOptions

	ln       net.Listener
	host     string
	port     int
	messages chan string

	mu       sync.Mutex
	conns    []net.Conn
	commands []string
	closed   bool
	wg       sync.WaitGroup
}

func newFakeServer(t *testing.T, opt fakeOptions) *fakeServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address is %T, want *net.TCPAddr", ln.Addr())
	}
	s := &fakeServer{
		opt: opt,
		ln:  ln,
		// The literal loopback address, not "localhost": smtp.PlainAuth's
		// localhost exception accepts it, so the authenticated tests exercise the
		// real credential path.
		host:     tcpAddr.IP.String(),
		port:     tcpAddr.Port,
		messages: make(chan string, 8),
	}
	s.wg.Add(1)
	go s.acceptLoop()
	t.Cleanup(s.Close)
	return s
}

// config returns a usable configuration pointed at this server.
func (s *fakeServer) config(security string) Config {
	return Config{
		Host:        s.host,
		Port:        s.port,
		Security:    security,
		FromAddress: "no-reply@opentalk.local",
		FromName:    "OpenTalk",
	}
}

// Close stops the listener and unblocks every handler, so a test that leaves a
// conversation mid-flight (a cancelled send, say) still tears down cleanly.
func (s *fakeServer) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	conns := append([]net.Conn(nil), s.conns...)
	s.mu.Unlock()

	_ = s.ln.Close()
	for _, conn := range conns {
		_ = conn.Close()
	}
	s.wg.Wait()
}

func (s *fakeServer) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = conn.Close()
			return
		}
		s.conns = append(s.conns, conn)
		s.wg.Add(1)
		s.mu.Unlock()
		go s.serveConn(conn)
	}
}

func (s *fakeServer) serveConn(conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()

	if s.opt.silent {
		// Read and discard: the point of a silent server is that the client
		// blocks on a greeting it never receives.
		_, _ = io.Copy(io.Discard, conn)
		return
	}

	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)
	reply := func(line string) bool {
		if _, err := writer.WriteString(line + "\r\n"); err != nil {
			return false
		}
		return writer.Flush() == nil
	}

	if !reply("220 fake ESMTP OpenTalk test server") {
		return
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		command := strings.TrimRight(line, "\r\n")
		s.recordCommand(command)
		upper := strings.ToUpper(command)
		switch {
		case strings.HasPrefix(upper, "EHLO"):
			if !s.replyEHLO(reply) {
				return
			}
		case strings.HasPrefix(upper, "HELO"):
			if !reply("250 fake") {
				return
			}
		case strings.HasPrefix(upper, "AUTH"):
			// PlainAuth sends the whole credential in one line, so a single 235
			// is all the client needs.
			if !reply("235 2.7.0 authentication succeeded") {
				return
			}
		case strings.HasPrefix(upper, "MAIL FROM"):
			line := s.opt.mailFromLine
			if line == "" {
				line = "250 2.1.0 sender ok"
			}
			if !reply(line) {
				return
			}
		case strings.HasPrefix(upper, "RCPT TO"):
			if !reply("250 2.1.5 recipient ok") {
				return
			}
		case upper == "DATA":
			if !reply("354 end data with <CR><LF>.<CR><LF>") {
				return
			}
			body, ok := readData(reader)
			if !ok {
				return
			}
			s.messages <- body
			if !reply("250 2.0.0 queued") {
				return
			}
		case upper == "QUIT":
			_ = reply("221 2.0.0 bye")
			return
		default:
			if !reply("250 2.0.0 ok") {
				return
			}
		}
	}
}

// replyEHLO writes a multi-line EHLO response: every capability but the last is
// introduced with "250-", which is what net/smtp's Extension parsing expects.
func (s *fakeServer) replyEHLO(reply func(string) bool) bool {
	caps := []string{"fake greets you"}
	if s.opt.startTLS {
		caps = append(caps, "STARTTLS")
	}
	if s.opt.auth {
		caps = append(caps, "AUTH PLAIN")
	}
	caps = append(caps, "8BITMIME", "SMTPUTF8")
	for i, cap := range caps {
		line := "250-" + cap
		if i == len(caps)-1 {
			line = "250 " + cap
		}
		if !reply(line) {
			return false
		}
	}
	return true
}

// readData consumes the DATA payload up to the terminating "." line and returns
// the message with dot-stuffing undone.
func readData(reader *bufio.Reader) (string, bool) {
	var lines []string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return "", false
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "." {
			break
		}
		// smtp's dot writer escapes a leading dot; undo it so assertions compare
		// against the body the caller supplied.
		lines = append(lines, strings.TrimPrefix(line, "."))
	}
	return strings.Join(lines, "\r\n") + "\r\n", true
}

func (s *fakeServer) recordCommand(command string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commands = append(s.commands, command)
}

func (s *fakeServer) commandLog() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.commands...)
}

// waitMessage returns the next recorded message, failing the test if the server
// never received one.
func (s *fakeServer) waitMessage(t *testing.T) string {
	t.Helper()
	select {
	case msg := <-s.messages:
		return msg
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the fake server to record a message")
		return ""
	}
}

// noMessage fails the test if the server received anything, which is how the
// fail-closed paths are checked.
func (s *fakeServer) noMessage(t *testing.T) {
	t.Helper()
	select {
	case msg := <-s.messages:
		t.Fatalf("server received a message it should have refused:\n%s", msg)
	case <-time.After(200 * time.Millisecond):
	}
}

// splitMessage parses a recorded message into its header block and body, so
// tests assert on values instead of raw wire bytes.
func splitMessage(t *testing.T, raw string) (textproto.MIMEHeader, string) {
	t.Helper()
	reader := textproto.NewReader(bufio.NewReader(strings.NewReader(raw)))
	header, err := reader.ReadMIMEHeader()
	if err != nil {
		t.Fatalf("ReadMIMEHeader(%q): %v", raw, err)
	}
	body, err := io.ReadAll(reader.R)
	if err != nil {
		t.Fatalf("reading message body: %v", err)
	}
	return header, string(body)
}

// decodeWord undoes RFC 2047 encoding. It is a no-op on plain ASCII, so a test
// can call it whether or not the header needed encoding.
func decodeWord(t *testing.T, value string) string {
	t.Helper()
	decoded, err := new(mime.WordDecoder).DecodeHeader(value)
	if err != nil {
		t.Fatalf("DecodeHeader(%q): %v", value, err)
	}
	return decoded
}

func TestSendPlainDeliversMessage(t *testing.T) {
	const (
		to      = "new-user@example.com"
		subject = "您的验证码是 123456"
		body    = "您的验证码是 123456，五分钟内有效。\n请勿转发给他人。"
	)

	srv := newFakeServer(t, fakeOptions{})
	sender, err := New(srv.config(securityNone))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := sender.Send(context.Background(), Message{To: to, Subject: subject, Body: body}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	raw := srv.waitMessage(t)
	header, gotBody := splitMessage(t, raw)

	if got := header.Get("To"); got != to {
		t.Errorf("To header = %q, want %q", got, to)
	}
	if !strings.Contains(raw, to) {
		t.Errorf("recorded message does not contain the recipient %q:\n%s", to, raw)
	}
	if got := header.Get("Subject"); !strings.HasPrefix(got, "=?utf-8?") {
		t.Errorf("Subject = %q, want an RFC 2047 encoded word", got)
	}
	if got := decodeWord(t, header.Get("Subject")); got != subject {
		t.Errorf("decoded Subject = %q, want %q", got, subject)
	}
	id := header.Get("Message-ID")
	if !strings.HasPrefix(id, "<") || !strings.HasSuffix(id, ">") || !strings.Contains(id, "@opentalk.local") {
		t.Errorf("Message-ID = %q, want <...@opentalk.local>", id)
	}
	if !strings.Contains(gotBody, "您的验证码是 123456") {
		t.Errorf("body = %q, want it to contain the verification code", gotBody)
	}
	if got := header.Get("MIME-Version"); got != "1.0" {
		t.Errorf("MIME-Version = %q, want 1.0", got)
	}
	if got := header.Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/plain; charset=utf-8", got)
	}
	if got := header.Get("Content-Transfer-Encoding"); got != "8bit" {
		t.Errorf("Content-Transfer-Encoding = %q, want 8bit", got)
	}
	if got := header.Get("Date"); got != "" {
		if _, err := time.Parse(time.RFC1123Z, got); err != nil {
			t.Errorf("Date = %q, want RFC 1123Z: %v", got, err)
		}
	} else {
		t.Error("Date header is missing")
	}

	// Every line break must be CRLF, and the message must end with one so smtp's
	// dot writer can put the terminating "." on a line of its own.
	for i, c := range raw {
		if c == '\n' && (i == 0 || raw[i-1] != '\r') {
			t.Fatalf("recorded message has a bare LF at offset %d:\n%q", i, raw)
		}
	}
	if !strings.HasSuffix(raw, "\r\n") {
		t.Errorf("recorded message does not end with CRLF: %q", raw)
	}
}

func TestSendEncodesNonASCIIDisplayName(t *testing.T) {
	srv := newFakeServer(t, fakeOptions{})
	cfg := srv.config(securityNone)
	cfg.FromName = "开源聊天"

	sender, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := sender.Send(context.Background(), Message{To: "new-user@example.com", Subject: "code", Body: "123456"}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	header, _ := splitMessage(t, srv.waitMessage(t))
	from := header.Get("From")
	if !strings.Contains(from, "=?utf-8?") {
		t.Errorf("From = %q, want the display name RFC 2047 encoded", from)
	}
	// mail.ParseAddress decodes encoded words, so this checks the value a mail
	// client would show rather than the wire form.
	addr, err := mail.ParseAddress(from)
	if err != nil {
		t.Fatalf("mail.ParseAddress(%q): %v", from, err)
	}
	if addr.Name != cfg.FromName {
		t.Errorf("From display name = %q, want %q", addr.Name, cfg.FromName)
	}
	if addr.Address != cfg.FromAddress {
		t.Errorf("From address = %q, want %q", addr.Address, cfg.FromAddress)
	}
}

func TestNewRejectsUnusableConfiguration(t *testing.T) {
	base := Config{
		Host:        "smtp.example.com",
		Port:        587,
		Security:    securityStartTLS,
		FromAddress: "no-reply@opentalk.local",
	}
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "empty host", mutate: func(c *Config) { c.Host = "" }},
		{name: "port zero", mutate: func(c *Config) { c.Port = 0 }},
		{name: "port above the TCP range", mutate: func(c *Config) { c.Port = 70000 }},
		{name: "empty from address", mutate: func(c *Config) { c.FromAddress = "" }},
		{name: "empty security", mutate: func(c *Config) { c.Security = "" }},
		{name: "ssl is not a mode", mutate: func(c *Config) { c.Security = "ssl" }},
		{name: "plain is not a mode", mutate: func(c *Config) { c.Security = "plain" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base
			tt.mutate(&cfg)
			sender, err := New(cfg)
			if err == nil {
				t.Fatalf("New with %s = %T, want an error", tt.name, sender)
			}
			if !errors.Is(err, ErrNotConfigured) {
				t.Errorf("New with %s error = %v, want it to wrap ErrNotConfigured", tt.name, err)
			}
		})
	}
}

func TestNewAcceptsUsableConfiguration(t *testing.T) {
	tests := []struct {
		name     string
		security string
		username string
		password string
	}{
		{name: "plaintext relay without credentials", security: securityNone},
		{name: "starttls with credentials", security: securityStartTLS, username: "relay-user", password: "relay-password"},
		{name: "implicit tls with credentials", security: securityTLS, username: "relay-user", password: "relay-password"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{
				Host:        "smtp.example.com",
				Port:        587,
				Security:    tt.security,
				Username:    tt.username,
				Password:    tt.password,
				FromAddress: "no-reply@opentalk.local",
				FromName:    "OpenTalk",
			}
			if _, err := New(cfg); err != nil {
				t.Errorf("New with %s: %v", tt.name, err)
			}
		})
	}
}

func TestSendReportsStageFailureWithoutLeakingPassword(t *testing.T) {
	const password = "relay-s3cret-passphrase"

	tests := []struct {
		name         string
		mailFrom     string
		wantStatus   string
		wantRedacted bool
	}{
		{
			name:       "sender rejected",
			mailFrom:   "550 5.7.1 sender address rejected",
			wantStatus: "550",
		},
		{
			// A broken or hostile relay can echo a credential back at us; the
			// error must not carry it into the logs.
			name:         "server echoes the credential",
			mailFrom:     "550 5.7.1 rejected " + password,
			wantStatus:   "550",
			wantRedacted: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newFakeServer(t, fakeOptions{auth: true, mailFromLine: tt.mailFrom})
			cfg := srv.config(securityNone)
			cfg.Username = "relay-user"
			cfg.Password = password

			sender, err := New(cfg)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			err = sender.Send(context.Background(), Message{
				To:      "new-user@example.com",
				Subject: "您的验证码是 123456",
				Body:    "您的验证码是 123456，五分钟内有效。",
			})
			if err == nil {
				t.Fatal("Send succeeded, want the MAIL FROM rejection to surface")
			}
			if strings.Contains(err.Error(), password) {
				t.Errorf("Send error leaked the SMTP password: %v", err)
			}
			if !strings.Contains(err.Error(), "MAIL FROM") {
				t.Errorf("Send error = %v, want it to name the MAIL FROM stage", err)
			}
			if !strings.Contains(err.Error(), tt.wantStatus) {
				t.Errorf("Send error = %v, want the server status %s in it", err, tt.wantStatus)
			}
			if tt.wantRedacted && !strings.Contains(err.Error(), "[redacted]") {
				t.Errorf("Send error = %v, want the echoed credential replaced with [redacted]", err)
			}
			srv.noMessage(t)
		})
	}
}

func TestStartTLSFailsClosedWhenNotAdvertised(t *testing.T) {
	// The server answers EHLO without offering STARTTLS.
	srv := newFakeServer(t, fakeOptions{})
	sender, err := New(srv.config(securityStartTLS))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	err = sender.Send(context.Background(), Message{
		To:      "new-user@example.com",
		Subject: "您的验证码是 123456",
		Body:    "您的验证码是 123456，五分钟内有效。",
	})
	if err == nil {
		t.Fatal("Send succeeded although the server never offered STARTTLS")
	}
	if !strings.Contains(strings.ToUpper(err.Error()), "STARTTLS") {
		t.Errorf("Send error = %v, want it to mention STARTTLS", err)
	}
	srv.noMessage(t)
	for _, command := range srv.commandLog() {
		upper := strings.ToUpper(command)
		for _, forbidden := range []string{"MAIL FROM", "RCPT TO", "DATA", "AUTH"} {
			if strings.HasPrefix(upper, forbidden) {
				t.Errorf("client sent %q after STARTTLS was unavailable; it must stop before the envelope", command)
			}
		}
	}
}

func TestSendReturnsImmediatelyWhenContextAlreadyCancelled(t *testing.T) {
	srv := newFakeServer(t, fakeOptions{silent: true})
	sender, err := New(srv.config(securityNone))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	err = sender.Send(ctx, Message{To: "new-user@example.com", Subject: "code", Body: "123456"})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Send with an already-cancelled context succeeded")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Send error = %v, want context.Canceled", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("Send took %s with an already-cancelled context, want an immediate return", elapsed)
	}
}

func TestSendAbortsWhenContextIsCancelledMidExchange(t *testing.T) {
	// The server accepts the connection and then says nothing, so only a
	// cancellation that reaches the live socket can end the call.
	srv := newFakeServer(t, fakeOptions{silent: true})
	sender, err := New(srv.config(securityNone))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	defer cancel()

	start := time.Now()
	err = sender.Send(ctx, Message{To: "new-user@example.com", Subject: "code", Body: "123456"})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Send against a silent server succeeded")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Send error = %v, want context.Canceled", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("Send blocked for %s after the context was cancelled", elapsed)
	}
}

func TestSendRespectsContextDeadline(t *testing.T) {
	srv := newFakeServer(t, fakeOptions{silent: true})
	sender, err := New(srv.config(securityNone))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	err = sender.Send(ctx, Message{To: "new-user@example.com", Subject: "code", Body: "123456"})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Send succeeded against a server that never greeted")
	}
	// Either the socket deadline derived from ctx or the cancellation watchdog
	// can win the race; both prove the deadline covers the whole exchange rather
	// than only the dial.
	var netErr net.Error
	if !errors.Is(err, context.DeadlineExceeded) && !(errors.As(err, &netErr) && netErr.Timeout()) {
		t.Errorf("Send error = %v, want the context deadline or a socket timeout", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("Send took %s, want it to stop at the context deadline", elapsed)
	}
}

func TestMessageIDsAreUniquePerSend(t *testing.T) {
	srv := newFakeServer(t, fakeOptions{})
	sender, err := New(srv.config(securityNone))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for i := 0; i < 2; i++ {
		msg := Message{
			To:      "new-user@example.com",
			Subject: fmt.Sprintf("您的验证码是 %06d", i),
			Body:    "您的验证码是 123456，五分钟内有效。",
		}
		if err := sender.Send(context.Background(), msg); err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
	}

	first, _ := splitMessage(t, srv.waitMessage(t))
	second, _ := splitMessage(t, srv.waitMessage(t))
	firstID, secondID := first.Get("Message-ID"), second.Get("Message-ID")
	if firstID == "" || secondID == "" {
		t.Fatalf("Message-ID missing: first %q, second %q", firstID, secondID)
	}
	if firstID == secondID {
		t.Errorf("both messages used Message-ID %q, want unique ids", firstID)
	}
}
