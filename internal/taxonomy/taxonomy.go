// Package taxonomy classifies the course catalogue for browsing and filtering: a
// course sits in at most one category and carries any number of tags. It knows
// nothing about HTTP, and references the catalogue (courses) by id through its own
// link tables — it never imports the catalog domain nor touches the courses table.
package taxonomy

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Sentinels. internal/httpapi maps each to a status; add a line to
// taxonomy_errors_test.go in the same commit as a new one.
var (
	ErrNotFound    = errors.New("taxonomy: not found")
	ErrDuplicate   = errors.New("taxonomy: that slug is already taken")
	ErrInvalid     = errors.New("taxonomy: the input is not valid")
	ErrInvalidPage = errors.New("taxonomy: the page cursor is not valid")
)

// Bounds.
const (
	MaxNameLen = 120
	MaxSlugLen = 140
)

// Audit actions.
const (
	ActionCategoryCreated = "course_category.created"
	ActionCategoryDeleted = "course_category.deleted"
	ActionTagCreated      = "course_tag.created"
	ActionTagDeleted      = "course_tag.deleted"
	ActionCourseCategory  = "course.categorised"
	ActionCourseTags      = "course.tagged"
)

// Category is one section of the catalogue; a course sits in at most one.
type Category struct {
	ID        uuid.UUID
	Name      string
	Slug      string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Tag is one cross-cutting label; a course carries any number.
type Tag struct {
	ID        uuid.UUID
	Name      string
	Slug      string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewTerm is a category or tag to create.
type NewTerm struct {
	Name string
	Slug string
}

var (
	slugStrip = regexp.MustCompile(`[^a-z0-9]+`)
	slugTrim  = regexp.MustCompile(`^-+|-+$`)
)

// slugify derives a URL-safe slug from a name: lowercase, non-alphanumerics
// collapsed to single hyphens, trimmed.
func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = slugStrip.ReplaceAllString(s, "-")
	s = slugTrim.ReplaceAllString(s, "")
	return s
}

// validate normalises the term: the name is required, and an empty slug is
// derived from the name.
func (n *NewTerm) validate() error {
	n.Name = strings.TrimSpace(n.Name)
	if n.Name == "" {
		return fmt.Errorf("%w: give it a name", ErrInvalid)
	}
	if len(n.Name) > MaxNameLen {
		return fmt.Errorf("%w: the name is too long", ErrInvalid)
	}
	n.Slug = slugify(n.Slug)
	if n.Slug == "" {
		n.Slug = slugify(n.Name)
	}
	if n.Slug == "" {
		return fmt.Errorf("%w: the slug is empty", ErrInvalid)
	}
	if len(n.Slug) > MaxSlugLen {
		return fmt.Errorf("%w: the slug is too long", ErrInvalid)
	}
	return nil
}
