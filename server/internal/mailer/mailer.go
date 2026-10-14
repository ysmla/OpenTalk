// Package mailer sends the transactional email this service needs — email
// verification codes for registration and for password reset — over SMTP.
//
// It uses the standard library only: the mail path is small, and a dependency
// that has to be reviewed for a security-sensitive transport is not worth it.
//
// Verification codes are bearer secrets, so nothing in this package logs or
// returns the message body, the SMTP transcript or the credentials. Errors name
// the endpoint and the failing stage, never the content of the message.
package mailer

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// Transport security modes accepted in Config.Security. They are unexported on
// purpose: the only supported way to pick one is the administrator setting
// itself, so there is no exported constant anyone could pass by mistake.
const (
	securityNone     = "none"
	securityStartTLS = "starttls"
	securityTLS      = "tls"
)

// Config describes how to reach the SMTP server. It is filled from administrator-editable
// settings, so every field may be empty at runtime and must be validated before use.
type Config struct {
	Host        string
	Port        int
	Security    string // "none", "starttls" or "tls"
	Username    string
	Password    string
	FromAddress string
	FromName    string
}

// Message is one outbound transactional email.
type Message struct {
	To      string
	Subject string
	Body    string
}

// Sender delivers messages. It exists so callers can substitute a fake in tests.
type Sender interface {
	Send(ctx context.Context, msg Message) error
}

// ErrNotConfigured reports that required SMTP settings are missing, so the caller can answer 503
// instead of attempting a connection with an empty host.
var ErrNotConfigured = errors.New("smtp not configured")

// smtpSender is the production Sender. It keeps a validated copy of the
// configuration and opens a fresh connection per message: verification traffic
// is rare, and a pooled connection would have to be re-authenticated and
// re-encrypted on every reuse anyway.
type smtpSender struct {
	cfg Config
}

// New validates cfg and returns a Sender.
//
// Every configuration failure wraps ErrNotConfigured — including an unknown
// Security value — because all of them are operator mistakes that the caller
// should surface the same way (503, "mail is not configured yet") instead of
// attempting a connection.
func New(cfg Config) (Sender, error) {
	switch {
	case cfg.Host == "":
		return nil, fmt.Errorf("%w: host is empty", ErrNotConfigured)
	case cfg.Port < 1 || cfg.Port > 65535:
		return nil, fmt.Errorf("%w: port %d is outside 1..65535", ErrNotConfigured, cfg.Port)
	case cfg.FromAddress == "":
		return nil, fmt.Errorf("%w: from address is empty", ErrNotConfigured)
	}
	switch cfg.Security {
	case securityNone, securityStartTLS, securityTLS:
	default:
		// An empty or misspelled mode must never be read as "unencrypted": a
		// missing setting would then silently send mail in plaintext, which is
		// exactly the failure this check exists to prevent.
		return nil, fmt.Errorf("%w: security %q is not one of %q, %q or %q",
			ErrNotConfigured, cfg.Security, securityNone, securityStartTLS, securityTLS)
	}
	return &smtpSender{cfg: cfg}, nil
}

// Send delivers one message.
//
// The whole exchange — dial, TLS handshake and every SMTP command — is bounded
// by ctx, not just the initial dial. net/smtp is frozen and has no context-aware
// API, so the deadline is additionally pushed onto the socket and a watchdog
// closes the connection when ctx is cancelled; without that, a silent server
// would block until the operating system's TCP timeout.
func (s *smtpSender) Send(ctx context.Context, msg Message) error {
	// An already-expired context must not even open a connection.
	if err := ctx.Err(); err != nil {
		return err
	}
	// Render the message first, so a malformed header fails without a connection
	// and without a half-sent envelope.
	raw, err := s.buildMessage(msg)
	if err != nil {
		return err
	}

	conn, err := s.dial(ctx)
	if err != nil {
		return s.fail(ctx, "connect", err)
	}
	defer conn.Close()

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			// Closing the socket is what unblocks the in-flight read or write
			// inside net/smtp.
			_ = conn.Close()
		case <-done:
		}
	}()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	client, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		return s.fail(ctx, "greeting", err)
	}
	defer client.Close()

	if err := s.secure(ctx, client); err != nil {
		return err
	}

	if s.cfg.Username != "" {
		// smtp.PlainAuth refuses to put credentials on an unencrypted connection
		// unless the host is localhost, since a plaintext server's claims cannot
		// be trusted. That guard is kept as-is on purpose: an operator who
		// configured a username without encryption gets a loud failure instead of
		// a password on the wire, and working around the check here would defeat it.
		auth := smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)
		if err := client.Auth(auth); err != nil {
			return s.fail(ctx, "AUTH", err)
		}
	}

	if err := client.Mail(s.cfg.FromAddress); err != nil {
		return s.fail(ctx, "MAIL FROM", err)
	}
	if err := client.Rcpt(envelopeRecipient(msg.To)); err != nil {
		return s.fail(ctx, "RCPT TO", err)
	}
	wc, err := client.Data()
	if err != nil {
		return s.fail(ctx, "DATA", err)
	}
	if _, err := wc.Write(raw); err != nil {
		// Close the writer so the connection does not stay mid-message, but
		// report the write failure rather than the cleanup one.
		_ = wc.Close()
		return s.fail(ctx, "DATA", err)
	}
	if err := wc.Close(); err != nil {
		return s.fail(ctx, "DATA", err)
	}
	if err := client.Quit(); err != nil {
		return s.fail(ctx, "QUIT", err)
	}
	return nil
}

