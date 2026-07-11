// Package gamify owns points, badges, and the leaderboard.
//
// Points are awarded for finishing things — a lesson, a course — by the domain
// that owns the event, in its transaction, through an interface it declares. The
// award is idempotent: the ledger's unique key means re-finishing the same lesson
// earns nothing, so points cannot be farmed by toggling completion.
//
// It knows nothing about HTTP, and it imports no sibling: producers declare their
// own interface and cmd wires an adapter over this package.
package gamify

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Sentinel errors. internal/httpapi maps them; nothing else does.
var (
	// ErrInvalidPage is an opaque cursor that did not decode. Reserved for a future
	// paginated leaderboard; the top-N view does not use it.
	ErrInvalidPage = errors.New("gamify: invalid page cursor")
)

// Award reasons. What a point award was for; half of its idempotency key.
const (
	ReasonLesson = "lesson"
	ReasonCourse = "course"
)

// Point values. Small integers, tuned so a course is worth far more than the sum
// of clicking through its lessons — finishing is the thing worth rewarding.
const (
	PointsLesson = 10
	PointsCourse = 100
)

// Badges. A fixed, code-defined catalog — like the question types — so the
// database records only who has which, not what a badge is.
const (
	// BadgeFirstLesson is a learner's first completed lesson: they have started.
	BadgeFirstLesson = "first_lesson"

	// BadgeGraduate is a learner's first completed course.
	BadgeGraduate = "graduate"

	// BadgeHonorRoll is five completed courses.
	BadgeHonorRoll = "honor_roll"
)

// Badge describes one badge for a client: its code, a name, and a line of why it
// was earned. The catalog lives here so a client need not hard-code it.
type Badge struct {
	Code        string
	Name        string
	Description string
}

// catalog is every badge this system can award, in the order they are earned.
var catalog = []Badge{
	{BadgeFirstLesson, "First steps", "Completed your first lesson."},
	{BadgeGraduate, "Graduate", "Completed your first course."},
	{BadgeHonorRoll, "Honour roll", "Completed five courses."},
}

// HonorRollCourses is how many completed courses earn the honour-roll badge.
const HonorRollCourses = 5

// describe returns the catalogue entry for a badge code, or a bare fallback so an
// unknown code (a badge removed from the catalogue but still held) still renders.
func describe(code string) Badge {
	for _, b := range catalog {
		if b.Code == code {
			return b
		}
	}
	return Badge{Code: code, Name: code}
}

// EarnedBadge is a badge a learner holds, with when they earned it.
type EarnedBadge struct {
	Badge
	AwardedAt time.Time
}

// Standing is a learner's own gamification summary: points, rank, and badges.
type Standing struct {
	Points int

	// Rank is 1-based, counting how many learners have strictly more points. A
	// learner with no points has the last rank, not rank zero.
	Rank int

	// OutOf is how many learners have any points, so a client can say "3rd of 40".
	OutOf int

	Badges []EarnedBadge
}

// LeaderboardEntry is one row of the leaderboard.
type LeaderboardEntry struct {
	UserID uuid.UUID
	Name   string
	Points int
	Rank   int
}

// Leaderboard page bounds. The board shows a top slice, not everyone.
const (
	DefaultLeaderboardSize = 20
	MaxLeaderboardSize     = 100
)
