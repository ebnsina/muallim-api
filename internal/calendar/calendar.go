// Package calendar holds a school's academic calendar — its holidays, exam dates,
// term boundaries, and events. It knows nothing about HTTP, and is tenant-scoped
// with RLS behind the one table it owns.
package calendar

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Sentinels. internal/httpapi maps each to a status; add a line to errors_test.go
// in the same commit as a new one.
var (
	ErrNotFound     = errors.New("calendar: not found")
	ErrInvalidEvent = errors.New("calendar: the event is not valid")
	ErrInvalidPage  = errors.New("calendar: the page cursor is not valid")
)

// Kinds an entry on the calendar can be.
const (
	KindHoliday   = "holiday"
	KindExam      = "exam"
	KindEvent     = "event"
	KindTermStart = "term_start"
	KindTermEnd   = "term_end"
)

// ValidKind reports whether k is a kind an event may hold.
func ValidKind(k string) bool {
	switch k {
	case KindHoliday, KindExam, KindEvent, KindTermStart, KindTermEnd:
		return true
	default:
		return false
	}
}

// Audit action.
const ActionCreated = "calendar.event_created"

// Event is one entry on the academic calendar. A single-day entry has no EndsOn.
type Event struct {
	ID          uuid.UUID
	Title       string
	Description *string
	Kind        string
	StartsOn    time.Time
	EndsOn      *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// NewEvent is an event to create.
type NewEvent struct {
	Title       string
	Description *string
	Kind        string
	StartsOn    time.Time
	EndsOn      *time.Time
}

// EventPatch edits an event; a nil field is left unchanged.
type EventPatch struct {
	Title       *string
	Description *string
	Kind        *string
	StartsOn    *time.Time
	EndsOn      *time.Time
}

// EventFilter narrows the calendar: by kind, and to a date window on StartsOn.
type EventFilter struct {
	Kind string
	From *time.Time
	To   *time.Time
}

func (n *NewEvent) validate() error {
	if n.Title == "" {
		return fmt.Errorf("%w: give it a title", ErrInvalidEvent)
	}
	if n.Kind == "" {
		n.Kind = KindEvent
	}
	if !ValidKind(n.Kind) {
		return fmt.Errorf("%w: %q is not a kind", ErrInvalidEvent, n.Kind)
	}
	if n.EndsOn != nil && n.EndsOn.Before(n.StartsOn) {
		return fmt.Errorf("%w: it cannot end before it starts", ErrInvalidEvent)
	}
	return nil
}

func (p EventPatch) validate() error {
	if p.Title != nil && *p.Title == "" {
		return fmt.Errorf("%w: give it a title", ErrInvalidEvent)
	}
	if p.Kind != nil && !ValidKind(*p.Kind) {
		return fmt.Errorf("%w: %q is not a kind", ErrInvalidEvent, *p.Kind)
	}
	// When both ends are supplied together the order is checked here; the table's
	// CHECK is the net for a patch that moves only one of them.
	if p.StartsOn != nil && p.EndsOn != nil && p.EndsOn.Before(*p.StartsOn) {
		return fmt.Errorf("%w: it cannot end before it starts", ErrInvalidEvent)
	}
	return nil
}