// dial opens the transport for the configured security mode. For implicit TLS
// (tls.Client) the handshake runs under ctx too, so a server that accepts the
// TCP connection and then stalls cannot hold the caller hostage.
func (s *smtpSender) dial(ctx context.Context) (net.Conn, error) {
	dialer := &net.Dialer{}
	if deadline, ok := ctx.Deadline(); ok {
		dialer.Deadline = deadline
	}
	conn, err := dialer.DialContext(ctx, "tcp", s.addr())
	if err != nil {
		return nil, err
	}
	if s.cfg.Security != securityTLS {
		return conn, nil
	}
	tlsConn := tls.Client(conn, s.tlsConfig())
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return tlsConn, nil
}

// secure performs the transport-security step the operator asked for. It never
// downgrades: a server that does not offer STARTTLS ends the send, because the
// operator explicitly asked for encryption.
func (s *smtpSender) secure(ctx context.Context, client *smtp.Client) error {
	switch s.cfg.Security {
	case securityNone:
		// Plaintext was requested explicitly; nothing to negotiate.
		return nil
	case securityStartTLS:
		ok, _ := client.Extension("STARTTLS")
		if !ok {
			return &stageError{
				stage: "STARTTLS",
				addr:  s.addr(),
				msg:   "server does not advertise STARTTLS; refusing to send in plaintext",
			}
		}
		if err := client.StartTLS(s.tlsConfig()); err != nil {
			return s.fail(ctx, "STARTTLS", err)
		}
		return nil
	case securityTLS:
		// The connection was already encrypted from the first byte.
		return nil
	default:
		// Unreachable while New validates, and refusing is the only safe answer
		// if that ever changes.
		return fmt.Errorf("%w: security %q", ErrNotConfigured, s.cfg.Security)
	}
}

// tlsConfig pins ServerName to the configured host. ServerName drives both SNI
// and certificate verification, so it must stay the host name the certificate
// was issued for and never the dial address (an IP dial address would turn
// verification off).
func (s *smtpSender) tlsConfig() *tls.Config {
	return &tls.Config{
		ServerName: s.cfg.Host,
		MinVersion: tls.VersionTLS12,
	}
}

// buildMessage renders the RFC 5322 wire form: headers CRLF terminated, a blank
// CRLF line, then a body that ends with CRLF so smtp's dot writer can put the
// terminating "." on a line of its own.
func (s *smtpSender) buildMessage(msg Message) ([]byte, error) {
	if msg.To == "" {
		return nil, errors.New("mailer: message has no recipient")
	}
	// Reject header injection: every one of these values lands in a header, and
	// a stray line break would let a caller append headers of its own.
	for _, field := range []struct{ name, value string }{
		{"To", msg.To},
		{"Subject", msg.Subject},
		{"FromName", s.cfg.FromName},
	} {
		if strings.ContainsAny(field.value, "\r\n") {
			return nil, fmt.Errorf("mailer: %s contains a line break", field.name)
		}
	}

	from := s.cfg.FromAddress
	if s.cfg.FromName != "" {
		// mail.Address handles both RFC 5322 quoting and RFC 2047 encoding, so a
		// display name such as "开源聊天" survives the wire.
		from = (&mail.Address{Name: s.cfg.FromName, Address: s.cfg.FromAddress}).String()
	}
	subject := msg.Subject
	if !isASCII(subject) {
		// Only non-ASCII subjects need encoding; encoding plain ASCII would make
		// the raw header unreadable for no gain.
		subject = mime.QEncoding.Encode("utf-8", subject)
	}

	var buf bytes.Buffer
	bw := bufio.NewWriter(&buf)
	tw := textproto.NewWriter(bw)
	headers := []struct{ name, value string }{
		{"From", from},
		{"To", msg.To},
		{"Subject", subject},
		{"Date", time.Now().Format(time.RFC1123Z)},
		{"Message-ID", newMessageID(s.cfg.FromAddress)},
		{"MIME-Version", "1.0"},
		{"Content-Type", "text/plain; charset=utf-8"},
		{"Content-Transfer-Encoding", "8bit"},
	}
	for _, h := range headers {
		if err := tw.PrintfLine("%s: %s", h.name, h.value); err != nil {
			return nil, fmt.Errorf("mailer: writing %s header: %w", h.name, err)
		}
	}
	if err := bw.Flush(); err != nil {
		return nil, fmt.Errorf("mailer: writing message: %w", err)
	}
	// The blank line that ends the header block.
	buf.WriteString("\r\n")
	buf.WriteString(normalizeBody(msg.Body))
	return buf.Bytes(), nil
}

