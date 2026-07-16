// Package learnpath models an ordered track of courses a learner works through
// in sequence. It is self-contained: it references the catalogue by course id
// and imports no other domain. The coordinator maps per-course progress (from
// enroll) onto the ordered course list this package exposes; learnpath knows
// nothing of enrolment, progress, or HTTP.
package learnpath

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Sentinels. internal/httpapi maps each to a status; add a line to
// learnpath_errors_test.go in the same commit as a new one.
var (
	ErrNotFound        = errors.New("learnpath: not found")
	ErrDuplicate       = errors.New("learnpath: that slug is already taken")
	ErrInvalid         = errors.New("learnpath: the path is not valid")
	ErrIncompleteOrder = errors.New("learnpath: the course order must name every course exactly once")
	ErrInvalidPage     = errors.New("learnpath: the page cursor is not valid")
)

// Status values.
const (
	StatusDraft     = "draft"
	StatusPublished = "published"
)

// Audit actions.
const (
	ActionPathCreated = "learning_path.created"
	ActionPathUpdated = "learning_path.updated"
	ActionPathDeleted = "learning_path.deleted"
	ActionCoursesSet  = "learning_path.courses_set"
)

// slugPattern permits lowercase letters, digits, and interior hyphens: what a URL
// path segment may safely hold.
var slugPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// Path is one ordered track of courses. Courses is populated only by Get and
// CourseIDs — the list view carries metadata alone.
type Path struct {
	ID          uuid.UUID
	Slug        string
	Title       string
	Description string
	Status      string
	Courses     []uuid.UUID
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// CourseRef is one course's place in a path: which course, at what position.
type CourseRef struct {
	CourseID uuid.UUID
	Position int
}

// PathCourse is one row of the join table, a course pinned to a path at a
// position.
type PathCourse struct {
	PathID   uuid.UUID
	CourseID uuid.UUID
	Position int
}

// NewPath describes a path to create.
type NewPath struct {
	Slug        string
	Title       string
	Description string
}

// PathPatch updates a path; a nil field is left unchanged.
type PathPatch struct {
	Title       *string
	Description *string
	Status      *string
}

// PathFilter narrows the listing.
type PathFilter struct {
	// IncludeDrafts widens the listing to every status.
	//
	// It carries an authorisation decision made by the transport layer, never a
	// request parameter: a client that could ask for drafts by adding one to the
	// query string would be the whole vulnerability. The zero value lists
	// published paths, so a caller who forgets it leaks nothing.
	IncludeDrafts bool

	// Status narrows to a single status. A reader who may not see drafts lists
	// published paths whatever this asks for.
	Status string
}

func (n *NewPath) validate() error {
	n.Slug = strings.TrimSpace(n.Slug)
	if !slugPattern.MatchString(n.Slug) {
		return fmt.Errorf("%w: slug %q must be lowercase letters, digits and hyphens", ErrInvalid, n.Slug)
	}
	n.Title = strings.TrimSpace(n.Title)
	if n.Title == "" {
		return fmt.Errorf("%w: give it a title", ErrInvalid)
	}
	n.Description = strings.TrimSpace(n.Description)
	return nil
}

func (p *PathPatch) validate() error {
	if p.Title != nil {
		t := strings.TrimSpace(*p.Title)
		if t == "" {
			return fmt.Errorf("%w: the title cannot be blank", ErrInvalid)
		}
		p.Title = &t
	}
	if p.Description != nil {
		d := strings.TrimSpace(*p.Description)
		p.Description = &d
	}
	if p.Status != nil {
		if *p.Status != StatusDraft && *p.Status != StatusPublished {
			return fmt.Errorf("%w: unknown status %q", ErrInvalid, *p.Status)
		}
	}
	return nil
}

// dedupe reports the first course named twice, or nil when every id is distinct.
// SetCourses replaces a path's whole membership, so a repeated id is an order
// that fails to name each intended course exactly once.
func dedupe(order []uuid.UUID) error {
	seen := make(map[uuid.UUID]struct{}, len(order))
	for _, id := range order {
		if id == uuid.Nil {
			return fmt.Errorf("%w: a course id is empty", ErrInvalid)
		}
		if _, dup := seen[id]; dup {
			return fmt.Errorf("%w: %s appears twice", ErrIncompleteOrder, id)
		}
		seen[id] = struct{}{}
	}
	return nil
}
