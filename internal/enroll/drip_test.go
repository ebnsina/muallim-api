package enroll

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// Drip is part of the access rule, so it is enumerated like the rest of it.
func TestDecideWithDrip(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	yesterday := now.Add(-24 * time.Hour)
	tomorrow := now.Add(24 * time.Hour)
	lastWeek := now.Add(-7 * 24 * time.Hour)

	learner := Reader{UserID: uuid.New()}
	authorReader := Reader{UserID: uuid.New(), CanAuthor: true}
	anonymous := Reader{}

	// A published course with a live enrolment, which is where drip applies.
	enrolled := func(mode string) LessonView {
		active := StatusActive
		return LessonView{
			CourseStatus:    "published",
			DripMode:        mode,
			EnrolmentStatus: &active,
			EnrolledAt:      &lastWeek,
		}
	}
	days := func(v LessonView, n int) LessonView { v.AvailableAfterDays = &n; return v }
	at := func(v LessonView, when time.Time) LessonView { v.Lesson.AvailableAt = &when; return v }
	preview := func(v LessonView) LessonView { v.Lesson.IsPreview = true; return v }
	prior := func(v LessonView, n int) LessonView { v.PriorIncomplete = n; return v }

	tests := []struct {
		name   string
		view   LessonView
		reader Reader
		want   Access
	}{
		// No drip: nothing changes.
		{"no drip", enrolled(DripNone), learner, AccessEnrolled},
		{"no drip ignores a stray date", at(enrolled(DripNone), tomorrow), learner, AccessEnrolled},

		// Scheduled: one instant, the same for everybody.
		{"scheduled, not yet", at(enrolled(DripScheduled), tomorrow), learner, AccessLocked},
		{"scheduled, already open", at(enrolled(DripScheduled), yesterday), learner, AccessEnrolled},
		{"scheduled with no date is open", enrolled(DripScheduled), learner, AccessEnrolled},

		// After enrolment: a different date for each learner. Enrolled a week ago.
		{"three days after enrolment, open", days(enrolled(DripAfterEnrolment), 3), learner, AccessEnrolled},
		{"ten days after enrolment, locked", days(enrolled(DripAfterEnrolment), 10), learner, AccessLocked},
		{"zero days after enrolment, open", days(enrolled(DripAfterEnrolment), 0), learner, AccessEnrolled},
		{"after enrolment with no delay set is open", enrolled(DripAfterEnrolment), learner, AccessEnrolled},

		// Sequential: opens on an event, not a clock.
		{"sequential, nothing left before it", enrolled(DripSequential), learner, AccessEnrolled},
		{"sequential, one lesson unfinished", prior(enrolled(DripSequential), 1), learner, AccessLocked},
		{"sequential ignores a stray date", at(prior(enrolled(DripSequential), 1), yesterday), learner, AccessLocked},

		// A preview is a free sample. Locking it for the learner who paid would mean
		// a stranger reads what a customer cannot.
		{"a preview is never dripped, scheduled", preview(at(enrolled(DripScheduled), tomorrow)), learner, AccessEnrolled},
		{"a preview is never dripped, sequential", preview(prior(enrolled(DripSequential), 3)), learner, AccessEnrolled},

		// Drip binds enrolled learners and nobody else.
		{"an author sees a locked lesson", at(enrolled(DripScheduled), tomorrow), authorReader, AccessAuthor},
		{"a stranger still gets a preview", preview(at(enrolled(DripScheduled), tomorrow)), anonymous, AccessPreview},
		{"a stranger is still denied a paid lesson", at(enrolled(DripScheduled), tomorrow), anonymous, AccessDenied},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := decide(tt.view, tt.reader, now); got != tt.want {
				t.Errorf("decide = %v, want %v", got, tt.want)
			}
		})
	}
}

// A locked lesson never grants access, whatever else it says.
func TestAccessLockedGrantsNothing(t *testing.T) {
	t.Parallel()

	if AccessLocked.Granted() {
		t.Error("a locked lesson is readable")
	}
	if AccessLocked.String() != "locked" {
		t.Errorf("String() = %q, want locked", AccessLocked.String())
	}
}

