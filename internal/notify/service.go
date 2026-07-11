package notify

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/lms-api/internal/platform/database"
)

// Repository is where notifications live. Every method takes a transaction,
// never the pool: app.tenant_id is bound transaction-locally.
type Repository interface {
	// Insert writes one notification. Called by a producer inside its own
	// transaction, so the notice and the event it describes commit together.
	Insert(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n Notification) error

	// List returns one keyset page of a person's notifications, newest first,
	// asking for limit+1 so the caller can tell whether a next page exists.
	List(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID, before *cursor, limit int) ([]Notification, error)

	// UnreadCount is the person's unread total, from the partial index.
	UnreadCount(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID) (int, error)

	// MarkRead marks one notification read, scoped to its owner. Returns whether a
	// row was theirs to mark.
	MarkRead(ctx context.Context, tx pgx.Tx, tenantID, userID, id uuid.UUID) (bool, error)

	// MarkAllRead marks every unread notification of a person read.
	MarkAllRead(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID) error

	// FanOutAnnouncement writes one notification per enrolled learner of a course,
	// idempotently, returning how many were newly created.
	FanOutAnnouncement(ctx context.Context, tx pgx.Tx, tenantID, courseID, announcementID uuid.UUID, title, body, link string) (int, error)
}

// Service holds the rules and owns the transaction boundaries.
type Service struct {
	db   *database.DB
	repo Repository
}

// NewService returns a Service.
func NewService(db *database.DB, repo Repository) *Service {
	return &Service{db: db, repo: repo}
}

// Record writes a notification inside the caller's transaction. This is the
// producer entry point — a domain calls it (through an interface it declares)
// while committing the event the notice is about, so the two are atomic. An empty
// recipient is a no-op rather than an error: an event with nobody to tell (a
// question whose author has been erased) simply notifies no one.
func (s *Service) Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n Notification) error {
	if n.UserID == uuid.Nil {
		return nil
	}
	return s.repo.Insert(ctx, tx, tenantID, n)
}

// List returns one keyset page of a person's notifications, newest first.
func (s *Service) List(ctx context.Context, tenantID, userID uuid.UUID, token string, limit int) (Page, error) {
	if limit <= 0 || limit > MaxPageSize {
		limit = DefaultPageSize
	}

	var before *cursor
	if token != "" {
		c, err := decodeCursor(token)
		if err != nil {
			return Page{}, err
		}
		before = &c
	}

	var page Page
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		// limit+1 to detect a next page without a COUNT.
		rows, err := s.repo.List(ctx, tx, tenantID, userID, before, limit+1)
		if err != nil {
			return err
		}
		if len(rows) > limit {
			last := rows[limit-1]
			page.NextCursor = cursor{CreatedAt: last.CreatedAt, ID: last.ID}.encode()
			rows = rows[:limit]
		}
		page.Notifications = rows
		return nil
	})
	return page, err
}

// UnreadCount is the number of notifications the person has not yet seen.
func (s *Service) UnreadCount(ctx context.Context, tenantID, userID uuid.UUID) (int, error) {
	var count int
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		count, err = s.repo.UnreadCount(ctx, tx, tenantID, userID)
		return err
	})
	return count, err
}

// MarkRead marks one of the caller's notifications read. Not theirs, or not
// there, is ErrNotFound — one answer, so neither reveals the other.
func (s *Service) MarkRead(ctx context.Context, tenantID, userID, id uuid.UUID) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		marked, err := s.repo.MarkRead(ctx, tx, tenantID, userID, id)
		if err != nil {
			return err
		}
		if !marked {
			return ErrNotFound
		}
		return nil
	})
}

// MarkAllRead clears the caller's whole unread count in one statement.
func (s *Service) MarkAllRead(ctx context.Context, tenantID, userID uuid.UUID) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return s.repo.MarkAllRead(ctx, tx, tenantID, userID)
	})
}

// FanOutAnnouncement notifies every enrolled learner of a course that an
// announcement was posted. It owns its own transaction: the job worker runs it
// out of band, after the announcement has already committed, so a slow fan-out
// never holds up the instructor's request. Returns how many were newly notified.
func (s *Service) FanOutAnnouncement(ctx context.Context, tenantID, courseID, announcementID uuid.UUID, title, body, link string) (int, error) {
	var created int
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		created, err = s.repo.FanOutAnnouncement(ctx, tx, tenantID, courseID, announcementID, title, body, link)
		return err
	})
	return created, err
}
