package comms

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/riverqueue/river"

	"github.com/ebnsina/muallim-api/internal/platform/mailer"
)

// Sender delivers a rendered message. Declared here, by its consumer; the
// drivers in platform/mailer satisfy it.
type Sender interface {
	Send(ctx context.Context, m mailer.Message) error
}

// EmailArgs is a message that has already been rendered.
//
// It carries no tenant and no entity ids, only text. A worker runs outside any
// tenant binding, so a job that had to load its subject from a tenant-scoped
// table would have to bypass row-level security to do it. Rendering at enqueue
// time, inside the transaction that already holds the tenant, avoids the
// question entirely.
type EmailArgs struct {
	To      string `json:"to"`
	Subject string `json:"subject"`
	Text    string `json:"text"`
	HTML    string `json:"html"`
}

// Kind identifies the job. It is persisted in every row of river_job, so it is a
// stored name: changing it strands the jobs already queued under the old one.
func (EmailArgs) Kind() string { return "email.send" }

// InsertOpts retries a send five times with River's default backoff. A mail
// server is a third party and is entitled to be briefly unreachable.
func (EmailArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 5}
}

// EmailWorker sends the messages other packages enqueue.
type EmailWorker struct {
	river.WorkerDefaults[EmailArgs]

	sender Sender
	log    *slog.Logger
}

// NewEmailWorker returns a worker that delivers through sender.
func NewEmailWorker(sender Sender, log *slog.Logger) (*EmailWorker, error) {
	if sender == nil {
		return nil, errors.New("comms: sender is required")
	}
	if log == nil {
		return nil, errors.New("comms: logger is required")
	}
	return &EmailWorker{sender: sender, log: log}, nil
}

// Work delivers one message.
//
// Delivery is at-least-once: a send that succeeds and then fails to report
// success is retried, and the recipient gets the message twice. That is the
// right trade for everything this system mails today — a duplicate verification
// link is a nuisance, a lost one is a stuck account — but it is a trade, and a
// message whose delivery has a side effect beyond the inbox must not reuse this
// job without a deduplication key.
func (w *EmailWorker) Work(ctx context.Context, job *river.Job[EmailArgs]) error {
	if err := w.sender.Send(ctx, mailer.Message{
		To:      job.Args.To,
		Subject: job.Args.Subject,
		Text:    job.Args.Text,
		HTML:    job.Args.HTML,
	}); err != nil {
		// Logged where it is handled, once. River records the error on the job row
		// and schedules the retry; the log line is what a human greps for.
		w.log.ErrorContext(ctx, "send email",
			slog.String("subject", job.Args.Subject),
			slog.Int("attempt", job.Attempt),
			slog.Any("error", err),
		)
		return fmt.Errorf("comms: deliver %q: %w", job.Args.Subject, err)
	}

	w.log.InfoContext(ctx, "email sent", slog.String("subject", job.Args.Subject))
	return nil
}