// The unlock time is offered when it is a time at all, and withheld when nobody
// can know it.
func TestUnlockAt(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	enrolledAt := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	opensAt := time.Date(2026, 7, 20, 8, 0, 0, 0, time.UTC)

	active := StatusActive
	base := LessonView{CourseStatus: "published", EnrolmentStatus: &active, EnrolledAt: &enrolledAt}

	t.Run("scheduled offers the course's date", func(t *testing.T) {
		v := base
		v.DripMode = DripScheduled
		v.Lesson.AvailableAt = &opensAt

		locked, at := unlockAt(v, now)
		if !locked || at == nil || !at.Equal(opensAt) {
			t.Fatalf("locked=%v at=%v, want locked at %v", locked, at, opensAt)
		}
	})

	t.Run("after enrolment counts from this learner's own date", func(t *testing.T) {
		v := base
		v.DripMode = DripAfterEnrolment
		ten := 10
		v.AvailableAfterDays = &ten

		locked, at := unlockAt(v, now)
		want := enrolledAt.AddDate(0, 0, 10)
		if !locked || at == nil || !at.Equal(want) {
			t.Fatalf("locked=%v at=%v, want locked at %v", locked, at, want)
		}
	})

	t.Run("sequential offers no date", func(t *testing.T) {
		v := base
		v.DripMode = DripSequential
		v.PriorIncomplete = 2

		locked, at := unlockAt(v, now)
		if !locked {
			t.Fatal("an unfinished predecessor did not lock the lesson")
		}
		if at != nil {
			t.Errorf("at = %v, want nil — nobody knows when they will finish", at)
		}
	})
}

// The date a client is shown must be the one the server enforces. A course that
// stops dripping stops promising dates, even though the column still holds one.
func TestScheduleFollowsTheMode(t *testing.T) {
	t.Parallel()

	opensAt := time.Date(2026, 7, 20, 8, 0, 0, 0, time.UTC)
	enrolledAt := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	ten := 10

	active := StatusActive
	base := LessonView{CourseStatus: "published", EnrolmentStatus: &active, EnrolledAt: &enrolledAt}
	base.Lesson.AvailableAt = &opensAt
	base.AvailableAfterDays = &ten

	t.Run("none reads neither column", func(t *testing.T) {
		v := base
		v.DripMode = DripNone
		if at := schedule(v); at != nil {
			t.Errorf("schedule = %v, want nil", at)
		}
	})

	t.Run("sequential reads neither column", func(t *testing.T) {
		v := base
		v.DripMode = DripSequential
		if at := schedule(v); at != nil {
			t.Errorf("schedule = %v, want nil", at)
		}
	})

	t.Run("scheduled reads only available_at", func(t *testing.T) {
		v := base
		v.DripMode = DripScheduled
		if at := schedule(v); at == nil || !at.Equal(opensAt) {
			t.Errorf("schedule = %v, want %v", at, opensAt)
		}
	})

	t.Run("after enrolment reads only the delay", func(t *testing.T) {
		v := base
		v.DripMode = DripAfterEnrolment
		want := enrolledAt.AddDate(0, 0, ten)
		if at := schedule(v); at == nil || !at.Equal(want) {
			t.Errorf("schedule = %v, want %v", at, want)
		}
	})

	t.Run("a preview has no schedule at all", func(t *testing.T) {
		v := base
		v.DripMode = DripScheduled
		v.Lesson.IsPreview = true
		if at := schedule(v); at != nil {
			t.Errorf("schedule = %v, want nil", at)
		}
	})
}

func TestValidDripMode(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{DripNone, DripScheduled, DripAfterEnrolment, DripSequential} {
		if !ValidDripMode(mode) {
			t.Errorf("%q is not accepted", mode)
		}
	}
	for _, mode := range []string{"", "weekly", "NONE", "sequential "} {
		if ValidDripMode(mode) {
			t.Errorf("%q was accepted", mode)
		}
	}
}
