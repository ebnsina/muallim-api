package library

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/platform/database"
)

// Repository is the persistence contract, declared by its consumer. Every method
// takes a transaction, never the pool: the tenant is bound transaction-locally.
type Repository interface {
	AddBook(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewBook) (Book, error)
	Books(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, f BookFilter, after *cursor, limit int) ([]Book, error)

	IssueLoan(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewLoan) (Loan, error)
	ReturnLoan(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) (Loan, error)
	Loans(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, f LoanFilter, after *cursor, limit int) ([]Loan, error)
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

// Service holds the library rules and owns transaction boundaries.
type Service struct {
	db    *database.DB
	repo  Repository
	audit AuditRecorder
}

// NewService returns a Service.
func NewService(db *database.DB, repo Repository, recorder AuditRecorder) *Service {
	return &Service{db: db, repo: repo, audit: recorder}
}

// AddBook adds a title to the catalogue, with its copies free to lend.
func (s *Service) AddBook(ctx context.Context, tenantID uuid.UUID, n NewBook, author Author) (Book, error) {
	if err := n.validate(); err != nil {
		return Book{}, err
	}
	var b Book
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		b, err = s.repo.AddBook(ctx, tx, tenantID, n)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionBookAdded,
			TargetType: "library_book", TargetID: b.ID.String(),
			Metadata: map[string]any{"title": b.Title, "copies": b.TotalCopies},
		})
	})
	return b, err
}

// ListBooks lists the catalogue, newest first, keyset-paginated, optionally
// narrowed to a category.
func (s *Service) ListBooks(ctx context.Context, tenantID uuid.UUID, f BookFilter, p PageParams) (BookPage, error) {
	after, err := p.decode()
	if err != nil {
		return BookPage{}, err
	}
	limit := p.clamp()

	var page BookPage
	err = s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := s.repo.Books(ctx, tx, tenantID, f, after, limit+1)
		if err != nil {
			return err
		}
		if len(rows) > limit {
			page.HasMore = true
			page.NextCursor = encodeBookCursor(rows[limit-1])
			rows = rows[:limit]
		}
		page.Books = rows
		return nil
	})
	return page, err
}

// IssueLoan lends one copy of a book to a student. The available-copies guard is
// in the same statement as the decrement, so two concurrent issues cannot both
// draw the last copy.
func (s *Service) IssueLoan(ctx context.Context, tenantID uuid.UUID, n NewLoan, author Author) (Loan, error) {
	if err := n.validate(); err != nil {
		return Loan{}, err
	}
	var l Loan
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		l, err = s.repo.IssueLoan(ctx, tx, tenantID, n)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionLoanIssued,
			TargetType: "library_loan", TargetID: l.ID.String(),
			Metadata: map[string]any{"book_id": l.BookID.String(), "student_id": l.StudentID.String()},
		})
	})
	return l, err
}

// ReturnLoan marks an outstanding loan returned and puts its copy back. The
// `WHERE status = 'out'` guard makes a double submission harmless — the second
// finds nothing to return.
func (s *Service) ReturnLoan(ctx context.Context, tenantID, id uuid.UUID, author Author) (Loan, error) {
	var l Loan
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		l, err = s.repo.ReturnLoan(ctx, tx, tenantID, id)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionLoanReturned,
			TargetType: "library_loan", TargetID: l.ID.String(),
			Metadata: map[string]any{"book_id": l.BookID.String()},
		})
	})
	return l, err
}

// ListLoans lists loans, newest first, keyset-paginated, filtered by student
// and/or status.
func (s *Service) ListLoans(ctx context.Context, tenantID uuid.UUID, f LoanFilter, p PageParams) (LoanPage, error) {
	after, err := p.decode()
	if err != nil {
		return LoanPage{}, err
	}
	limit := p.clamp()

	var page LoanPage
	err = s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := s.repo.Loans(ctx, tx, tenantID, f, after, limit+1)
		if err != nil {
			return err
		}
		if len(rows) > limit {
			page.HasMore = true
			page.NextCursor = encodeLoanCursor(rows[limit-1])
			rows = rows[:limit]
		}
		page.Loans = rows
		return nil
	})
	return page, err
}
