// Package grade turns marks into a grade.
//
// It owns two things nothing else does: the scale that converts a percentage into
// a word, and the roll-up of every mark a learner has earned in a course.
//
// It does not go looking for marks. `assess` and `assign` hand them over, in the
// transaction that awarded them — the same seam `enroll` is reached through, and
// for the same reason. A gradebook assembled by reading `quiz_attempts` and
// `assignment_submissions` would have to know both schemas, and would learn about
// a third kind of assessment by breaking.
package grade

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// The sentinels. `internal/httpapi` maps them; nothing else does.
var (
	ErrNotFound     = errors.New("grade: not found")
	ErrInvalidScale = errors.New("grade: invalid scale")
	ErrScaleExists  = errors.New("grade: a scale with that name already exists")
	ErrInvalidScore = errors.New("grade: invalid score")
)

// What an item can be. The database checks the same two strings.
const (
	SourceQuiz       = "quiz"
	SourceAssignment = "assignment"
)

// Band is one step of a scale: everything at or above Min, and below the band
// above it, is called Label.
type Band struct {
	Label   string
	Min     int
	IsPass  bool
	ScaleID uuid.UUID
}

// Scale is a workspace's grading scale.
type Scale struct {
	ID   uuid.UUID
	Name string

	Bands []Band

	// Builtin marks the scale nobody created and nobody may edit.
	Builtin bool
}

/*
DefaultScale is what a workspace grades by until it says otherwise.

Named rather than nil. A course whose scale is unset still has to render a letter,
and a nil scale would mean every read path carried a branch for "no scale yet" —
which is the state most workspaces are in for ever.

The floors are the ones an American school uses, because a default has to be
somebody's. A workspace that grades differently makes a scale and says so.
*/
func DefaultScale() Scale {
	return Scale{
		Name:    "Default",
		Builtin: true,
		Bands: []Band{
			{Label: "A", Min: 90, IsPass: true},
			{Label: "B", Min: 80, IsPass: true},
			{Label: "C", Min: 70, IsPass: true},
			{Label: "D", Min: 60, IsPass: true},
			{Label: "F", Min: 0, IsPass: false},
		},
	}
}

/*
Band returns the band a percentage falls in.

Bands are searched from the highest floor down, so the first one at or below the
percentage is the answer. A scale with no band at 0 has a hole at the bottom: a
learner who scored 3% would fall through it. `validate` refuses such a scale, and
this returns the lowest band rather than a zero value if one ever gets through —
an unlabelled grade is worse than a wrong one, because nobody notices it.
*/
func (s Scale) Band(percent int) Band {
	sorted := s.sortedBands()
	if len(sorted) == 0 {
		return Band{}
	}

	for _, band := range sorted {
		if percent >= band.Min {
			return band
		}
	}

	return sorted[len(sorted)-1]
}

// sortedBands is the bands from the highest floor down. The database returns them
// in this order too; sorting here means a Scale built in a test behaves the same.
func (s Scale) sortedBands() []Band {
	sorted := make([]Band, len(s.Bands))
	copy(sorted, s.Bands)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Min > sorted[j].Min })
	return sorted
}

/*
validate refuses a scale that cannot grade every percentage exactly once.

Three ways to get it wrong, and a scale is used on every gradebook page of every
course that points at it, so all three are refused at the door rather than
discovered by a learner with an empty grade.
*/
func (s Scale) validate() error {
	if strings.TrimSpace(s.Name) == "" {
		return fmt.Errorf("%w: a scale needs a name", ErrInvalidScale)
	}
	if len(s.Bands) == 0 {
		return fmt.Errorf("%w: a scale needs at least one band", ErrInvalidScale)
	}

	floors := make(map[int]bool, len(s.Bands))
	lowest, passes := 101, 0

	for _, band := range s.Bands {
		if strings.TrimSpace(band.Label) == "" {
			return fmt.Errorf("%w: a band needs a label", ErrInvalidScale)
		}
		if band.Min < 0 || band.Min > 100 {
			return fmt.Errorf("%w: %q starts at %d%%", ErrInvalidScale, band.Label, band.Min)
		}
		if floors[band.Min] {
			// Two bands at one floor: the letter would depend on which row came back
			// first, which is a decision the planner should not be making.
			return fmt.Errorf("%w: two bands start at %d%%", ErrInvalidScale, band.Min)
		}
		floors[band.Min] = true

		if band.Min < lowest {
			lowest = band.Min
		}
		if band.IsPass {
			passes++
		}
	}

	if lowest != 0 {
		// A learner who scored below the lowest floor has no grade at all.
		return fmt.Errorf("%w: no band covers 0%%", ErrInvalidScale)
	}
	if passes == 0 {
		return fmt.Errorf("%w: no band is a pass", ErrInvalidScale)
	}

	return nil
}

