// Package comms owns outbound communication: what a message says, and getting it
// onto a queue in the transaction that decided to send it.
//
// It knows nothing about HTTP, and nothing about the domains that ask it to send.
// Callers name the interface they need; cmd/ wires this package in behind it.
package comms

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

// Enqueuer renders a message and enqueues it for delivery.
//
// Every method takes the caller's transaction. The job and the row that caused
// it commit together or not at all, which is the entire reason River is backed by
// Postgres: there is no interleaving in which a password reset token exists and
// its email was never queued, nor one in which the email is sent for a
// transaction that rolled back.
type Enqueuer struct {
	client *river.Client[pgx.Tx]
	base   *url.URL
}

// NewEnqueuer returns an Enqueuer that builds links against webBaseURL — the
// origin of the web client, which owns the pages these links point at.
func NewEnqueuer(client *river.Client[pgx.Tx], webBaseURL string) (*Enqueuer, error) {
	if client == nil {
		return nil, errors.New("comms: river client is required")
	}

	base, err := url.Parse(webBaseURL)
	if err != nil {
		return nil, fmt.Errorf("comms: parse web base url: %w", err)
	}
	if base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("comms: web base url %q needs a scheme and a host", webBaseURL)
	}

	return &Enqueuer{client: client, base: base}, nil
}

// SendVerification asks a new account to confirm the address it registered with.
func (e *Enqueuer) SendVerification(ctx context.Context, tx pgx.Tx, to, name, token string, expiresIn time.Duration) error {
	link := e.link("/verify-email", token)

	return e.enqueue(ctx, tx, EmailArgs{
		To:      to,
		Subject: "Confirm your email address",
		Text: fmt.Sprintf(
			"Hi %s,\n\nConfirm your email address to finish setting up your account:\n\n%s\n\n"+
				"The link expires in %s and can be used once.\n\n"+
				"If you did not create this account, ignore this email.",
			name, link, humanDuration(expiresIn)),
		HTML: paragraphs(
			fmt.Sprintf("Hi %s,", html.EscapeString(name)),
			"Confirm your email address to finish setting up your account.",
			button(link, "Confirm email address"),
			fmt.Sprintf("The link expires in %s and can be used once.", humanDuration(expiresIn)),
			"If you did not create this account, ignore this email.",
		),
	})
}

// SendPasswordReset delivers a single-use link that authorises a new password.
func (e *Enqueuer) SendPasswordReset(ctx context.Context, tx pgx.Tx, to, name, token string, expiresIn time.Duration) error {
	link := e.link("/reset-password", token)

	return e.enqueue(ctx, tx, EmailArgs{
		To:      to,
		Subject: "Reset your password",
		Text: fmt.Sprintf(
			"Hi %s,\n\nSomeone asked to reset the password for this account. If it was you, choose a new one:\n\n%s\n\n"+
				"The link expires in %s and can be used once.\n\n"+
				"If it was not you, ignore this email. Your password has not changed.",
			name, link, humanDuration(expiresIn)),
		HTML: paragraphs(
			fmt.Sprintf("Hi %s,", html.EscapeString(name)),
			"Someone asked to reset the password for this account. If it was you, choose a new one.",
			button(link, "Choose a new password"),
			fmt.Sprintf("The link expires in %s and can be used once.", humanDuration(expiresIn)),
			"If it was not you, ignore this email. Your password has not changed.",
		),
	})
}

// SendInvitation delivers an offer of membership in a workspace.
func (e *Enqueuer) SendInvitation(ctx context.Context, tx pgx.Tx, to, workspace, token string, expiresIn time.Duration) error {
	link := e.link("/accept-invitation", token)

	return e.enqueue(ctx, tx, EmailArgs{
		To:      to,
		Subject: fmt.Sprintf("You have been invited to %s", workspace),
		Text: fmt.Sprintf(
			"You have been invited to join %s.\n\nAccept the invitation:\n\n%s\n\n"+
				"The link expires in %s.\n\n"+
				"If you already have an account, you will be asked for its password.",
			workspace, link, humanDuration(expiresIn)),
		HTML: paragraphs(
			fmt.Sprintf("You have been invited to join <strong>%s</strong>.", html.EscapeString(workspace)),
			button(link, "Accept the invitation"),
			fmt.Sprintf("The link expires in %s.", humanDuration(expiresIn)),
			"If you already have an account, you will be asked for its password.",
		),
	})
}

func (e *Enqueuer) enqueue(ctx context.Context, tx pgx.Tx, args EmailArgs) error {
	if _, err := e.client.InsertTx(ctx, tx, args, nil); err != nil {
		return fmt.Errorf("comms: enqueue %q: %w", args.Subject, err)
	}
	return nil
}

// SendRendered enqueues an email whose subject and body a caller has already
// composed — the notification digest, where the text is built from a person's own
// notifications and there is no template for comms to own. The plain text is
// wrapped into a minimal HTML body so a client that prefers HTML has one.
func (e *Enqueuer) SendRendered(ctx context.Context, tx pgx.Tx, to, subject, text string) error {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	for i, line := range lines {
		lines[i] = html.EscapeString(line)
	}
	return e.enqueue(ctx, tx, EmailArgs{
		To:      to,
		Subject: subject,
		Text:    text,
		HTML:    paragraphs(strings.Join(lines, "<br>")),
	})
}

// link builds an absolute URL into the web client, carrying the token as a query
// parameter.
func (e *Enqueuer) link(path, token string) string {
	u := *e.base
	u.Path = strings.TrimSuffix(u.Path, "/") + path

	q := u.Query()
	q.Set("token", token)
	u.RawQuery = q.Encode()

	return u.String()
}

// button renders a link. The href is escaped because the token is base64url and
// the query separator is an ampersand, both of which are markup in an attribute.
func button(link, label string) string {
	return fmt.Sprintf(`<a href=%q>%s</a>`, html.EscapeString(link), html.EscapeString(label))
}

func paragraphs(lines ...string) string {
	var b strings.Builder
	for _, line := range lines {
		fmt.Fprintf(&b, "<p>%s</p>\n", line)
	}
	return b.String()
}

// humanDuration renders a TTL the way a person would say it. The durations here
// are chosen by the caller that owns the token, so this formats whole hours and
// days and does not try to be clever about the rest.
func humanDuration(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return plural(int(d.Hours())/24, "day")
	case d >= time.Hour:
		return plural(int(d.Hours()), "hour")
	default:
		return plural(int(d.Minutes()), "minute")
	}
}

func plural(n int, unit string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", unit)
	}
	return fmt.Sprintf("%d %ss", n, unit)
}
