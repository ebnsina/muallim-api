package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/ebnsina/lms-api/internal/assess"
	"github.com/ebnsina/lms-api/internal/audit"
	"github.com/ebnsina/lms-api/internal/auth"
	"github.com/ebnsina/lms-api/internal/enroll"
	"github.com/ebnsina/lms-api/internal/gamify"
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

// assessAuditor adapts the recorder to the assess package's interface, exactly as
// cmd/api does. Grading writes an audit line in the transaction that records the
// score.
type assessAuditor struct{ recorder *audit.Recorder }

func (a assessAuditor) Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e assess.AuditEntry) error {
	return a.recorder.Record(ctx, tx, tenantID, audit.Entry{
		ActorID: e.ActorID, Action: e.Action,
		TargetType: e.TargetType, TargetID: e.TargetID,
		IP: e.IP, UserAgent: e.UserAgent, Metadata: e.Metadata,
	})
}

// refusingEnqueuer satisfies assess.Enqueuer and queues nothing.
//
// Only a submitting request enqueues grading, and only cmd/api serves requests.
// A worker given a working enqueuer would be a worker that could queue work — and
// a grading job that enqueued its own grading is a loop nobody meant to write.
type refusingEnqueuer struct{}

func (refusingEnqueuer) GradeAttempt(context.Context, pgx.Tx, uuid.UUID, uuid.UUID) error {
	return errors.New("worker: the grading process does not enqueue jobs")
}

// enrolAuditor adapts the recorder to the enrolment package's interface.
//
// Completing the last lesson of a course completes the enrolment, and that
// transition is audited wherever it happens — including here, in a grading job.
type enrolAuditor struct{ recorder *audit.Recorder }

func (a enrolAuditor) Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e enroll.AuditEntry) error {
	return a.recorder.Record(ctx, tx, tenantID, audit.Entry{
		ActorID: e.ActorID, Action: e.Action,
		TargetType: e.TargetType, TargetID: e.TargetID,
		IP: e.IP, UserAgent: e.UserAgent, Metadata: e.Metadata,
	})
}

// gamifyRewards adapts the gamify service to enroll's Rewards interface, so a
// course completed by a passing quiz — graded here in the worker — earns its
// points and badge too, in the same transaction.
type gamifyRewards struct{ svc *gamify.Service }

var _ enroll.Rewards = gamifyRewards{}

func (r gamifyRewards) LessonCompleted(ctx context.Context, tx pgx.Tx, tenantID, userID, lessonID uuid.UUID) error {
	return r.svc.AwardLesson(ctx, tx, tenantID, userID, lessonID)
}

func (r gamifyRewards) CourseCompleted(ctx context.Context, tx pgx.Tx, tenantID, userID, courseID uuid.UUID) error {
	return r.svc.AwardCourse(ctx, tx, tenantID, userID, courseID)
}
