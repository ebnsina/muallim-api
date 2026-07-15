package comms

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/ebnsina/muallim-api/internal/platform/sms"
)

// SMSSender delivers a rendered text. Declared here, by its consumer; the drivers
// in platform/sms satisfy it.
type SMSSender interface {
	Send(ctx context.Context, m sms.Message) error
}

// SMSArgs is a text that has already been rendered. Like EmailArgs it carries no
// tenant and no entity ids, only the number and the words: a worker runs outside
// any tenant binding, so the message is composed at enqueue time, inside the
// transaction that still holds the tenant.
type SMSArgs struct {
	To   string `json:"to"`
	Text string `json:"text"`
}

// Kind is the stored job name in river_job; changing it strands queued jobs.
func (SMSArgs) Kind() string { return "sms.send" }

// InsertOpts retries a send five times. A gateway is a third party and is
// entitled to be briefly unreachable.
func (SMSArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 5}
}

// SendSMS enqueues a text for delivery, in the caller's transaction — the same
// at-least-once, commit-together guarantee EmailArgs has.
func (e *Enqueuer) SendSMS(ctx context.Context, tx pgx.Tx, to, text string) error {
	if _, err := e.client.InsertTx(ctx, tx, SMSArgs{To: to, Text: text}, nil); err != nil {
		return fmt.Errorf("comms: enqueue sms to %q: %w", to, err)
	}
	return nil
}

// SMSWorker sends the texts other packages enqueue.
type SMSWorker struct {
	river.WorkerDefaults[SMSArgs]

	sender SMSSender
	log    *slog.Logger
}

// NewSMSWorker returns a worker that delivers through sender.
func NewSMSWorker(sender SMSSender, log *slog.Logger) (*SMSWorker, error) {
	if sender == nil {
		return nil, errors.New("comms: sms sender is required")
	}
	if log == nil {
		return nil, errors.New("comms: logger is required")
	}
	return &SMSWorker{sender: sender, log: log}, nil
}

// Work delivers one text. Delivery is at-least-once, like email: a send that
// succeeds and then fails to report success is retried, and the recipient gets the
// text twice. A notice is safe to repeat; a message with a side effect beyond the
// handset must not reuse this job without a deduplication key.
func (w *SMSWorker) Work(ctx context.Context, job *river.Job[SMSArgs]) error {
	if err := w.sender.Send(ctx, sms.Message{To: job.Args.To, Text: job.Args.Text}); err != nil {
		w.log.ErrorContext(ctx, "send sms",
			slog.String("to", job.Args.To),
			slog.Int("attempt", job.Attempt),
			slog.Any("error", err),
		)
		return fmt.Errorf("comms: deliver sms to %q: %w", job.Args.To, err)
	}
	w.log.InfoContext(ctx, "sms sent", slog.String("to", job.Args.To))
	return nil
}
