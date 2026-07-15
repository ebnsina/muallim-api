package academics

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ErrInvalidAttendance means a mark carried no entries or a status the register
// does not know.
var ErrInvalidAttendance = errors.New("academics: the attendance mark is not valid")

// Attendance statuses.
const (
	Present = "present"
	Absent  = "absent"
	Late    = "late"
	Excused = "excused"
)

// ValidAttendanceStatus reports whether s is a mark a register may carry.
func ValidAttendanceStatus(s string) bool {
	switch s {
	case Present, Absent, Late, Excused:
		return true
	default:
		return false
	}
}

// MaxAttendanceEntries bounds one day's mark. A section is a class, not a stadium.
const MaxAttendanceEntries = 500

// AttendanceEntry is one student's status on the day being marked.
type AttendanceEntry struct {
	StudentID uuid.UUID
	Status    string
}

// AttendanceMark is a section's register for one day.
type AttendanceMark struct {
	SectionID *uuid.UUID
	OnDate    time.Time
	Entries   []AttendanceEntry
	MarkedBy  uuid.UUID
}

// RegisterEntry is one line of a section's register on a day: a student and how
// they were marked.
type RegisterEntry struct {
	StudentID   uuid.UUID
	AdmissionNo string
	FullName    string
	Status      string
}

// AttendanceDay is one day of a single student's history.
type AttendanceDay struct {
	OnDate    time.Time
	SectionID *uuid.UUID
	Status    string
}

// AttendanceSummary counts a student's days by status over a range.
type AttendanceSummary struct {
	Present int
	Absent  int
	Late    int
	Excused int
	Total   int
}

func (m AttendanceMark) validate() error {
	if len(m.Entries) == 0 {
		return fmt.Errorf("%w: name at least one student", ErrInvalidAttendance)
	}
	if len(m.Entries) > MaxAttendanceEntries {
		return fmt.Errorf("%w: a register holds at most %d", ErrInvalidAttendance, MaxAttendanceEntries)
	}
	for _, e := range m.Entries {
		if !ValidAttendanceStatus(e.Status) {
			return fmt.Errorf("%w: %q is not a status", ErrInvalidAttendance, e.Status)
		}
	}
	return nil
}
