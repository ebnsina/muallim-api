package ledger

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/platform/database"
)

// Repository is the persistence contract, declared by its consumer. Every method
// takes a transaction, never the pool: the tenant is bound transaction-locally.
type Repository interface {
	CreateCategory(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewCategory) (Category, error)
	Categories(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, limit int) ([]Category, error)

	CreateEntry(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewEntry) (Entry, error)
	Entries(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, f EntryFilter, after *cursor, limit int) ([]Entry, error)
	Summary(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, f EntryFilter) ([]Total, error)
}

// AuditRecorder writes an audit line in the transaction of the thing it describes.
type AuditRecorder interface {
	Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e AuditEntry) error
}

// AuditEntry is one line of the audit log.
type AuditEntry struct {
	ActorID    *uuid.UUID
	Action     string
	TargetType string
	TargetID   string
	Metadata   map[string]any
}

// Author identifies who did something, for the audit trail.
type Author struct {
	UserID uuid.UUID
}

// Service holds the ledger rules and owns transaction boundaries.
type Service struct {
	db    *database.DB
	repo  Repository
	audit AuditRecorder
}

// NewService returns a Service.
func NewService(db *database.DB, repo Repository, recorder AuditRecorder) *Service {
	return &Service{db: db, repo: repo, audit: recorder}
}

// CreateCategory defines an income or expense head.
func (s *Service) CreateCategory(ctx context.Context, tenantID uuid.UUID, n NewCategory, author Author) (Category, error) {
	if err := n.validate(); err != nil {
		return Category{}, err
	}
	var c Category
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		c, err = s.repo.CreateCategory(ctx, tx, tenantID, n)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionCategoryCreated,
			TargetType: "ledger_category", TargetID: c.ID.String(),
			Metadata: map[string]any{"name": c.Name, "kind": c.Kind},
		})
	})
	return c, err
}

// ListCategories lists the workspace's income and expense heads.
func (s *Service) ListCategories(ctx context.Context, tenantID uuid.UUID) ([]Category, error) {
	var out []Category
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = s.repo.Categories(ctx, tx, tenantID, MaxCategories)
		return err
	})
	return out, err
}

// RecordEntry posts one dated amount against a category. A bad category is an
// ErrNotFound, not a 500.
func (s *Service) RecordEntry(ctx context.Context, tenantID uuid.UUID, n NewEntry, author Author) (Entry, error) {
	if err := n.validate(); err != nil {
		return Entry{}, err
	}
	var e Entry
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		e, err = s.repo.CreateEntry(ctx, tx, tenantID, n)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionEntryRecorded,
			TargetType: "ledger_entry", TargetID: e.ID.String(),
			Metadata: map[string]any{"amount": e.Amount, "currency": e.Currency, "category_id": e.CategoryID.String()},
		})
	})
	return e, err
}

// ListEntries lists entries, newest first, keyset-paginated, filtered by kind,
// category and/or a date range.
func (s *Service) ListEntries(ctx context.Context, tenantID uuid.UUID, f EntryFilter, p PageParams) (EntryPage, error) {
	after, err := p.decode()
	if err != nil {
		return EntryPage{}, err
	}
	limit := p.clamp()

	var page EntryPage
	err = s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := s.repo.Entries(ctx, tx, tenantID, f, after, limit+1)
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

// Summarise totals income, expense and net per currency, over the same filter as a
// listing. One query, no loop over rows.
func (s *Service) Summarise(ctx context.Context, tenantID uuid.UUID, f EntryFilter) (Summary, error) {
	var summary Summary
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		totals, err := s.repo.Summary(ctx, tx, tenantID, f)
		if err != nil {
			return err
		}
		summary.Totals = totals
		return nil
	})
	return summary, err
}
