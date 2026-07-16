// Package ledger models a school's own books: income and expense heads a workspace
// defines, and the dated amounts posted against them. It knows nothing about HTTP,
// and records money that has already moved — it never moves any. Money is bigint
// minor units + a currency, defaulting to BDT poisha, never a float.
//
// This is the institution's own accounting. It is separate from `fees` (which bills
// students) and `commerce` (a learner buying a course).
package ledger

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Sentinels. internal/httpapi maps each to a status; add a line to
// ledger_errors_test.go in the same commit as a new one.
var (
	ErrNotFound        = errors.New("ledger: not found")
	ErrInvalidCategory = errors.New("ledger: the category is not valid")
	ErrInvalidEntry    = errors.New("ledger: the entry is not valid")
	ErrInvalidPage     = errors.New("ledger: the page cursor is not valid")
)

// Category kinds. A head is either money in or money out.
const (
	KindIncome  = "income"
	KindExpense = "expense"
)

// ValidKind reports whether k is a category kind.
func ValidKind(k string) bool {
	return k == KindIncome || k == KindExpense
}

// DefaultCurrency is BDT — the primary market. An international workspace passes its
// own; the column is char(3) either way.
const DefaultCurrency = "BDT"

// Bounds.
const MaxCategories = 200

// Audit actions.
const (
	ActionCategoryCreated = "ledger_category.created"
	ActionEntryRecorded   = "ledger_entry.recorded"
)

// Category is a named income or expense head.
type Category struct {
	ID        uuid.UUID
	Name      string
	Kind      string
	CreatedAt time.Time
}

// NewCategory is a category to create.
type NewCategory struct {
	Name string
	Kind string
}

// Entry is one dated amount posted against a category.
type Entry struct {
	ID          uuid.UUID
	CategoryID  uuid.UUID
	Amount      int64
	Currency    string
	OccurredOn  time.Time
	Description string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// NewEntry is an entry to record.
type NewEntry struct {
	CategoryID  uuid.UUID
	Amount      int64
	Currency    string
	OccurredOn  time.Time
	Description string
}

// EntryFilter narrows a listing.
type EntryFilter struct {
	Kind       string
	CategoryID *uuid.UUID
	From       *time.Time
	To         *time.Time
}

// Total is the income, expense and net for one currency.
type Total struct {
	Currency string
	Income   int64
	Expense  int64
	Net      int64
}

// Summary is the income/expense/net totals per currency.
type Summary struct {
	Totals []Total
}

func normaliseCurrency(c string) string {
	c = strings.ToUpper(strings.TrimSpace(c))
	if c == "" {
		return DefaultCurrency
	}
	return c
}

func (n *NewCategory) validate() error {
	if n.Name == "" {
		return fmt.Errorf("%w: name it", ErrInvalidCategory)
	}
	if !ValidKind(n.Kind) {
		return fmt.Errorf("%w: %q is not a kind", ErrInvalidCategory, n.Kind)
	}
	return nil
}

func (n *NewEntry) validate() error {
	if n.CategoryID == uuid.Nil {
		return fmt.Errorf("%w: name the category", ErrInvalidEntry)
	}
	if n.Amount < 0 {
		return fmt.Errorf("%w: the amount cannot be negative", ErrInvalidEntry)
	}
	if n.OccurredOn.IsZero() {
		return fmt.Errorf("%w: give it a date", ErrInvalidEntry)
	}
	n.Currency = normaliseCurrency(n.Currency)
	if len(n.Currency) != 3 {
		return fmt.Errorf("%w: a currency is three letters", ErrInvalidEntry)
	}
	return nil
}
