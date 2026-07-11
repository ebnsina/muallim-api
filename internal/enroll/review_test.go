package enroll_test

import (
	"errors"
	"testing"

	"github.com/ebnsina/lms-api/internal/enroll"
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
