// Package catalog owns courses, topics, and lessons.
//
// It knows nothing about HTTP. It returns its own sentinel errors, which the
// transport layer maps to status codes.
package catalog

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Sentinel errors.
var (
	ErrNotFound     = errors.New("catalog: not found")
	ErrInvalidPage  = errors.New("catalog: invalid page cursor")
	ErrInvalidLimit = errors.New("catalog: invalid page size")
)

// Course statuses.
const (
	StatusDraft     = "draft"
	StatusPublished = "published"
	StatusArchived  = "archived"
)

// Course is a unit of sale and of study.
type Course struct {
	ID          uuid.UUID
	Slug        string
	Title       string
	Summary     string
	Difficulty  string
	Status      string
	PublishedAt *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Topic is an ordered section of a course.
type Topic struct {
	ID       uuid.UUID
	CourseID uuid.UUID
	Title    string
	Position int
	Lessons  []Lesson
}

// Lesson is an ordered item within a topic.
type Lesson struct {
	ID              uuid.UUID
	TopicID         uuid.UUID
	Title           string
	ContentType     string
	DurationSeconds int
	IsPreview       bool
	Position        int
}

// Curriculum is a course together with its full topic and lesson tree.
type Curriculum struct {
	Course Course
	Topics []Topic
}

// TotalDuration sums every lesson. Computed in Go from data already loaded, not
// by a second query.
func (c Curriculum) TotalDuration() time.Duration {
	var total int
	for _, t := range c.Topics {
		for _, l := range t.Lessons {
			total += l.DurationSeconds
		}
	}
	return time.Duration(total) * time.Second
}

// LessonCount reports the number of lessons across all topics.
func (c Curriculum) LessonCount() int {
	var n int
	for _, t := range c.Topics {
		n += len(t.Lessons)
	}
	return n
}

// Page is one page of a keyset-paginated list.
//
// There is no total count. Counting the matching rows costs a full scan on every
// page request, and nobody clicks "page 4,271".
type Page struct {
	Courses    []Course
	NextCursor string
	HasMore    bool
}

// ListParams filters and paginates a course list.
type ListParams struct {
	// Status filters by course status. Empty means published only, because an
	// unauthenticated catalog request must never surface drafts.
	Status string

	// Limit is the page size, bounded by MaxPageSize.
	Limit int

	// Cursor is an opaque continuation token from a previous Page.
	Cursor string
}

// Page size bounds. An unbounded list endpoint is an outage waiting for the
// tenant with 40,000 courses.
const (
	DefaultPageSize = 20
	MaxPageSize     = 100
)
