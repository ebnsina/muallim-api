package mailer

import (
	"errors"
	"strings"
	"testing"
)

func testSMTP(t *testing.T) *SMTP {
	t.Helper()

	m, err := NewSMTP(SMTPOptions{Host: "smtp.example.com", Port: 587, From: "LMS <no-reply@example.com>"})
	if err != nil {
		t.Fatalf("NewSMTP: %v", err)
	}
	return m
}

// A newline in a header value lets its author append headers of their own — a
// Bcc, a different From — which turns a transactional email into an open relay.
// These values come from user input: an invited address, a workspace name.
func TestRenderRefusesHeaderInjection(t *testing.T) {
	t.Parallel()

	m := testSMTP(t)

	tests := []struct {
		name string
		msg  Message
	}{
		{"newline in recipient", Message{To: "ada@example.com\r\nBcc: mallory@evil.test", Subject: "Hi"}},
		{"bare newline in recipient", Message{To: "ada@example.com\nBcc: mallory@evil.test", Subject: "Hi"}},
		{"newline in subject", Message{To: "ada@example.com", Subject: "Hi\r\nBcc: mallory@evil.test"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, err := m.render(tt.msg); !errors.Is(err, ErrHeaderInjection) {
				t.Errorf("render() error = %v, want ErrHeaderInjection", err)
			}
		})
	}
}

func TestNewSMTPRefusesAnInjectedFromAddress(t *testing.T) {
	t.Parallel()

	_, err := NewSMTP(SMTPOptions{Host: "smtp.example.com", Port: 587, From: "a@b.test\r\nBcc: c@d.test"})
	if !errors.Is(err, ErrHeaderInjection) {
		t.Errorf("NewSMTP() error = %v, want ErrHeaderInjection", err)
	}
}

func TestNewSMTPValidatesOptions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opts SMTPOptions
	}{
		{"no host", SMTPOptions{Port: 587, From: "a@b.test"}},
		{"no from", SMTPOptions{Host: "smtp.example.com", Port: 587}},
		{"port zero", SMTPOptions{Host: "smtp.example.com", From: "a@b.test"}},
		{"port out of range", SMTPOptions{Host: "smtp.example.com", Port: 70000, From: "a@b.test"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, err := NewSMTP(tt.opts); err == nil {
				t.Error("NewSMTP accepted options it cannot send with")
			}
		})
	}
}

// The rendered message must be a multipart/alternative carrying both bodies, with
// the plain text first so a client picks the richest part it understands.
func TestRenderBuildsMultipartAlternative(t *testing.T) {
	t.Parallel()

	m := testSMTP(t)

	raw, err := m.render(Message{
		To:      "ada@example.com",
		Subject: "Confirm your email address",
		Text:    "plain body",
		HTML:    "<p>html body</p>",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	out := string(raw)

	for _, want := range []string{
		"From: LMS <no-reply@example.com>\r\n",
		"To: ada@example.com\r\n",
		"Subject: Confirm your email address\r\n",
		"MIME-Version: 1.0\r\n",
		"Content-Type: multipart/alternative; boundary=",
		"Content-Type: text/plain; charset=utf-8",
		"Content-Type: text/html; charset=utf-8",
		"plain body",
		"<p>html body</p>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered message is missing %q:\n%s", want, out)
		}
	}

	if strings.Index(out, "text/plain") > strings.Index(out, "text/html") {
		t.Error("html part precedes the plain part; a client would prefer the plain one")
	}
}

// Two messages must not share a boundary, or a body containing the first
// boundary could terminate the second message early.
func TestRenderUsesAFreshBoundary(t *testing.T) {
	t.Parallel()

	m := testSMTP(t)
	msg := Message{To: "ada@example.com", Subject: "Hi", Text: "t", HTML: "<p>h</p>"}

	first, err := m.render(msg)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	second, err := m.render(msg)
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	if boundaryOf(t, string(first)) == boundaryOf(t, string(second)) {
		t.Error("two messages shared a MIME boundary")
	}
}

func boundaryOf(t *testing.T, msg string) string {
	t.Helper()

	const marker = `boundary="`
	i := strings.Index(msg, marker)
	if i < 0 {
		t.Fatalf("no boundary in:\n%s", msg)
	}
	rest := msg[i+len(marker):]

	j := strings.Index(rest, `"`)
	if j < 0 {
		t.Fatalf("unterminated boundary in:\n%s", msg)
	}
	return rest[:j]
}
