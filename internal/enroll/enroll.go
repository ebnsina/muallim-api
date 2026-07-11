// Package enroll owns enrolments and learning progress.
//
// It answers one question above all others: may this person read this lesson?
// Everything else here exists to make that answer cheap and correct.
//
// It knows nothing about HTTP. It returns its own sentinel errors.
package enroll

import (
	"errors"
	"net/netip"
	"time"

	"github.com/google/uuid"
)

// Sentinel errors.
var (
	ErrNotFound        = errors.New("enroll: not found")
	ErrNotEnrolled     = errors.New("enroll: not enrolled in this course")
	ErrAlreadyEnrolled = errors.New("enroll: already enrolled")
	ErrCourseNotOpen   = errors.New("enroll: the course is not open for enrolment")
	ErrEnrolmentEnded  = errors.New("enroll: the enrolment has expired or been cancelled")
	ErrInvalidReview   = errors.New("enroll: the review is empty or out of range")
	ErrReviewNotFound  = errors.New("enroll: review not found")
)

// Audit actions this package emits.
const (
	ActionEnrolled       = "enrolment.created"
	ActionEnrolmentEnded = "enrolment.cancelled"
	ActionCourseFinished = "course.completed"

	// ActionCourseReopened records a completion being retracted. A finished course
	// that becomes unfinished is a fact somebody will one day need to explain.
	ActionCourseReopened = "course.reopened"

	// ActionReviewed records a learner rating a course. Retracting a review is
	// ordinary and unaudited; leaving a public verdict is the fact worth keeping.
	ActionReviewed = "course.reviewed"
)

// Review bounds. A star rating is 1..5; the written body is optional and capped
// so a review is a paragraph, not an essay pasted into a text column.
const (
	MinRating      = 1
	MaxRating      = 5
	MaxReviewBody  = 4000
)

// Drip modes. A course releases its lessons all at once, on a fixed date, a
// number of days after each learner enrols, or one at a time as the learner
// finishes the one before.
const (
	DripNone           = "none"
	DripScheduled      = "scheduled"
	DripAfterEnrolment = "after_enrolment"
	DripSequential     = "sequential"
)

// ValidDripMode reports whether mode is one this system knows. An unknown mode
// is refused rather than treated as "none": a course that silently stops dripping
// is a course that publishes its content early.
func ValidDripMode(mode string) bool {
	switch mode {
	case DripNone, DripScheduled, DripAfterEnrolment, DripSequential:
		return true
	default:
		return false
	}
}

// Enrolment statuses.
const (
	StatusActive    = "active"
	StatusCompleted = "completed"
	StatusExpired   = "expired"
	StatusCancelled = "cancelled"
)

// Enrolment sources. Why this person has access is the first question support
// asks, so it is recorded rather than inferred.
const (
	SourceSelf     = "self"
	SourceGranted  = "granted"
	SourcePurchase = "purchase"
	SourceImport   = "import"
)

// Enrolment is a person's right to study a course.
type Enrolment struct {
	ID       uuid.UUID
	CourseID uuid.UUID
	UserID   uuid.UUID

	Status string
	Source string

	ExpiresAt   *time.Time
	EnrolledAt  time.Time
	CompletedAt *time.Time
}

// Live reports whether the enrolment currently grants access.
//
// A completed enrolment still grants access: finishing a course does not evict
// you from it. An expired or cancelled one does not.
func (e Enrolment) Live(now time.Time) bool {
	switch e.Status {
	case StatusActive, StatusCompleted:
	default:
		return false
	}
	return e.ExpiresAt == nil || now.Before(*e.ExpiresAt)
}

// Progress is a learner's standing in one course.
type Progress struct {
	CourseID         uuid.UUID
	LessonsCompleted int
	LessonsTotal     int
	Percent          int
	UpdatedAt        time.Time
}

// Complete reports whether every lesson is done. A course with no lessons is not
// complete, however tempting the arithmetic.
func (p Progress) Complete() bool {
	return p.LessonsTotal > 0 && p.LessonsCompleted == p.LessonsTotal
}

// EnrolmentWithCourse pairs an enrolment with the course it grants, so a
// learner's dashboard is one query rather than one per row.
type EnrolmentWithCourse struct {
	Enrolment Enrolment
	Progress  Progress

	CourseSlug  string
	CourseTitle string
}

