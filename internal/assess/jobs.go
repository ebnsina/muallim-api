package assess

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

// GradeAttemptArgs grades one submitted attempt.
//
// It carries the tenant because a job runs outside a request and there is no
// other way to bind app.tenant_id — a job that guessed would read nothing, or
// somebody else's rows.
type GradeAttemptArgs struct {
	TenantID  uuid.UUID `json:"tenant_id"`
	AttemptID uuid.UUID `json:"attempt_id"`
}

// Kind is stored in every river_job row, so it is a name and not a label.
func (GradeAttemptArgs) Kind() string { return "assess.grade_attempt" }

// InsertOpts retries generously. An attempt that never grades is a learner
// staring at "grading" for ever, and grading is idempotent, so trying again is
// free.
func (GradeAttemptArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 10}
}

// GradeAttemptWorker grades attempts out of band.
type GradeAttemptWorker struct {
	river.WorkerDefaults[GradeAttemptArgs]

	assess *Service
}

// NewGradeAttemptWorker returns a worker, refusing one it cannot run.
func NewGradeAttemptWorker(service *Service) (*GradeAttemptWorker, error) {
	if service == nil {
		return nil, errors.New("assess: the grading worker needs a service")
	}
	return &GradeAttemptWorker{assess: service}, nil
}

// Work grades the attempt.
//
// Idempotent: a retry recomputes the same verdicts from the same rows, and an
// attempt already out of `grading` is left exactly as it is — an instructor may
// have marked its essay in the meantime.
//
// An attempt that no longer exists is not an error. The quiz was deleted, and
// retrying will not bring it back.
func (w *GradeAttemptWorker) Work(ctx context.Context, job *river.Job[GradeAttemptArgs]) error {
	_, err := w.assess.GradeAttempt(ctx, job.Args.TenantID, job.Args.AttemptID)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrNotFound):
		return nil
	default:
		return fmt.Errorf("assess: grade attempt %s: %w", job.Args.AttemptID, err)
	}
}

// RiverEnqueuer adapts a River client to Enqueuer.
//
// It lives here rather than in cmd/ because the job's arguments do, and a caller
// forced to construct them itself could construct them wrongly.
type RiverEnqueuer struct {
	client *river.Client[pgx.Tx]
}

// NewRiverEnqueuer returns an Enqueuer backed by River.
func NewRiverEnqueuer(client *river.Client[pgx.Tx]) (*RiverEnqueuer, error) {
	if client == nil {
		return nil, errors.New("assess: the river enqueuer needs a client")
	}
	return &RiverEnqueuer{client: client}, nil
}

// GradeAttempt queues the grading on the caller's transaction, so the job and the
// submission commit together — or neither does.
func (e *RiverEnqueuer) GradeAttempt(ctx context.Context, tx pgx.Tx, tenantID, attemptID uuid.UUID) error {
	args := GradeAttemptArgs{TenantID: tenantID, AttemptID: attemptID}
	if _, err := e.client.InsertTx(ctx, tx, args, nil); err != nil {
		return fmt.Errorf("assess: enqueue grading of attempt %s: %w", attemptID, err)
	}
	return nil
}