// Item is one thing worth marks in a course.
type Item struct {
	ID        uuid.UUID
	CourseID  uuid.UUID
	LessonID  uuid.UUID
	Source    string
	SourceID  uuid.UUID
	Title     string
	MaxPoints int
	CreatedAt time.Time
}

// Entry is one learner's mark for one item.
type Entry struct {
	ItemID    uuid.UUID
	UserID    uuid.UUID
	Points    int
	MaxPoints int
	GradedAt  time.Time
}

// Score is a mark, as `assess` and `assign` hand it over.
type Score struct {
	LessonID uuid.UUID
	UserID   uuid.UUID

	Source   string
	SourceID uuid.UUID

	Title     string
	Points    int
	MaxPoints int

	/*
		KeepHighest leaves a better mark alone.

		A quiz may be attempted several times, and a learner who scores 100 and then
		retries out of curiosity should not be punished for the curiosity. Their
		standing is the best they have done.

		An assignment is marked, not attempted, and a marker who corrects a typo means
		to correct it — downwards as often as up. That path overwrites.

		Compared as fractions, not percentages: 9 of 10 beats 89 of 100, and rounding
		both to 90% first would call it a draw and keep whichever arrived first.
	*/
	KeepHighest bool
}

func (s Score) validate() error {
	if s.Source != SourceQuiz && s.Source != SourceAssignment {
		return fmt.Errorf("%w: %q is not a kind of assessment", ErrInvalidScore, s.Source)
	}
	if s.MaxPoints <= 0 {
		return fmt.Errorf("%w: an assessment worth %d points", ErrInvalidScore, s.MaxPoints)
	}
	if s.Points < 0 || s.Points > s.MaxPoints {
		return fmt.Errorf("%w: %d of %d points", ErrInvalidScore, s.Points, s.MaxPoints)
	}
	if strings.TrimSpace(s.Title) == "" {
		return fmt.Errorf("%w: an assessment needs a title", ErrInvalidScore)
	}
	return nil
}

// Result is what a learner has earned in a course.
type Result struct {
	// Graded is how many items have a mark, of ItemCount in the course. A learner
	// looking at "72%" wants to know 72% of what.
	Graded    int
	ItemCount int

	Points    int
	MaxPoints int

	Percent int

	// Band is the zero value until something has been graded. A learner who has
	// attempted nothing has no grade — reporting 0% and an F would fail them for
	// work they have not yet been asked to hand in.
	Band Band
}

/*
Summarise rolls a learner's entries up into a course grade.

Only graded items count. A course grade computed across every item, with the
unattempted ones scored zero, tells a learner who has done the first of ten
assessments perfectly that they are failing at 10% — and it tells them so on the
day they are most likely to give up.

`Graded` and `ItemCount` are both returned so the page can say what the number is
a percentage of. That is the honest way to show a partial grade: the number, and
how much of the course it covers.

A learner with nothing graded gets no band at all. Zero percent and an F is a
grade; "not yet" is the truth.
*/
func Summarise(items []Item, entries []Entry, scale Scale) Result {
	marks := make(map[uuid.UUID]Entry, len(entries))
	for _, entry := range entries {
		marks[entry.ItemID] = entry
	}

	result := Result{ItemCount: len(items)}

	for _, item := range items {
		entry, graded := marks[item.ID]
		if !graded {
			continue
		}

		result.Graded++
		result.Points += entry.Points

		// The item's worth as it stood when it was marked, not as it stands now. An
		// author who doubles a quiz's points has not halved everybody's grade.
		result.MaxPoints += entry.MaxPoints
	}

	if result.Graded == 0 {
		return result
	}

	// Rounded half up, on the total, once. Rounding each item and averaging the
	// roundings turns three 66.67% items into 67% when the truth is 66.67%.
	result.Percent = (result.Points*200 + result.MaxPoints) / (result.MaxPoints * 2)
	result.Band = scale.Band(result.Percent)

	return result
}
