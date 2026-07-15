// Package fees models institutional billing: fee structures a school defines and
// the invoices it raises against its students. It knows nothing about HTTP, and
// references the academic spine (students, classes) by id, never by import. Money
// is bigint minor units + a currency, defaulting to BDT poisha — never a float.
package fees

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
	ErrNotFound         = errors.New("fees: not found")
	ErrInvalidStructure = errors.New("fees: the fee structure is not valid")
	ErrInvalidInvoice   = errors.New("fees: the invoice is not valid")
	ErrInvalidPayment   = errors.New("fees: the payment is not valid")
	ErrNotUnpaid        = errors.New("fees: only an unpaid invoice can change this way")
	ErrNoTarget         = errors.New("fees: name the students or a class to bill")
	ErrInvalidPage      = errors.New("fees: the page cursor is not valid")
)

// Recurrence of a fee structure.
const (
	OneTime = "one_time"
	Monthly = "monthly"
	Termly  = "termly"
	Annual  = "annual"
)

// Invoice status.
const (
	StatusUnpaid    = "unpaid"
	StatusPaid      = "paid"
	StatusWaived    = "waived"
	StatusCancelled = "cancelled"
)

// DefaultCurrency is BDT — the primary market. An international workspace passes
// its own; the column is char(3) either way.
const DefaultCurrency = "BDT"

// Bounds.
const (
	MaxStructures = 200
	MaxBatch      = 2000
)

// Audit actions.
const (
	ActionStructureCreated = "fee_structure.created"
	ActionInvoiceIssued    = "fee_invoice.issued"
	ActionInvoicesIssued   = "fee_invoice.batch_issued"
	ActionPaymentRecorded  = "fee_invoice.paid"
	ActionInvoiceWaived    = "fee_invoice.waived"
	ActionInvoiceCancelled = "fee_invoice.cancelled"
)

// FeeStructure is a recurring charge a school defines.
type FeeStructure struct {
	ID           uuid.UUID
	Name         string
	Amount       int64
	Currency     string
	GradeLevelID *uuid.UUID
	Recurrence   string
	CreatedAt    time.Time
}

// NewFeeStructure is a structure to create.
type NewFeeStructure struct {
	Name         string
	Amount       int64
	Currency     string
	GradeLevelID *uuid.UUID
	Recurrence   string
}

// Invoice is one charge raised against one student.
type Invoice struct {
	ID             uuid.UUID
	StudentID      uuid.UUID
	FeeStructureID *uuid.UUID
	Title          string
	Amount         int64
	Currency       string
	Period         string
	DueDate        *time.Time
	Status         string
	PaidAmount     int64
	PaidAt         *time.Time
	Method         string
	Note           string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// NewInvoice is an ad-hoc invoice against one student, outside any structure.
type NewInvoice struct {
	StudentID uuid.UUID
	Title     string
	Amount    int64
	Currency  string
	DueDate   *time.Time
	Note      string
}

// Batch issues a structure's fee to a set of students, or a whole class, for one
// period. Idempotent on (student, structure, period): re-running a month bills
// nobody twice.
type Batch struct {
	StructureID  uuid.UUID
	Period       string
	DueDate      *time.Time
	GradeLevelID *uuid.UUID
	StudentIDs   []uuid.UUID
}

// Payment records money collected against an invoice.
type Payment struct {
	Amount int64
	Method string
	Note   string
}

// Ledger is one student's invoices and what they still owe, per currency.
type Ledger struct {
	Invoices    []Invoice
	Outstanding map[string]int64
}

// InvoiceFilter narrows a listing.
type InvoiceFilter struct {
	StudentID *uuid.UUID
	Status    string
}

func normaliseCurrency(c string) string {
	c = strings.ToUpper(strings.TrimSpace(c))
	if c == "" {
		return DefaultCurrency
	}
	return c
}

func validRecurrence(r string) bool {
	switch r {
	case OneTime, Monthly, Termly, Annual:
		return true
	default:
		return false
	}
}

func (n *NewFeeStructure) validate() error {
	if n.Name == "" {
		return fmt.Errorf("%w: name it", ErrInvalidStructure)
	}
	if n.Amount < 0 {
		return fmt.Errorf("%w: the amount cannot be negative", ErrInvalidStructure)
	}
	if n.Recurrence == "" {
		n.Recurrence = OneTime
	}
	if !validRecurrence(n.Recurrence) {
		return fmt.Errorf("%w: %q is not a recurrence", ErrInvalidStructure, n.Recurrence)
	}
	n.Currency = normaliseCurrency(n.Currency)
	if len(n.Currency) != 3 {
		return fmt.Errorf("%w: a currency is three letters", ErrInvalidStructure)
	}
	return nil
}

func (n *NewInvoice) validate() error {
	if n.Title == "" {
		return fmt.Errorf("%w: give it a title", ErrInvalidInvoice)
	}
	if n.Amount < 0 {
		return fmt.Errorf("%w: the amount cannot be negative", ErrInvalidInvoice)
	}
	n.Currency = normaliseCurrency(n.Currency)
	if len(n.Currency) != 3 {
		return fmt.Errorf("%w: a currency is three letters", ErrInvalidInvoice)
	}
	return nil
}

func (p Payment) validate() error {
	if p.Amount <= 0 {
		return fmt.Errorf("%w: a payment must be positive", ErrInvalidPayment)
	}
	return nil
}
