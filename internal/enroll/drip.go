package enroll

import (
	"errors"
	"time"
)

// ErrLessonLocked is returned when an enrolled learner opens a lesson the course
// has not released to them yet.
//
// A 403, not a 404. The lesson is in a curriculum they can already see, and the
// answer is "come back later", not "no such lesson".
var ErrLessonLocked = errors.New("enroll: this lesson has not been released yet")

// LessonLocked carries when the lesson opens, when that is a time at all.
//
// AvailableAt is nil in sequential mode, where a lesson opens on an event rather
// than a clock: nobody can say when the learner will finish the one before it.
type LessonLocked struct {
	AvailableAt *time.Time
}

func (e *LessonLocked) Error() string { return ErrLessonLocked.Error() }

// Unwrap makes errors.Is(err, ErrLessonLocked) true.
func (e *LessonLocked) Unwrap() error { return ErrLessonLocked }

// unlockAt returns the instant a dripped lesson opens for this reader, or nil
// when it is already open or opens on an event rather than a date.
//
// It is a pure function of facts the one lesson query already loaded, like
// decide, and for the same reason: every branch is enumerable, and enumerated.
func unlockAt(view LessonView, now time.Time) (locked bool, at *time.Time) {
	if view.DripMode == DripSequential {
		// No date to offer: the lesson opens when the learner finishes the one
		// before it, and nobody knows when that will be.
		return !view.Lesson.IsPreview && view.PriorIncomplete > 0, nil
	}

	at = schedule(view)
	if at == nil {
		return false, nil
	}
	return now.Before(*at), at
}

// schedule returns the instant this lesson opens for this reader, past or future,
// or nil when the course's mode does not schedule it by date.
//
// It is the one place that reads the per-lesson columns, and it reads only the
// one the course's mode names. A lesson carrying an available_at in a course that
// drips by enrolment date is carrying a value nobody asked for, and a client
// shown it would display a date the server will never enforce.
func schedule(view LessonView) *time.Time {
	// A preview is a free sample of the course, readable by strangers. Locking it
	// for the learner who enrolled would be a stranger reading what a customer
	// cannot, so it has no schedule at all.
	if view.Lesson.IsPreview {
		return nil
	}

	switch view.DripMode {
	case DripScheduled:
		return view.Lesson.AvailableAt

	case DripAfterEnrolment:
		if view.AvailableAfterDays == nil || view.EnrolledAt == nil {
			return nil
		}
		opens := view.EnrolledAt.AddDate(0, 0, *view.AvailableAfterDays)
		return &opens
	}

	return nil
}
