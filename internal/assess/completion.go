package assess

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Completions marks a lesson complete when its quiz is passed.
//
// Declared here, by the package that needs it, and implemented in cmd/ over the
// enrolment service — a domain package may not import a sibling. The method takes
// the caller's transaction, so the grade and the progress it implies commit
// together. A grade without its progress is a learner whose gradebook and course
// page disagree.
//
// The bool answers "did the lesson actually get marked". It is not an error for
// the answer to be no: a learner may have cancelled their enrolment between
// submitting and the worker reaching the job, and the honest outcome then is a
// graded attempt and no progress — not a grading job that retries for ever
// against a state that will never change.
type Completions interface {
	TryCompleteLesson(ctx context.Context, tx pgx.Tx, tenantID, lessonID, userID uuid.UUID) (bool, error)
}

// settle completes the quiz's lesson when the attempt has passed it.
//
// Passing a quiz completes its lesson. Failing one does not un-complete it, and
// neither does a later failed retry: a completion is a thing that happened, and a
// learner does not lose it by trying to do better. Marking a lesson complete is
// idempotent, so a retried grading job settles it again to the same value.
//
// Called from the two places an attempt can reach `graded`: the grading job, and
// an instructor marking the last essay.
func (s *Service) settle(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, quiz Quiz, attempt Attempt) error {
	if attempt.Status != StatusGraded || attempt.Passed == nil || !*attempt.Passed {
		return nil
	}

	_, err := s.completions.TryCompleteLesson(ctx, tx, tenantID, quiz.LessonID, attempt.UserID)
	return err
}
