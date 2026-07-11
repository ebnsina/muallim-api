package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

// AnnouncementFanoutArgs notifies every enrolled learner of a course that an
// announcement was posted.
//
// It is a job, not an inline write: a busy course has thousands of learners, and
// an instructor should not wait for a bell to reach each of them before their
// "posted" confirmation returns. It carries the tenant because a job runs outside
// a request and there is no other way to bind app.tenant_id — and the
// announcement's content, so the worker needs no second read.
type AnnouncementFanoutArgs struct {
	TenantID       uuid.UUID `json:"tenant_id"`
	CourseID       uuid.UUID `json:"course_id"`
	AnnouncementID uuid.UUID `json:"announcement_id"`
	Title          string    `json:"title"`
	Body           string    `json:"body"`
	Link           string    `json:"link"`
}

// Kind is stored in every river_job row, so it is a name, not a label.
func (AnnouncementFanoutArgs) Kind() string { return "notify.announcement_fanout" }

// InsertOpts retries a few times. The fan-out is idempotent — the notification id
// is derived from the announcement and the recipient — so a retry after a partial
// failure reaches only whoever the last attempt missed.
func (AnnouncementFanoutArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 5}
}

// AnnouncementFanoutWorker delivers the fan-out out of band.
type AnnouncementFanoutWorker struct {
	river.WorkerDefaults[AnnouncementFanoutArgs]

	notify *Service
}

// NewAnnouncementFanoutWorker returns a worker, refusing one it cannot run.
func NewAnnouncementFanoutWorker(service *Service) (*AnnouncementFanoutWorker, error) {
	if service == nil {
		return nil, errors.New("notify: the fan-out worker needs a service")
	}
	return &AnnouncementFanoutWorker{notify: service}, nil
}

// Work notifies the course's enrolled learners. Idempotent: a repeat inserts
// nothing for anyone already reached.
func (w *AnnouncementFanoutWorker) Work(ctx context.Context, job *river.Job[AnnouncementFanoutArgs]) error {
	a := job.Args
	if _, err := w.notify.FanOutAnnouncement(ctx, a.TenantID, a.CourseID, a.AnnouncementID, a.Title, a.Body, a.Link); err != nil {
		return fmt.Errorf("notify: fan out announcement %s: %w", a.AnnouncementID, err)
	}
	return nil
}

// RiverEnqueuer queues notification jobs on the caller's transaction, so the job
// and the event that warrants it commit together — or neither does.
type RiverEnqueuer struct {
	client *river.Client[pgx.Tx]
}

// NewRiverEnqueuer returns an enqueuer, refusing one without a client.
func NewRiverEnqueuer(client *river.Client[pgx.Tx]) (*RiverEnqueuer, error) {
	if client == nil {
		return nil, errors.New("notify: the river enqueuer needs a client")
	}
	return &RiverEnqueuer{client: client}, nil
}

// NotifyAnnouncement queues the fan-out on the transaction that posted the
// announcement. This is the method a producing domain (catalog) calls through an
// interface it declares — the parameters are primitives, so no package imports
// this one to use it.
func (e *RiverEnqueuer) NotifyAnnouncement(ctx context.Context, tx pgx.Tx, tenantID, courseID, announcementID uuid.UUID, title, body, link string) error {
	args := AnnouncementFanoutArgs{
		TenantID: tenantID, CourseID: courseID, AnnouncementID: announcementID,
		Title: title, Body: body, Link: link,
	}
	if _, err := e.client.InsertTx(ctx, tx, args, nil); err != nil {
		return fmt.Errorf("notify: enqueue announcement fan-out %s: %w", announcementID, err)
	}
	return nil
}

// DigestArgs is the daily sweep that mails everyone a summary of their unread
// notifications. It carries nothing: it visits every tenant itself.
type DigestArgs struct{}

// Kind is stored in every river_job row, so it is a name, not a label.
func (DigestArgs) Kind() string { return "notify.digest" }

// InsertOpts retries a few times. The sweep is idempotent — a notification rolled
// into a digest is stamped, so a retry re-mails no one — so trying again is safe.
func (DigestArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 3}
}

// DigestWorker runs the daily digest.
type DigestWorker struct {
	river.WorkerDefaults[DigestArgs]

	notify *Service
	log    *slog.Logger
}

// NewDigestWorker returns a worker, refusing one it cannot run.
func NewDigestWorker(service *Service, log *slog.Logger) (*DigestWorker, error) {
	if service == nil || log == nil {
		return nil, errors.New("notify: the digest worker needs a service and a logger")
	}
	return &DigestWorker{notify: service, log: log}, nil
}

// Work mails the digests.
func (w *DigestWorker) Work(ctx context.Context, _ *river.Job[DigestArgs]) error {
	sent, err := w.notify.SendDigests(ctx)
	if err != nil {
		return fmt.Errorf("notify: send digests: %w", err)
	}
	if sent > 0 {
		w.log.InfoContext(ctx, "sent notification digests", slog.Int("people", sent))
	}
	return nil
}
