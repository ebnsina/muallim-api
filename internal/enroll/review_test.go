package enroll_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/ebnsina/muallim-api/internal/enroll"
)

func TestReviewRequiresEnrolmentThenListsAndSummarises(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	slug, _ := seedCourse(t, db, tenantID, 2, true)

	stranger := seedUser(t, db, tenantID)
	learner := seedUser(t, db, tenantID)

	// A stranger who never enrolled may not review.
	if _, err := svc.Review(t.Context(), tenantID, slug, actorFor(stranger), 5, "great"); !errors.Is(err, enroll.ErrNotEnrolled) {
		t.Fatalf("stranger review: got %v, want ErrNotEnrolled", err)
	}

	// A rating out of range is refused before any enrolment check leaks existence.
	if _, err := svc.Review(t.Context(), tenantID, slug, actorFor(learner), 6, ""); !errors.Is(err, enroll.ErrInvalidReview) {
		t.Fatalf("out-of-range rating: got %v, want ErrInvalidReview", err)
	}

	if _, err := svc.Enrol(t.Context(), tenantID, slug, actorFor(learner), enroll.SourceSelf); err != nil {
		t.Fatalf("enrol: %v", err)
	}

	rev, err := svc.Review(t.Context(), tenantID, slug, actorFor(learner), 4, "solid course")
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if rev.Rating != 4 || rev.Body != "solid course" {
		t.Fatalf("review round-trip: %+v", rev)
	}

	// Submitting again edits rather than duplicating.
	if _, err := svc.Review(t.Context(), tenantID, slug, actorFor(learner), 5, "even better on a second pass"); err != nil {
		t.Fatalf("re-review: %v", err)
	}

	list, summary, mine, err := svc.Reviews(t.Context(), tenantID, slug, learner, 20)
	if err != nil {
		t.Fatalf("reviews: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("want one review after edit, got %d", len(list))
	}
	if summary.Count != 1 || summary.Average != 5 {
		t.Fatalf("summary: %+v, want count 1 avg 5", summary)
	}
	if mine == nil || mine.Rating != 5 {
		t.Fatalf("own review not returned: %+v", mine)
	}

	// Retracting removes it; retracting again is not an error.
	if err := svc.UnReview(t.Context(), tenantID, slug, actorFor(learner)); err != nil {
		t.Fatalf("unreview: %v", err)
	}
	if err := svc.UnReview(t.Context(), tenantID, slug, actorFor(learner)); err != nil {
		t.Fatalf("unreview again: %v", err)
	}
	list, summary, mine, err = svc.Reviews(t.Context(), tenantID, slug, learner, 20)
	if err != nil {
		t.Fatalf("reviews after retract: %v", err)
	}
	if len(list) != 0 || summary.Count != 0 || mine != nil {
		t.Fatalf("review survived retraction: list=%d summary=%+v mine=%+v", len(list), summary, mine)
	}
}

func TestAnalyticsCountsEnrolmentsProgressAndRating(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	slug, lessons := seedCourse(t, db, tenantID, 2, true)

	finisher := seedUser(t, db, tenantID)
	starter := seedUser(t, db, tenantID)

	for _, u := range []uuid.UUID{finisher, starter} {
		if _, err := svc.Enrol(t.Context(), tenantID, slug, actorFor(u), enroll.SourceSelf); err != nil {
			t.Fatalf("enrol: %v", err)
		}
	}

	// The finisher completes both lessons; the starter completes one.
	for _, id := range lessons {
		if _, err := svc.CompleteLesson(t.Context(), tenantID, id, actorFor(finisher), true); err != nil {
			t.Fatalf("complete: %v", err)
		}
	}
	if _, err := svc.CompleteLesson(t.Context(), tenantID, lessons[0], actorFor(starter), true); err != nil {
		t.Fatalf("complete one: %v", err)
	}

	// The finisher rates it.
	if _, err := svc.Review(t.Context(), tenantID, slug, actorFor(finisher), 4, "good"); err != nil {
		t.Fatalf("review: %v", err)
	}

	a, err := svc.Analytics(t.Context(), tenantID, slug)
	if err != nil {
		t.Fatalf("analytics: %v", err)
	}
	if a.Total != 2 {
		t.Fatalf("total enrolments = %d, want 2", a.Total)
	}
	if a.Completed != 1 || a.Active != 1 {
		t.Fatalf("completed=%d active=%d, want 1 and 1", a.Completed, a.Active)
	}
	if a.CompletionRate() != 0.5 {
		t.Fatalf("completion rate = %v, want 0.5", a.CompletionRate())
	}
	// Progress is 100 and 50, mean 75.
	if a.AvgProgress != 75 {
		t.Fatalf("avg progress = %v, want 75", a.AvgProgress)
	}
	if a.Reviews.Count != 1 || a.Reviews.Average != 4 {
		t.Fatalf("reviews = %+v, want count 1 avg 4", a.Reviews)
	}
}
