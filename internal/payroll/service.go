package payroll

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/platform/database"
)

// Repository is the persistence contract, declared by its consumer. Every method
// takes a transaction, never the pool: the tenant is bound transaction-locally.
type Repository interface {
	UpsertSalary(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewSalary) (SalaryStructure, error)
	SalaryByStaff(ctx context.Context, tx pgx.Tx, tenantID, staffID uuid.UUID) (SalaryStructure, error)

	GeneratePayslips(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, b Batch) (int, error)
	Payslips(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, f PayslipFilter, after *cursor, limit int) ([]Payslip, error)
	MarkPaid(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID, method string) (Payslip, error)
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

// Service holds the payroll rules and owns transaction boundaries.
type Service struct {
	db    *database.DB
	repo  Repository
	audit AuditRecorder
}

// NewService returns a Service.
func NewService(db *database.DB, repo Repository, recorder AuditRecorder) *Service {
	return &Service{db: db, repo: repo, audit: recorder}
}

// SetSalary sets or replaces a staff member's salary structure. Setting it again
// updates the current one in place — there is one per staff member.
func (s *Service) SetSalary(ctx context.Context, tenantID uuid.UUID, n NewSalary, author Author) (SalaryStructure, error) {
	if err := n.validate(); err != nil {
		return SalaryStructure{}, err
	}
	var st SalaryStructure
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		st, err = s.repo.UpsertSalary(ctx, tx, tenantID, n)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionSalarySet,
			TargetType: "payroll_salary", TargetID: st.StaffID.String(),
			Metadata: map[string]any{"basic": st.BasicAmount, "currency": st.Currency},
		})
	})
	return st, err
}

// GetSalary reads a staff member's salary structure, or ErrNotFound.
func (s *Service) GetSalary(ctx context.Context, tenantID, staffID uuid.UUID) (SalaryStructure, error) {
	var st SalaryStructure
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		st, err = s.repo.SalaryByStaff(ctx, tx, tenantID, staffID)
		return err
	})
	return st, err
}

// GeneratePayslips draws one payslip per staff member with a salary structure for a
// period, computing net = basic + allowances - deductions. It is idempotent:
// re-running the same period pays nobody twice. Naming one staff member with no
// salary structure is refused; a workspace-wide run simply skips the unpaid.
func (s *Service) GeneratePayslips(ctx context.Context, tenantID uuid.UUID, b Batch, author Author) (int, error) {
	if err := b.validate(); err != nil {
		return 0, err
	}
	var generated int
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if b.StaffID != nil {
			if _, err := s.repo.SalaryByStaff(ctx, tx, tenantID, *b.StaffID); err != nil {
				if errors.Is(err, ErrNotFound) {
					return ErrNoSalary
				}
				return err
			}
		}
		var err error
		generated, err = s.repo.GeneratePayslips(ctx, tx, tenantID, b)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionPayslipsGenerated,
			TargetType: "payroll_payslip", TargetID: b.Period,
			Metadata: map[string]any{"period": b.Period, "generated": generated},
		})
	})
	return generated, err
}

// ListPayslips lists payslips, newest first, keyset-paginated, filtered by staff,
// period and/or status.
func (s *Service) ListPayslips(ctx context.Context, tenantID uuid.UUID, f PayslipFilter, p PageParams) (Page, error) {
	after, err := p.decode()
	if err != nil {
		return Page{}, err
	}
	limit := p.clamp()

	var page Page
	err = s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := s.repo.Payslips(ctx, tx, tenantID, f, after, limit+1)
		if err != nil {
			return err
		}
		if len(rows) > limit {
			page.HasMore = true
			page.NextCursor = encodeCursor(rows[limit-1])
			rows = rows[:limit]
		}
		page.Payslips = rows
		return nil
	})
	return page, err
}

// MarkPaid records that a draft payslip was paid. The `WHERE status = 'draft'`
// guard makes a double submission harmless — the second finds nothing to pay.
func (s *Service) MarkPaid(ctx context.Context, tenantID, id uuid.UUID, method string, author Author) (Payslip, error) {
	var ps Payslip
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		ps, err = s.repo.MarkPaid(ctx, tx, tenantID, id, method)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionPayslipPaid,
			TargetType: "payroll_payslip", TargetID: ps.ID.String(),
			Metadata: map[string]any{"amount": ps.NetAmount, "method": method},
		})
	})
	return ps, err
}
