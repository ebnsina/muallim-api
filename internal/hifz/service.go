package hifz

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/platform/database"
)

// Repository is the persistence contract, declared by its consumer.
type Repository interface {
	Create(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewEntry, recordedBy uuid.UUID) (Entry, error)
	StudentLog(ctx context.Context, tx pgx.Tx, tenantID, studentID uuid.UUID, after *cursor, limit int) ([]Entry, error)
	LatestSabaq(ctx context.Context, tx pgx.Tx, tenantID, studentID uuid.UUID) (Entry, error)
	CountsByKind(ctx context.Context, tx pgx.Tx, tenantID, studentID uuid.UUID, since time.Time) (map[string]int, error)
	Delete(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) error
}

// Author identifies who recorded a recitation.
type Author struct {
	UserID uuid.UUID
}

// Service holds the hifz rules and owns transaction boundaries.
type Service struct {
	db   *database.DB
	repo Repository
}

// NewService returns a Service.
func NewService(db *database.DB, repo Repository) *Service {
	return &Service{db: db, repo: repo}
}

// Log records one recitation.
func (s *Service) Log(ctx context.Context, tenantID uuid.UUID, n NewEntry, author Author) (Entry, error) {
	if err := n.validate(); err != nil {
		return Entry{}, err
	}
	var e Entry
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		e, err = s.repo.Create(ctx, tx, tenantID, n, author.UserID)
		return err
	})
	return e, err
}

// StudentLog reads a student's recitations, newest first, keyset-paginated.
func (s *Service) StudentLog(ctx context.Context, tenantID, studentID uuid.UUID, p PageParams) (LogPage, error) {
	after, err := p.decode()
	if err != nil {
		return LogPage{}, err
	}
	limit := p.clamp()

	var page LogPage
	err = s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := s.repo.StudentLog(ctx, tx, tenantID, studentID, after, limit+1)
		if err != nil {
			return err
		}
		if len(rows) > limit {
			page.HasMore = true
			page.NextCursor = encodeCursor(rows[limit-1])
			rows = rows[:limit]
		}
		page.Entries = rows
		return nil
	})
	return page, err
}

// Summary reads where a student's Sabaq stands and how much they have recited over
// the trailing window.
func (s *Service) Summary(ctx context.Context, tenantID, studentID uuid.UUID, since time.Time) (Summary, error) {
	var sum Summary
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		latest, err := s.repo.LatestSabaq(ctx, tx, tenantID, studentID)
		switch {
		case err == nil:
			sum.CurrentSabaq = &latest
		case errors.Is(err, ErrNotFound):
			// A student with no Sabaq yet has no current position; that is not an error.
		default:
			return err
		}
		sum.Counts, err = s.repo.CountsByKind(ctx, tx, tenantID, studentID, since)
		return err
	})
	return sum, err
}

// Delete removes one log entry.
func (s *Service) Delete(ctx context.Context, tenantID, id uuid.UUID) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return s.repo.Delete(ctx, tx, tenantID, id)
	})
}
