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
	ErrNotFound      = errors.New("catalog: not found")
	ErrInvalidPage   = errors.New("catalog: invalid page cursor")
	ErrInvalidLimit  = errors.New("catalog: invalid page size")
	ErrSlugTaken     = errors.New("catalog: slug is already used in this workspace")
	ErrInvalidSlug   = errors.New("catalog: slug must be lowercase letters, digits, and hyphens")
	ErrInvalidLesson = errors.New("catalog: lesson is not valid")
)

// ActionCourseCreated is the audit action this package emits.
const ActionCourseCreated = "course.created"

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

	// DripMode decides how the course releases its lessons, and which of the
	// per-lesson schedule columns mean anything.
	DripMode string

	// LessonCount is the number of lessons across the course's topics. It is set
	// only where a caller asks for it — the listing fills it, a single-course load
	// does not — so a zero here can mean "no lessons" or "not counted".
	LessonCount int

	CreatedAt time.Time
	UpdatedAt time.Time
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

	// The drip schedule as stored. Which of the two the course reads is decided by
	// its drip_mode; both are carried so an author can see and edit the schedule
	// they set, and a learner's client can mark a lesson as not yet open.
	//
	// Neither is a secret. A learner refused the lesson is told the date anyway,
	// and knowing that lesson four opens in a week is the point of a drip.
	AvailableAt        *time.Time
	AvailableAfterDays *int
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

// ListParams paginates a course list.
type ListParams struct {
	// IncludeDrafts widens the listing to every status.
	//
	// It carries an authorisation decision made by the transport layer, never a
	// request parameter: a client that could ask for drafts by adding one to the
	// query string would be the whole vulnerability. The zero value lists
	// published courses, so a caller who forgets it leaks nothing.
	IncludeDrafts bool

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
