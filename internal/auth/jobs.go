package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

// RevokeSessionsArgs ends every session a user holds, in every workspace.
//
// It carries a user and no tenant, because that is the point: a session lives in
// one workspace and a password lives in none.
type RevokeSessionsArgs struct {
	UserID uuid.UUID `json:"user_id"`
}

// Kind is stored in every river_job row, so it is a name and not a label.
func (RevokeSessionsArgs) Kind() string { return "auth.revoke_sessions" }

// InsertOpts retries generously. Every attempt that fails leaves a session alive
// that a password change was meant to end.
func (RevokeSessionsArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 10}
}

// RevokeSessionsWorker runs the cross-tenant revocation.
type RevokeSessionsWorker struct {
	river.WorkerDefaults[RevokeSessionsArgs]

	maintenance *Maintenance
}

// NewRevokeSessionsWorker returns a worker, refusing one it cannot run.
func NewRevokeSessionsWorker(maintenance *Maintenance) (*RevokeSessionsWorker, error) {
	if maintenance == nil {
		return nil, errors.New("auth: revoke-sessions worker needs a maintenance service")
	}
	return &RevokeSessionsWorker{maintenance: maintenance}, nil
}

// Work revokes the sessions. Idempotent: an already-revoked session is not
// matched, so a retry revokes nothing and succeeds.
func (w *RevokeSessionsWorker) Work(ctx context.Context, job *river.Job[RevokeSessionsArgs]) error {
	if _, err := w.maintenance.RevokeSessionsEverywhere(ctx, job.Args.UserID); err != nil {
		return fmt.Errorf("auth: revoke sessions job: %w", err)
	}
	return nil
}

// Enqueuer queues the work this package cannot do inside a tenant-bound
// transaction. Declared here, by its consumer; cmd/ supplies a River client.
//
// The method takes the caller's transaction, so the job and the password change
// commit together. A password changed without the job is a device still signed
// in somewhere; a job without the password change is a user logged out for
// nothing.
type Enqueuer interface {
	RevokeSessionsEverywhere(ctx context.Context, tx pgx.Tx, userID uuid.UUID) error
}

// RiverEnqueuer adapts a River client to Enqueuer.
//
// It lives here rather than in cmd/ because the job arguments do, and a caller
// that had to construct them itself could construct them wrongly.
type RiverEnqueuer struct {
	client *river.Client[pgx.Tx]
	log    *slog.Logger
}

// NewRiverEnqueuer returns an Enqueuer backed by River.
func NewRiverEnqueuer(client *river.Client[pgx.Tx], log *slog.Logger) (*RiverEnqueuer, error) {
	if client == nil || log == nil {
		return nil, errors.New("auth: river enqueuer needs a client and a logger")
	}
	return &RiverEnqueuer{client: client, log: log}, nil
}

// RevokeSessionsEverywhere queues the revocation on the caller's transaction.
func (e *RiverEnqueuer) RevokeSessionsEverywhere(ctx context.Context, tx pgx.Tx, userID uuid.UUID) error {
	if _, err := e.client.InsertTx(ctx, tx, RevokeSessionsArgs{UserID: userID}, nil); err != nil {
		return fmt.Errorf("auth: enqueue session revocation for %s: %w", userID, err)
	}
	return nil
}
