package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/riverqueue/river"

	"github.com/ebnsina/lms-api/internal/auth"
)

// EraseOrphansArgs sweeps users who belong to no workspace.
//
// The job carries no arguments and no tenant: an orphan belongs to none, which is
// exactly why nothing tenant-scoped could ever reach them.
type EraseOrphansArgs struct{}

// Kind is stored in every river_job row, so it is a name, not a label.
func (EraseOrphansArgs) Kind() string { return "maintenance.erase_orphans" }

// InsertOpts retries three times. A periodic schedule does not backfill missed
// runs, so no queue of identical sweeps can build up while the worker is down,
// and the sweep is idempotent in any case.
func (EraseOrphansArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 3}
}

// EraseOrphansWorker honours GDPR's right to erasure for people no workspace can
// reach.
type EraseOrphansWorker struct {
	river.WorkerDefaults[EraseOrphansArgs]

	maintenance *auth.Maintenance
	log         *slog.Logger
}

// NewEraseOrphansWorker returns a worker, refusing one it cannot run.
func NewEraseOrphansWorker(maintenance *auth.Maintenance, log *slog.Logger) (*EraseOrphansWorker, error) {
	if maintenance == nil || log == nil {
		return nil, errors.New("worker: erase-orphans needs a maintenance service and a logger")
	}
	return &EraseOrphansWorker{maintenance: maintenance, log: log}, nil
}

// Work erases one batch. Jobs are retried, and this one is idempotent: a repeat
// finds whoever the last attempt did not reach, and a sweep with nothing to do
// does nothing.
func (w *EraseOrphansWorker) Work(ctx context.Context, _ *river.Job[EraseOrphansArgs]) error {
	erased, err := w.maintenance.EraseOrphanedUsers(ctx, auth.DefaultErasureBatch)
	if err != nil {
		return fmt.Errorf("worker: erase orphans: %w", err)
	}

	if erased > 0 {
		w.log.InfoContext(ctx, "erased orphaned users", slog.Int("count", erased))
	}
	return nil
}
