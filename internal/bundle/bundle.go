// Package bundle models course bundles: several courses grouped under one name and
// one price so a workspace can sell them together. It is a self-contained domain —
// it knows nothing about HTTP, references courses by id (never by import), and is
// tenant-scoped with RLS behind every table. Money is bigint minor units + a
// currency, defaulting to BDT poisha, never a float; a bundle priced 0 is free.
//
// Granting a bundle enrols the learner in each of its courses. That step lives with
// the coordinator, which reads a bundle's course ids from here (CourseIDs) and calls
// enrol itself — bundle never imports enrol.
package bundle

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Sentinels. internal/httpapi maps each to a status; add a line to
// bundle_errors_test.go in the same commit as a new one.
var (
	ErrNotFound    = errors.New("bundle: not found")
	ErrDuplicate   = errors.New("bundle: that slug is already used in this workspace")
	ErrInvalid     = errors.New("bundle: the bundle is not valid")
	ErrInvalidPage = errors.New("bundle: the page cursor is not valid")
)

// DefaultCurrency is BDT — the primary market. An international workspace passes its
// own; the column is char(3) either way.
const DefaultCurrency = "BDT"

// Bounds.
const (
	MaxCourses = 500
)

// Audit actions.
const (
	ActionCreated    = "bundle.created"
	ActionUpdated    = "bundle.updated"
	ActionDeleted    = "bundle.deleted"
	ActionCoursesSet = "bundle.courses_set"
)

// slugPattern permits lowercase letters, digits, and interior hyphens. Slugs appear
// in URLs, so anything else is either an encoding problem or an attempt at one.
var slugPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// Bundle is a named, priced group of courses. Courses is populated only by Get; a
// listing leaves it empty rather than issuing a query per row.
type Bundle struct {
	ID          uuid.UUID
	Slug        string
	Name        string
	Description string
	PriceAmount int64
	Currency    string
	Courses     []CourseRef
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// CourseRef is one course's place in a bundle.
type CourseRef struct {
	CourseID uuid.UUID
	Position int
}

// NewBundle describes a bundle to create.
type NewBundle struct {
	Slug        string
	Name        string
	Description string
	PriceAmount int64
	Currency    string
}

// BundlePatch is a partial update to a bundle's name, description, or price. A nil
// field is left unchanged.
type BundlePatch struct {
	Name        *string
	Description *string
	PriceAmount *int64
}

func normaliseCurrency(c string) string {
	c = strings.ToUpper(strings.TrimSpace(c))
	if c == "" {
		return DefaultCurrency
	}
	return c
}

func (n *NewBundle) validate() error {
	n.Slug = strings.TrimSpace(n.Slug)
	if !slugPattern.MatchString(n.Slug) {
		return fmt.Errorf("%w: %q is not a slug", ErrInvalid, n.Slug)
	}
	n.Name = strings.TrimSpace(n.Name)
	if n.Name == "" {
		return fmt.Errorf("%w: name it", ErrInvalid)
	}
	if n.PriceAmount < 0 {
		return fmt.Errorf("%w: the price cannot be negative", ErrInvalid)
	}
	n.Currency = normaliseCurrency(n.Currency)
	if len(n.Currency) != 3 {
		return fmt.Errorf("%w: a currency is three letters", ErrInvalid)
	}
	return nil
}

func (p *BundlePatch) validate() error {
	if p.Name != nil {
		trimmed := strings.TrimSpace(*p.Name)
		if trimmed == "" {
			return fmt.Errorf("%w: a name cannot be blank", ErrInvalid)
		}
		p.Name = &trimmed
	}
	if p.PriceAmount != nil && *p.PriceAmount < 0 {
		return fmt.Errorf("%w: the price cannot be negative", ErrInvalid)
	}
	return nil
}