// Review is a learner's public verdict on a course they enrolled in.
//
// AuthorName is denormalised at read time from the users table, so a review
// wall is one query. It is empty when the account has since been erased — the
// review outlives the reviewer, and is shown without a name rather than dropped.
type Review struct {
	ID       uuid.UUID
	CourseID uuid.UUID
	UserID   uuid.UUID

	Rating int
	Body   string

	AuthorName string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// ReviewSummary is a course's standing at a glance: how many learners reviewed
// it and their mean rating, rounded to one decimal for display. Count zero means
// no reviews, and Average is then zero rather than a division by nobody.
type ReviewSummary struct {
	Count   int
	Average float64
}

// CourseAnalytics is a course's standing at a glance, for the instructor who
// owns it: how many have enrolled and where they are, and how it has been rated.
type CourseAnalytics struct {
	Total     int // every enrolment ever, whatever its status
	Active    int
	Completed int
	Inactive  int // expired or cancelled

	// AvgProgress is the mean course-progress percent across live enrolments,
	// zero when nobody has enrolled.
	AvgProgress float64

	Reviews ReviewSummary
}

// CompletionRate is the share of live enrolments that finished, 0..1. A course
// nobody active is studying has a rate of zero rather than a division by nobody.
func (a CourseAnalytics) CompletionRate() float64 {
	live := a.Active + a.Completed
	if live == 0 {
		return 0
	}
	return float64(a.Completed) / float64(live)
}

// LessonContent is a lesson as a learner reads it.
type LessonContent struct {
	ID       uuid.UUID
	TopicID  uuid.UUID
	CourseID uuid.UUID

	Title       string
	ContentType string
	Content     string
	VideoSource string
	VideoURL    string

	// VideoEmbedURL is the player, written by a provider from a validated id. It is
	// the only one of the three a client may put in an `iframe`; VideoURL is what an
	// author typed, and nothing has vouched for it.
	VideoEmbedURL   string
	DurationSeconds int
	IsPreview       bool
	Position        int

	// AvailableAt is when a dripped lesson opens for this reader: a fixed instant
	// in scheduled mode, their own enrolment date plus the delay in
	// after_enrolment mode, and nil in sequential mode — where the lesson opens on
	// an event, not a clock.
	AvailableAt *time.Time

	// CompletedAt is this reader's own progress, nil when they have not finished
	// it or are not signed in.
	CompletedAt *time.Time
}

// Access describes why a lesson may be read, for the caller's benefit and for
// anyone reading a log line six months from now.
type Access int

const (
	// AccessDenied is the zero value on purpose: a bug that forgets to set an
	// access level denies rather than grants.
	AccessDenied Access = iota

	// AccessPreview is a free sample of a published course. Anyone may read it.
	AccessPreview

	// AccessEnrolled is the ordinary case.
	AccessEnrolled

	// AccessAuthor is somebody who may edit the course, reading their own draft.
	AccessAuthor

	// AccessLocked is an enrolled learner whose lesson has not been released yet.
	//
	// It grants nothing, like AccessDenied, and means something different: the
	// learner belongs here and the lesson exists, so they are told to come back
	// rather than told the lesson does not exist.
	AccessLocked
)

func (a Access) String() string {
	switch a {
	case AccessPreview:
		return "preview"
	case AccessEnrolled:
		return "enrolled"
	case AccessAuthor:
		return "author"
	case AccessLocked:
		return "locked"
	default:
		return "denied"
	}
}

// Granted reports whether the lesson may be read.
//
// A locked lesson is not readable. It is listed against AccessDenied explicitly
// rather than by inequality, so that adding a fifth level is a compile-time
// decision about whether it opens the door.
func (a Access) Granted() bool {
	switch a {
	case AccessPreview, AccessEnrolled, AccessAuthor:
		return true
	default:
		return false
	}
}

// Reader identifies who is asking to read a lesson.
//
// UserID is uuid.Nil for an anonymous reader, which is a legitimate case: a
// preview lesson of a published course is readable by a stranger.
type Reader struct {
	UserID uuid.UUID

	// CanAuthor is an authorisation decision made by the transport layer, never a
	// request parameter. It lets an instructor read their own unpublished draft.
	CanAuthor bool
}

// Anonymous reports whether nobody is signed in.
func (r Reader) Anonymous() bool { return r.UserID == uuid.Nil }

// AuditEntry mirrors audit.Entry. Restated here because a domain package may not
// import a sibling; cmd/ wires an adapter.
type AuditEntry struct {
	ActorID    *uuid.UUID
	Action     string
	TargetType string
	TargetID   string
	IP         netip.Addr
	UserAgent  string
	Metadata   map[string]any
}

// Actor identifies who performed an audited action.
type Actor struct {
	UserID    uuid.UUID
	IP        netip.Addr
	UserAgent string
}

// Page bounds. Nobody has 40,000 enrolments, but nobody had 40,000 courses either.
const (
	DefaultPageSize = 20
	MaxPageSize     = 100
)
