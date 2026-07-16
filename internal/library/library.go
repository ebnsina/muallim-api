// Package library models an institution's library: the books a school holds and
// the loans it makes to its students. It knows nothing about HTTP, and references
// the academic spine (students) by id, never by import. A book carries a copy
// count; issuing a loan draws a copy and returning one puts it back.
package library

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Sentinels. internal/httpapi maps each to a status; add a line to
// library_errors_test.go in the same commit as a new one.
var (
	ErrNotFound        = errors.New("library: not found")
	ErrNoCopies        = errors.New("library: no copies are available to lend")
	ErrAlreadyReturned = errors.New("library: that loan is already returned")
	ErrInvalidBook     = errors.New("library: the book is not valid")
	ErrInvalidLoan     = errors.New("library: the loan is not valid")
	ErrInvalidPage     = errors.New("library: the page cursor is not valid")
)

// Loan status.
const (
	StatusOut      = "out"
	StatusReturned = "returned"
)

// Bounds.
const (
	MaxCopies   = 100000
	LoanMaxDays = 3650
)

// Audit actions.
const (
	ActionBookAdded    = "library_book.added"
	ActionLoanIssued   = "library_loan.issued"
	ActionLoanReturned = "library_loan.returned"
)

// Book is one title the library holds, with a count of copies and how many are
// free to lend right now.
type Book struct {
	ID              uuid.UUID
	Title           string
	Author          string
	ISBN            *string
	Category        *string
	TotalCopies     int
	AvailableCopies int
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// NewBook is a book to add to the catalogue.
type NewBook struct {
	Title       string
	Author      string
	ISBN        *string
	Category    *string
	TotalCopies int
}

// Loan is one book lent to one student, out until it is returned.
type Loan struct {
	ID         uuid.UUID
	BookID     uuid.UUID
	StudentID  uuid.UUID
	BorrowedAt time.Time
	DueAt      time.Time
	ReturnedAt *time.Time
	Status     string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// NewLoan lends one copy of a book to a student until a due date.
type NewLoan struct {
	BookID    uuid.UUID
	StudentID uuid.UUID
	DueAt     time.Time
}

// BookFilter narrows the catalogue listing.
type BookFilter struct {
	Category string
}

// LoanFilter narrows the loans listing.
type LoanFilter struct {
	StudentID *uuid.UUID
	Status    string
}

func trimPtr(p *string) *string {
	if p == nil {
		return nil
	}
	v := strings.TrimSpace(*p)
	if v == "" {
		return nil
	}
	return &v
}

func (n *NewBook) validate() error {
	n.Title = strings.TrimSpace(n.Title)
	if n.Title == "" {
		return fmt.Errorf("%w: give it a title", ErrInvalidBook)
	}
	n.Author = strings.TrimSpace(n.Author)
	n.ISBN = trimPtr(n.ISBN)
	n.Category = trimPtr(n.Category)
	if n.TotalCopies <= 0 {
		n.TotalCopies = 1
	}
	if n.TotalCopies > MaxCopies {
		return fmt.Errorf("%w: too many copies", ErrInvalidBook)
	}
	return nil
}

func (n *NewLoan) validate() error {
	if n.BookID == uuid.Nil {
		return fmt.Errorf("%w: name the book", ErrInvalidLoan)
	}
	if n.StudentID == uuid.Nil {
		return fmt.Errorf("%w: name the student", ErrInvalidLoan)
	}
	if n.DueAt.IsZero() {
		n.DueAt = time.Now().Add(14 * 24 * time.Hour)
	}
	if n.DueAt.After(time.Now().Add(LoanMaxDays * 24 * time.Hour)) {
		return fmt.Errorf("%w: the due date is too far off", ErrInvalidLoan)
	}
	return nil
}
