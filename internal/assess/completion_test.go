package assess_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/ebnsina/lms-api/internal/assess"
	"github.com/ebnsina/lms-api/internal/enroll"
)

// take answers the choice question rightly or wrongly, submits, and grades.
func take(t *testing.T, f fixture, choiceID uuid.UUID, correctly bool) assess.Attempt {
	t.Helper()

	learner := assess.Author{UserID: f.learner}
	if _, err := f.svc.StartAttempt(t.Context(), f.tenant, f.lesson, f.learner, learner); err != nil {
		t.Fatalf("start: %v", err)
	}

	choice := f.correctOption(t, choiceID)
	if !correctly {
		choice = uuid.New() // an option belonging to nothing: a wrong answer.
	}
	if err := f.svc.SaveAnswer(t.Context(), f.tenant, f.lesson, f.learner, choiceID,
		assess.Response{Choices: []uuid.UUID{choice}}); err != nil {
		t.Fatalf("save: %v", err)
	}

	submitted, err := f.svc.SubmitAttempt(t.Context(), f.tenant, f.lesson, f.learner, learner)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	graded, err := f.svc.GradeAttempt(t.Context(), f.tenant, submitted.ID)
	if err != nil {
		t.Fatalf("grade: %v", err)
	}
	return graded
}

// The seam. Passing a quiz completes its lesson, in the transaction that recorded
// the grade — so a learner's gradebook and their course page can never disagree.
func TestPassingAQuizCompletesItsLesson(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.enrol(t)

	// The choice question is worth 3 of 5; a 60% bar means answering it is passing.
	_, choiceID, _ := f.quiz(t, assess.NewQuiz{PassingPercent: 60})

	if before := f.progress(t); before.LessonsCompleted != 0 {
		t.Fatalf("before the quiz: %d lessons complete", before.LessonsCompleted)
	}

	graded := take(t, f, choiceID, true)
	if graded.Passed == nil || !*graded.Passed {
		t.Fatalf("the attempt did not pass: %+v", graded)
	}

	after := f.progress(t)
	if after.LessonsCompleted != 1 || after.Percent != 100 {
		t.Errorf("after passing: %d of %d lessons, %d%%",
			after.LessonsCompleted, after.LessonsTotal, after.Percent)
	}
}

// Failing does not.
func TestFailingAQuizLeavesTheLessonAlone(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.enrol(t)
	_, choiceID, _ := f.quiz(t, assess.NewQuiz{PassingPercent: 60})

	graded := take(t, f, choiceID, false)
	if graded.Passed == nil || *graded.Passed {
		t.Fatalf("a wrong answer passed: %+v", graded)
	}

	if after := f.progress(t); after.LessonsCompleted != 0 {
		t.Errorf("failing the quiz completed the lesson")
	}
}

// A completion is a thing that happened. A learner does not lose it by trying to
// do better and doing worse.
func TestAFailedRetryDoesNotUncompleteTheLesson(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.enrol(t)
	_, choiceID, _ := f.quiz(t, assess.NewQuiz{PassingPercent: 60})

	take(t, f, choiceID, true)
	if after := f.progress(t); after.LessonsCompleted != 1 {
		t.Fatalf("passing did not complete the lesson")
	}

	take(t, f, choiceID, false)
	if after := f.progress(t); after.LessonsCompleted != 1 {
		t.Errorf("a failed second attempt took the completion away")
	}
}

// Marking the last essay is the other way an attempt reaches `graded`, and it
// completes the lesson for the same reason and in the same transaction.
func TestMarkingTheLastEssayCompletesTheLesson(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.enrol(t)

	// 3 machine points of 10; the essay is worth 5. Only marking it can reach 60%.
	attemptID, essayID := essayAttempt(t, f, 60)

	if awaiting := f.progress(t); awaiting.LessonsCompleted != 0 {
		t.Fatalf("an attempt awaiting review completed the lesson")
	}

	marked, err := f.svc.MarkAnswer(t.Context(), f.tenant, attemptID, essayID, assess.Mark{Points: 5}, f.author)
	if err != nil {
		t.Fatalf("mark: %v", err)
	}
	if marked.Passed == nil || !*marked.Passed {
		t.Fatalf("8 of 10 did not pass a 60%% bar: %+v", marked)
	}

	if after := f.progress(t); after.LessonsCompleted != 1 {
		t.Errorf("marking the last essay did not complete the lesson")
	}
}

// A learner who cancels their enrolment between submitting and grading leaves a
// graded attempt and no progress. Not an error: nothing about that state changes
// on a retry, and a job that failed here would roll the grade back with it.
func TestGradingSurvivesALearnerWhoLeft(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.enrol(t)
	_, choiceID, _ := f.quiz(t, assess.NewQuiz{PassingPercent: 60})

	learner := assess.Author{UserID: f.learner}
	if _, err := f.svc.StartAttempt(t.Context(), f.tenant, f.lesson, f.learner, learner); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := f.svc.SaveAnswer(t.Context(), f.tenant, f.lesson, f.learner, choiceID,
		assess.Response{Choices: []uuid.UUID{f.correctOption(t, choiceID)}}); err != nil {
		t.Fatalf("save: %v", err)
	}
	submitted, err := f.svc.SubmitAttempt(t.Context(), f.tenant, f.lesson, f.learner, learner)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	// They leave before the worker gets there.
	if err := f.learning.Cancel(t.Context(), f.tenant, f.course, enroll.Actor{UserID: f.learner}); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	graded, err := f.svc.GradeAttempt(t.Context(), f.tenant, submitted.ID)
	if err != nil {
		t.Fatalf("grading a departed learner's attempt failed: %v", err)
	}
	if graded.Status != assess.StatusGraded || graded.Passed == nil || !*graded.Passed {
		t.Errorf("the attempt was not graded: %+v", graded)
	}
}
