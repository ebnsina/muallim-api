package fees

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/platform/database"
)

// Repository is the persistence contract, declared by its consumer. Every method
// takes a transaction, never the pool: the tenant is bound transaction-locally.
type Repository interface {
	CreateStructure(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewFeeStructure) (FeeStructure, error)
	Structures(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, limit int) ([]FeeStructure, error)
	StructureByID(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) (FeeStructure, error)
	DeleteStructure(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) error

	CreateInvoice(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewInvoice) (Invoice, error)
	IssueBatch(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, s FeeStructure, b Batch) (int, error)
	Invoices(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, f InvoiceFilter, after *cursor, limit int) ([]Invoice, error)
	StudentInvoices(ctx context.Context, tx pgx.Tx, tenantID, studentID uuid.UUID) ([]Invoice, error)
	RecordPayment(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID, p Payment) (Invoice, error)
	SetStatus(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID, from, to string) (Invoice, error)
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

// Service holds the billing rules and owns transaction boundaries.
type Service struct {
	db    *database.DB
	repo  Repository
	audit AuditRecorder
}

// NewService returns a Service.
func NewService(db *database.DB, repo Repository, recorder AuditRecorder) *Service {
	return &Service{db: db, repo: repo, audit: recorder}
}

// CreateStructure defines a recurring fee.
func (s *Service) CreateStructure(ctx context.Context, tenantID uuid.UUID, n NewFeeStructure, author Author) (FeeStructure, error) {
	if err := n.validate(); err != nil {
		return FeeStructure{}, err
	}
	var fs FeeStructure
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		fs, err = s.repo.CreateStructure(ctx, tx, tenantID, n)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionStructureCreated,
			TargetType: "fee_structure", TargetID: fs.ID.String(),
			Metadata: map[string]any{"name": fs.Name, "amount": fs.Amount, "currency": fs.Currency},
		})
	})
	return fs, err
}

// Structures lists the workspace's fee structures.
func (s *Service) Structures(ctx context.Context, tenantID uuid.UUID) ([]FeeStructure, error) {
	var out []FeeStructure
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = s.repo.Structures(ctx, tx, tenantID, MaxStructures)
		return err
	})
	return out, err
}

// DeleteStructure removes a fee structure. Invoices already raised keep their own
// copy of the amount and merely lose the link.
func (s *Service) DeleteStructure(ctx context.Context, tenantID, id uuid.UUID) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return s.repo.DeleteStructure(ctx, tx, tenantID, id)
	})
}

// IssueInvoice raises one ad-hoc invoice against a student.
func (s *Service) IssueInvoice(ctx context.Context, tenantID uuid.UUID, n NewInvoice, author Author) (Invoice, error) {
	if err := n.validate(); err != nil {
		return Invoice{}, err
	}
	var inv Invoice
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		inv, err = s.repo.CreateInvoice(ctx, tx, tenantID, n)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionInvoiceIssued,
			TargetType: "fee_invoice", TargetID: inv.ID.String(),
			Metadata: map[string]any{"amount": inv.Amount, "currency": inv.Currency},
		})
	})
	return inv, err
}

// IssueBatch bills a fee structure to a set of students, or a whole class, for one
// period. It is idempotent: re-running the same period skips students already
// billed for it, so nobody is charged twice.
func (s *Service) IssueBatch(ctx context.Context, tenantID uuid.UUID, b Batch, author Author) (int, error) {
	if len(b.StudentIDs) == 0 && b.GradeLevelID == nil {
		return 0, ErrNoTarget
	}
	if len(b.StudentIDs) > MaxBatch {
		return 0, ErrInvalidInvoice
	}
	var issued int
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		structure, err := s.repo.StructureByID(ctx, tx, tenantID, b.StructureID)
		if err != nil {
			return err
		}
		issued, err = s.repo.IssueBatch(ctx, tx, tenantID, structure, b)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionInvoicesIssued,
			TargetType: "fee_structure", TargetID: b.StructureID.String(),
			Metadata: map[string]any{"period": b.Period, "issued": issued},
		})
	})
	return issued, err
}

// Invoices lists invoices, newest first, keyset-paginated, filtered by student
// and/or status.
func (s *Service) Invoices(ctx context.Context, tenantID uuid.UUID, f InvoiceFilter, p PageParams) (InvoicePage, error) {
	after, err := p.decode()
	if err != nil {
		return InvoicePage{}, err
	}
	limit := p.clamp()

	var page InvoicePage
	err = s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := s.repo.Invoices(ctx, tx, tenantID, f, after, limit+1)
		if err != nil {
			return err
		}
		if len(rows) > limit {
			page.HasMore = true
			page.NextCursor = encodeCursor(rows[limit-1])
			rows = rows[:limit]
		}
		page.Invoices = rows
		return nil
	})
	return page, err
}

// StudentLedger is a student's invoices and what they still owe, per currency.
func (s *Service) StudentLedger(ctx context.Context, tenantID, studentID uuid.UUID) (Ledger, error) {
	var ledger Ledger
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		invoices, err := s.repo.StudentInvoices(ctx, tx, tenantID, studentID)
		if err != nil {
			return err
		}
		ledger.Invoices = invoices
		ledger.Outstanding = map[string]int64{}
		for _, inv := range invoices {
			if inv.Status == StatusUnpaid {
				ledger.Outstanding[inv.Currency] += inv.Amount
			}
		}
		return nil
	})
	return ledger, err
}

// RecordPayment marks an unpaid invoice paid. The `WHERE status = 'unpaid'` guard
// makes a double submission harmless — the second finds nothing to pay.
func (s *Service) RecordPayment(ctx context.Context, tenantID, id uuid.UUID, p Payment, author Author) (Invoice, error) {
	if err := p.validate(); err != nil {
		return Invoice{}, err
	}
	var inv Invoice
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		inv, err = s.repo.RecordPayment(ctx, tx, tenantID, id, p)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionPaymentRecorded,
			TargetType: "fee_invoice", TargetID: inv.ID.String(),
			Metadata: map[string]any{"amount": p.Amount, "method": p.Method},
		})
	})
	return inv, err
}

// WaiveInvoice forgives an unpaid invoice.
func (s *Service) WaiveInvoice(ctx context.Context, tenantID, id uuid.UUID, author Author) (Invoice, error) {
	return s.transition(ctx, tenantID, id, StatusUnpaid, StatusWaived, ActionInvoiceWaived, author)
}

// CancelInvoice voids an unpaid invoice raised in error.
func (s *Service) CancelInvoice(ctx context.Context, tenantID, id uuid.UUID, author Author) (Invoice, error) {
	return s.transition(ctx, tenantID, id, StatusUnpaid, StatusCancelled, ActionInvoiceCancelled, author)
}

func (s *Service) transition(ctx context.Context, tenantID, id uuid.UUID, from, to, action string, author Author) (Invoice, error) {
	var inv Invoice
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		inv, err = s.repo.SetStatus(ctx, tx, tenantID, id, from, to)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: action,
			TargetType: "fee_invoice", TargetID: inv.ID.String(),
		})
	})
	return inv, err
}
