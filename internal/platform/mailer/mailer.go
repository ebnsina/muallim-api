// Package mailer delivers a rendered message to an address.
//
// It has no domain knowledge. What to say, and to whom, is decided elsewhere;
// this package only puts bytes on the wire.
package mailer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/smtp"
	"strings"
	"time"
)

// Message is a rendered email.
//
// Both bodies are always populated. HTML is what a modern client renders; Text
// is what everything else falls back to, and what a spam filter reads when it
// decides whether the HTML is hiding something.
type Message struct {
	To      string
	Subject string
	Text    string
	HTML    string
}

// ErrHeaderInjection is returned when a value bound for a header carries a line
// break.
//
// A newline in an address or subject lets its author append headers of their
// own — a Bcc, a different From — turning any transactional email into an open
// relay. The values here reach us from user input (an invited address, a
// workspace name), so they are checked rather than trusted.
var ErrHeaderInjection = errors.New("mailer: header value contains a line break")

// Log writes messages to the log instead of sending them.
//
// Development only: it prints the body, which for every message this system
// sends contains a single-use credential in a link. Config refuses it outside
// development for exactly that reason.
type Log struct{ log *slog.Logger }

// NewLog returns a Mailer that logs rather than delivers.
func NewLog(log *slog.Logger) *Log { return &Log{log: log} }

// Send records the message. It never fails, so a missing SMTP server cannot make
// registration fail on a laptop.
func (l *Log) Send(ctx context.Context, m Message) error {
	l.log.InfoContext(ctx, "email not sent: no SMTP server is configured",
		slog.String("to", m.To),
		slog.String("subject", m.Subject),
		slog.String("body", m.Text),
	)
	return nil
}

// SMTPOptions configures the SMTP driver.
type SMTPOptions struct {
	Host     string
	Port     int
	Username string
	Password string

	// From is the envelope sender and the From header, e.g. "LMS <no-reply@example.com>".
	From string
}

// SMTP delivers over SMTP with STARTTLS.
type SMTP struct {
	addr string
	host string
	from string
	auth smtp.Auth
}

// NewSMTP returns an SMTP mailer, refusing options it could not send with.
func NewSMTP(opts SMTPOptions) (*SMTP, error) {
	switch {
	case opts.Host == "":
		return nil, errors.New("mailer: SMTP host is required")
	case opts.Port <= 0 || opts.Port > 65535:
		return nil, fmt.Errorf("mailer: SMTP port %d is out of range", opts.Port)
	case opts.From == "":
		return nil, errors.New("mailer: SMTP from address is required")
	}
	if err := validateHeader(opts.From); err != nil {
		return nil, err
	}

	// smtp.PlainAuth refuses to hand a password to a server that has not
	// negotiated TLS, unless that server is localhost. Anonymous relays exist and
	// are configured with no username at all, so an empty username is not an error.
	var auth smtp.Auth
	if opts.Username != "" {
		auth = smtp.PlainAuth("", opts.Username, opts.Password, opts.Host)
	}

	return &SMTP{
		addr: fmt.Sprintf("%s:%d", opts.Host, opts.Port),
		host: opts.Host,
		from: opts.From,
		auth: auth,
	}, nil
}

// Send delivers m.
//
// net/smtp offers no way to pass a context, so a server that accepts the
// connection and then stalls is bounded by River's job timeout rather than by
// ctx. The context is still checked first, so a job cancelled before it starts
// does not open a connection at all.
func (s *SMTP) Send(ctx context.Context, m Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	raw, err := s.render(m)
	if err != nil {
		return err
	}

	if err := smtp.SendMail(s.addr, s.auth, s.from, []string{m.To}, raw); err != nil {
		return fmt.Errorf("mailer: send to %s: %w", m.To, err)
	}
	return nil
}

// render builds an RFC 5322 message with a multipart/alternative body.
func (s *SMTP) render(m Message) ([]byte, error) {
	if err := validateHeader(m.To); err != nil {
		return nil, err
	}
	if err := validateHeader(m.Subject); err != nil {
		return nil, err
	}

	boundary, err := randomBoundary()
	if err != nil {
		return nil, err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", s.from)
	fmt.Fprintf(&b, "To: %s\r\n", m.To)
	fmt.Fprintf(&b, "Subject: %s\r\n", m.Subject)
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().UTC().Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\n")
	fmt.Fprintf(&b, "Content-Type: multipart/alternative; boundary=%q\r\n\r\n", boundary)

	// Least-preferred alternative first: a client picks the last part it can render.
	fmt.Fprintf(&b, "--%s\r\n", boundary)
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
	b.WriteString(m.Text)
	b.WriteString("\r\n")

	fmt.Fprintf(&b, "--%s\r\n", boundary)
	b.WriteString("Content-Type: text/html; charset=utf-8\r\n\r\n")
	b.WriteString(m.HTML)
	b.WriteString("\r\n")

	fmt.Fprintf(&b, "--%s--\r\n", boundary)

	return []byte(b.String()), nil
}

func randomBoundary() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("mailer: generate boundary: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func validateHeader(v string) error {
	if strings.ContainsAny(v, "\r\n") {
		return ErrHeaderInjection
	}
	return nil
}