// addr is the dial target, formatted so an IPv6 literal keeps its brackets.
func (s *smtpSender) addr() string {
	return net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port))
}

// fail attributes a failure to one SMTP stage and one endpoint, so a caller can
// tell a connect failure from a protocol failure. Configuration failures are a
// different shape: they come from New and wrap ErrNotConfigured.
func (s *smtpSender) fail(ctx context.Context, stage string, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		// The context, not the socket, decided the outcome: report the caller's
		// own error so errors.Is(err, context.Canceled/DeadlineExceeded) works.
		return &stageError{stage: stage, addr: s.addr(), msg: cerr.Error(), err: cerr}
	}
	return &stageError{stage: stage, addr: s.addr(), msg: s.redact(err.Error()), err: err}
}

// redact strips the configured password from text produced by the server: a
// broken or hostile relay can echo a credential back in a rejection, and these
// errors end up in operator logs.
func (s *smtpSender) redact(text string) string {
	if s.cfg.Password == "" {
		return text
	}
	return strings.ReplaceAll(text, s.cfg.Password, "[redacted]")
}

// stageError names the endpoint and the SMTP stage that failed. Its rendered
// form never contains the password: Unwrap keeps the original error for
// errors.Is/As, but the message a caller prints comes from here.
type stageError struct {
	stage string
	addr  string
	msg   string
	err   error
}

func (e *stageError) Error() string {
	return fmt.Sprintf("mailer: smtp %s to %s failed: %s", e.stage, e.addr, e.msg)
}

func (e *stageError) Unwrap() error { return e.err }

// envelopeRecipient reduces a header form such as "Name <a@b>" to the bare
// address RCPT TO expects. Anything that does not parse is passed through
// unchanged: the server is the authority on envelope validity, and duplicating
// its judgement here would only produce a worse error message.
func envelopeRecipient(to string) string {
	addr, err := mail.ParseAddress(to)
	if err != nil {
		return to
	}
	return addr.Address
}

// normalizeBody rewrites every line ending to CRLF and guarantees a trailing
// one, so the terminating "." cannot end up glued to the last line of the body.
func normalizeBody(body string) string {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")
	body = strings.ReplaceAll(body, "\n", "\r\n")
	if !strings.HasSuffix(body, "\r\n") {
		body += "\r\n"
	}
	return body
}

// messageIDSeq keeps Message-IDs unique even if two sends land in the same
// nanosecond and crypto/rand were unavailable.
var messageIDSeq atomic.Uint64

// newMessageID builds a per-message identifier whose right-hand side is the
// sender's domain, as RFC 5322 requires.
func newMessageID(from string) string {
	domain := "localhost"
	if at := strings.LastIndex(from, "@"); at >= 0 && at < len(from)-1 {
		domain = from[at+1:]
	}
	if strings.ContainsAny(domain, " \t\r\n<>\"") {
		// A malformed FromAddress must not be able to break the header it is
		// pasted into.
		domain = "localhost"
	}
	var entropy [12]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		// crypto/rand does not fail in practice; the sequence number alone still
		// makes the id unique within this process.
		entropy = [12]byte{}
	}
	return fmt.Sprintf("<%d.%d.%s@%s>",
		time.Now().UnixNano(), messageIDSeq.Add(1), hex.EncodeToString(entropy[:]), domain)
}

// isASCII reports whether s is pure 7-bit text, which decides whether a header
// value needs RFC 2047 encoding.
func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}
