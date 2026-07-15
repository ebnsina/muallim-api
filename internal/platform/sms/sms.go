// Package sms delivers a short text to a phone number.
//
// It has no domain knowledge. What to say, and to whom, is decided elsewhere;
// this package only puts bytes on the wire. The primary market is Bangladesh, so
// the HTTP driver speaks the shape the common BD gateways expect — an api key, a
// sender id, a number, and a message — configurable to point at any of them.
package sms

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Message is a rendered SMS.
type Message struct {
	To   string
	Text string
}

// ErrInvalidNumber is returned when a destination number carries a control
// character. A number reaches us from user input (a guardian's phone), so it is
// checked rather than trusted before it is placed in a request.
var ErrInvalidNumber = errors.New("sms: number contains a control character")

func validNumber(to string) error {
	if strings.ContainsAny(to, "\r\n") {
		return ErrInvalidNumber
	}
	return nil
}

// Log writes messages to the log instead of sending them.
//
// Development only: no SMS costs money, and a guardian broadcast on a laptop has
// no gateway to reach. Config selects it when no gateway is configured.
type Log struct{ log *slog.Logger }

// NewLog returns a driver that logs rather than delivers.
func NewLog(log *slog.Logger) *Log { return &Log{log: log} }

// Send records the message. It never fails, so a missing gateway cannot make a
// notice fail on a laptop.
func (l *Log) Send(ctx context.Context, m Message) error {
	l.log.InfoContext(ctx, "sms not sent: no gateway is configured",
		slog.String("to", m.To),
		slog.String("text", m.Text),
	)
	return nil
}

// HTTPOptions configures the HTTP gateway driver.
type HTTPOptions struct {
	// URL is the gateway endpoint the form is posted to.
	URL string
	// APIKey and SenderID identify the account and the registered sender.
	APIKey   string
	SenderID string
	// Client is optional; a 10-second client is used when nil.
	Client *http.Client
}

// HTTP delivers through an HTTP SMS gateway by posting a form the common
// Bangladesh providers accept.
type HTTP struct {
	url      string
	apiKey   string
	senderID string
	client   *http.Client
}

// NewHTTP returns an HTTP driver.
func NewHTTP(opts HTTPOptions) (*HTTP, error) {
	if opts.URL == "" {
		return nil, errors.New("sms: gateway URL is required")
	}
	if _, err := url.Parse(opts.URL); err != nil {
		return nil, fmt.Errorf("sms: parse gateway URL: %w", err)
	}
	if opts.APIKey == "" {
		return nil, errors.New("sms: api key is required")
	}
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &HTTP{url: opts.URL, apiKey: opts.APIKey, senderID: opts.SenderID, client: client}, nil
}

// Send posts one message. A non-2xx response is an error, so River retries it.
func (h *HTTP) Send(ctx context.Context, m Message) error {
	if err := validNumber(m.To); err != nil {
		return err
	}

	form := url.Values{
		"api_key":  {h.apiKey},
		"senderid": {h.senderID},
		"number":   {m.To},
		"message":  {m.Text},
		"type":     {"text"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.url, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("sms: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := h.client.Do(req)
	if err != nil {
		return fmt.Errorf("sms: send: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("sms: gateway returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}
