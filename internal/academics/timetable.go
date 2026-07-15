package academics

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ErrInvalidPeriod means a timetable slot named an impossible day or time.
var ErrInvalidPeriod = errors.New("academics: the timetable period is not valid")

// MaxPeriods bounds one section's week.
const MaxPeriods = 200

// clockLayout is how a period speaks its start and end: a plain wall-clock time,
// no date, no zone — a school week repeats.
const clockLayout = "15:04"

// Period is one slot in a section's weekly grid.
type Period struct {
	ID          uuid.UUID
	SectionID   uuid.UUID
	SubjectID   *uuid.UUID
	DayOfWeek   int
	StartsAt    string
	EndsAt      string
	TeacherName string
	Room        string
}

// NewPeriod is a slot to add to a section's timetable.
type NewPeriod struct {
	SectionID   uuid.UUID
	SubjectID   *uuid.UUID
	DayOfWeek   int
	StartsAt    string
	EndsAt      string
	TeacherName string
	Room        string
}

func (n NewPeriod) validate() error {
	if n.DayOfWeek < 0 || n.DayOfWeek > 6 {
		return fmt.Errorf("%w: a weekday is 0 (Sunday) to 6 (Saturday)", ErrInvalidPeriod)
	}
	start, err := time.Parse(clockLayout, n.StartsAt)
	if err != nil {
		return fmt.Errorf("%w: start time must be HH:MM", ErrInvalidPeriod)
	}
	end, err := time.Parse(clockLayout, n.EndsAt)
	if err != nil {
		return fmt.Errorf("%w: end time must be HH:MM", ErrInvalidPeriod)
	}
	if !end.After(start) {
		return fmt.Errorf("%w: a period ends after it starts", ErrInvalidPeriod)
	}
	return nil
}
