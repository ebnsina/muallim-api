package commerce

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

// ConfirmRefundArgs chases an asynchronous refund until the gateway says the money
// moved, or admits it will not.
//
// It carries the tenant because a job runs outside a request and there is no other
// way to bind app.tenant_id — a job that guessed would read nothing, or the wrong
// workspace's rows.
type ConfirmRefundArgs struct {
	TenantID uuid.UUID `json:"tenant_id"`
	OrderID  uuid.UUID `json:"order_id"`
}

// Kind names the row in river_job.
func (ConfirmRefundArgs) Kind() string { return "commerce.confirm_refund" }

// refundPollInterval is how long a still-processing refund waits before it is asked
// again. Long, because a bank refund settles in hours or days, not seconds.
const refundPollInterval = 10 * time.Minute

// refundPollCeiling is when we stop asking. A refund still "processing" after this
// is one a person should look at, so the job records that and gives up rather than
// snoozing for ever.
const refundPollCeiling = 7 * 24 * time.Hour

/*
InsertOpts.

MaxAttempts is a backstop, not the schedule: a snooze does not spend an attempt, so
the poll paces itself on refundPollInterval and only burns an attempt on a real
error. A generous cap covers those retries without letting a genuinely stuck job run
against the ceiling for ever.
*/
func (ConfirmRefundArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 20, Queue: river.QueueDefault}
}

// ConfirmRefundWorker polls a gateway for a refund's final state.
type ConfirmRefundWorker struct {
	river.WorkerDefaults[ConfirmRefundArgs]

	commerce *Service
	log      *slog.Logger
}

// NewConfirmRefundWorker returns one, refusing what it cannot run.
func NewConfirmRefundWorker(service *Service, log *slog.Logger) (*ConfirmRefundWorker, error) {
	if service == nil {
		return nil, errors.New("commerce: the refund-confirmation worker needs a service")
	}
	if log == nil {
		log = slog.Default()
	}
	return &ConfirmRefundWorker{commerce: service, log: log}, nil
}

/*
Work asks the gateway what became of the refund.

Done, and there is nothing left to do. Pending, and the job snoozes — which does not
count against its attempts, so a refund may be chased for as long as the ceiling
allows without ever "failing". Failed, and the service has already written that to
the audit trail; the worker logs it too, because a refund the gateway took back is
somebody out both the course and the money, and it must not pass in silence.

An order that has moved on, or a gateway that is briefly unreachable, is a normal
retry — the second raises an error and River backs off; the first ConfirmRefund
reports as done.
*/
func (w *ConfirmRefundWorker) Work(ctx context.Context, job *river.Job[ConfirmRefundArgs]) error {
	// Stop asking once a refund has been processing longer than anyone should wait.
	if time.Since(job.CreatedAt) > refundPollCeiling {
		w.log.WarnContext(ctx, "giving up chasing a refund that never settled",
			slog.String("order_id", job.Args.OrderID.String()),
			slog.String("tenant_id", job.Args.TenantID.String()))
		return nil
	}

	state, err := w.commerce.ConfirmRefund(ctx, job.Args.TenantID, job.Args.OrderID)
	if err != nil {
		return fmt.Errorf("commerce: confirm refund %s: %w", job.Args.OrderID, err)
	}

	switch state {
	case RefundDone:
		return nil
	case RefundFailed:
		w.log.ErrorContext(ctx, "a gateway took a refund back after accepting it; the learner lost the course and the money",
			slog.String("order_id", job.Args.OrderID.String()),
			slog.String("tenant_id", job.Args.TenantID.String()))
		return nil
	default:
		return river.JobSnooze(refundPollInterval)
	}
}

// RefundPollEnqueuer adapts a River client to RefundPoller. It lives here because
// the job's arguments do, and a caller forced to build them could build them wrong.
type RefundPollEnqueuer struct {
	client *river.Client[pgx.Tx]
}

// NewRefundPollEnqueuer returns one.
func NewRefundPollEnqueuer(client *river.Client[pgx.Tx]) (*RefundPollEnqueuer, error) {
	if client == nil {
		return nil, errors.New("commerce: the refund-poll enqueuer needs a client")
	}
	return &RefundPollEnqueuer{client: client}, nil
}

// PollRefund queues the confirmation on the caller's transaction, so the job and the
// refunded order commit together — or neither does.
func (e *RefundPollEnqueuer) PollRefund(ctx context.Context, tx pgx.Tx, tenantID, orderID uuid.UUID) error {
	args := ConfirmRefundArgs{TenantID: tenantID, OrderID: orderID}
	if _, err := e.client.InsertTx(ctx, tx, args, nil); err != nil {
		return fmt.Errorf("commerce: enqueue refund confirmation for order %s: %w", orderID, err)
	}
	return nil
}

var _ RefundPoller = (*RefundPollEnqueuer)(nil)
