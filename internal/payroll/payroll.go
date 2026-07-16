// Package payroll models how a school pays its staff: a salary structure per staff
// member and the payslips generated from it. It knows nothing about HTTP, and
// references staff by id, never by import. Money is bigint minor units + a currency,
// defaulting to BDT poisha — never a float.
package payroll

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Sentinels. internal/httpapi maps each to a status; add a line to errors_test.go
// in the same commit as a new one.
var (
	ErrNotFound         = errors.New("payroll: not found")
	ErrNoSalary         = errors.New("payroll: the staff member has no salary structure")
	ErrNotDraft         = errors.New("payroll: only a draft payslip can be paid")
	ErrInvalidStructure = errors.New("payroll: the salary structure is not valid")
	ErrInvalidPayslip   = errors.New("payroll: the payslip request is not valid")
	ErrInvalidPage      = errors.New("payroll: the page cursor is not valid")
)

// Payslip status.
const (
	StatusDraft = "draft"
	StatusPaid  = "paid"
)

// DefaultCurrency is BDT — the primary market. An international workspace passes
// its own; the column is char(3) either way.
const DefaultCurrency = "BDT"

// MaxPayslips bounds a student's or listing's rows before a cursor is required.
const MaxPayslips = 500

// Audit actions.
const (
	ActionSalarySet         = "payroll_salary.set"
	ActionPayslipsGenerated = "payroll_payslip.batch_generated"
	ActionPayslipPaid       = "payroll_payslip.paid"
)

// SalaryStructure is what a school pays one staff member each period.
type SalaryStructure struct {
	ID               uuid.UUID
	StaffID          uuid.UUID
	BasicAmount      int64
	AllowancesAmount int64
	DeductionsAmount int64
	Currency         string
	EffectiveFrom    *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// NewSalary is a salary structure to set for a staff member.
type NewSalary struct {
	StaffID          uuid.UUID
	BasicAmount      int64
	AllowancesAmount int64
	DeductionsAmount int64
	Currency         string
	EffectiveFrom    *time.Time
}

// Payslip is one period's pay computed from a salary structure. Net is gross less
// deductions; a draft becomes paid when the money leaves.
type Payslip struct {
	ID               uuid.UUID
	StaffID          uuid.UUID
	Period           string
	GrossAmount      int64
	DeductionsAmount int64
	NetAmount        int64
	Currency         string
	Status           string
	GeneratedAt      time.Time
	PaidAt           *time.Time
	Method           string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// Batch generates one payslip per staff member with a salary structure, for one
// period. Idempotent on (staff, period): re-running a month pays nobody twice. A
// nil StaffID generates for the whole workspace; a set one, for that person alone.
type Batch struct {
	Period  string
	StaffID *uuid.UUID
}

// PayslipFilter narrows a listing.
type PayslipFilter struct {
	StaffID *uuid.UUID
	Period  string
	Status  string
}

func normaliseCurrency(c string) string {
	c = strings.ToUpper(strings.TrimSpace(c))
	if c == "" {
		return DefaultCurrency
	}
	return c
}

func (n *NewSalary) validate() error {
	if n.BasicAmount < 0 || n.AllowancesAmount < 0 || n.DeductionsAmount < 0 {
		return fmt.Errorf("%w: amounts cannot be negative", ErrInvalidStructure)
	}
	if n.DeductionsAmount > n.BasicAmount+n.AllowancesAmount {
		return fmt.Errorf("%w: deductions exceed gross pay", ErrInvalidStructure)
	}
	n.Currency = normaliseCurrency(n.Currency)
	if len(n.Currency) != 3 {
		return fmt.Errorf("%w: a currency is three letters", ErrInvalidStructure)
	}
	return nil
}

func (b Batch) validate() error {
	if strings.TrimSpace(b.Period) == "" {
		return fmt.Errorf("%w: name the pay period", ErrInvalidPayslip)
	}
	return nil
}
