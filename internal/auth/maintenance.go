package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/platform/database"
)

// DefaultErasureBatch bounds one sweep. Erasing is a cascade across sessions,
// enrolments, progress, and tokens; doing it in batches keeps any single
// transaction short and lets a long backlog drain over several runs rather than
// holding a connection for all of it.
const DefaultErasureBatch = 100

// MaintenanceRepository reads and writes across tenants. It is satisfied by the
// same Postgres repository, but its methods run only under WithoutTenant.
type MaintenanceRepository interface {
	OrphanedUsers(ctx context.Context, tx pgx.Tx, limit int) ([]User, error)
	DeleteUser(ctx context.Context, tx pgx.Tx, userID uuid.UUID) (bool, error)
	RevokeSessionsEverywhere(ctx context.Context, tx pgx.Tx, userID uuid.UUID) (int64, error)
}

// Maintenance performs platform-wide work that belongs to no tenant.
//
// It is a separate type from Service because it needs neither tokens nor a
// mailer, and because everything it does runs unbound. A caller holding this
// value is holding the ability to read across workspaces; there is exactly one,
// and it lives in the worker.
type Maintenance struct {
	db   *database.DB
	repo MaintenanceRepository
	log  *slog.Logger
}

// NewMaintenance returns a Maintenance, refusing to build one it cannot use.
func NewMaintenance(db *database.DB, repo MaintenanceRepository, log *slog.Logger) (*Maintenance, error) {
	if db == nil || repo == nil || log == nil {
		return nil, errors.New("auth: maintenance needs a database, a repository, and a logger")
	}
	return &Maintenance{db: db, repo: repo, log: log}, nil
}

// EraseOrphanedUsers deletes users who belong to no workspace, and returns how
// many it erased.
//
// A user is global and a membership is tenant-scoped, so deleting a workspace —
// or removing its last member — can leave a person with an account and nowhere
// to use it. Nobody can reach them through a tenant-scoped request, which is
// precisely why nobody could erase them, and why GDPR's right to erasure went
// unhonoured for the people most likely to invoke it.
//
// It is idempotent: a sweep that finds nothing does nothing, and a job retried
// after a partial batch simply finds the survivors. The database, not this code,
// decides who is an orphan: an unbound session can only see orphans, and Postgres
// applies SELECT policies to the rows a DELETE must read, so a user who joins a
// workspace between the select and the delete is spared rather than raced away.
//
// No audit entry is written. audit_log is tenant-scoped by a NOT NULL column,
// and an orphan has no tenant to file it under. The erasure is logged instead,
// by id and count, never by address: a log line naming the person who asked to
// be forgotten is its own violation.
// RevokeSessionsEverywhere ends every session a user holds, in every workspace,
// and returns how many it ended.
//
// A password is global and a session is not, so a reset performed from one
// workspace must reach the others — otherwise the device the user is trying to
// lock out stays signed in wherever they happen to belong. Only an unbound
// session can see across that boundary, which is why this is maintenance work
// and not something the reset does inline.
//
// Idempotent: sessions already revoked are not matched, so a retried job revokes
// nothing and reports zero.
func (m *Maintenance) RevokeSessionsEverywhere(ctx context.Context, userID uuid.UUID) (int64, error) {
	var revoked int64

	err := m.db.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		revoked, err = m.repo.RevokeSessionsEverywhere(ctx, tx, userID)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("auth: revoke sessions everywhere: %w", err)
	}

	if revoked > 0 {
		m.log.InfoContext(ctx, "revoked every session a user held",
			slog.String("user_id", userID.String()), slog.Int64("sessions", revoked))
	}
	return revoked, nil
}

func (m *Maintenance) EraseOrphanedUsers(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = DefaultErasureBatch
	}

	var erased int

	err := m.db.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		orphans, err := m.repo.OrphanedUsers(ctx, tx, limit)
		if err != nil {
			return err
		}

		for _, orphan := range orphans {
			deleted, err := m.repo.DeleteUser(ctx, tx, orphan.ID)
			if err != nil {
				return err
			}
			if !deleted {
				// They joined a workspace between the select and the delete. Not an
				// error: the policy did its job, and they are no longer an orphan.
				m.log.InfoContext(ctx, "skipped erasing a user who is no longer orphaned",
					slog.String("user_id", orphan.ID.String()))
				continue
			}

			erased++
			m.log.InfoContext(ctx, "erased an orphaned user",
				slog.String("user_id", orphan.ID.String()))
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("auth: erase orphaned users: %w", err)
	}

	return erased, nil
}
