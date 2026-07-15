// Package overview is the institution dashboard read-model: a handful of tenant-
// scoped aggregates the admin home shows at a glance. It is read-only and reaches
// the tables the other domains own by id-free aggregate, never by import — the same
// way a report card reads marks, so a count can never disagree with the rows it
// counts.
package overview

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/platform/database"
)

// AttendanceToday tallies today's register across the workspace.
type AttendanceToday struct {
	Present int
	Absent  int
	Late    int
	Excused int
	Total   int
}

// Snapshot is the institution at a glance.
type Snapshot struct {
	Students        int
	Staff           int
	Classes         int
	Subjects        int
	AttendanceToday AttendanceToday
	OutstandingFees map[string]int64 // currency → minor units still owed
	Notices         int
}

// Repository is the persistence contract, declared by its consumer.
type Repository interface {
	Load(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (Snapshot, error)
}

// Service reads the dashboard snapshot.
type Service struct {
	db   *database.DB
	repo Repository
}

// NewService returns a Service.
func NewService(db *database.DB, repo Repository) *Service {
	return &Service{db: db, repo: repo}
}

// Snapshot reads the institution's dashboard numbers in one read-only transaction.
func (s *Service) Snapshot(ctx context.Context, tenantID uuid.UUID) (Snapshot, error) {
	var snap Snapshot
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		snap, err = s.repo.Load(ctx, tx, tenantID)
		return err
	})
	return snap, err
}
