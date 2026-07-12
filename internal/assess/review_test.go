package assess_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/ebnsina/muallim-api/internal/assess"
	"github.com/ebnsina/muallim-api/internal/platform/database"
)

// essayAttempt builds a quiz with one machine-graded question and one essay,
// answers both, submits, and grades it — leaving an attempt awaiting review.
func essayAttempt(t *testing.T, f fixture, bar int) (attemptID, essayID uuid.UUID) {
	t.Helper()

	_, choiceID, _ := f.quiz(t, assess.NewQuiz{PassingPercent: bar})

	essay, err := f.svc.AddQuestion(t.Context(), f.tenant, f.lesson, assess.NewQuestion{
		Type: assess.TypeOpenEnded, Prompt: "Discuss.", Points: 5,
	}, f.author)
	if err != nil {
		t.Fatalf("add essay: %v", err)
	}

	learner := assess.Author{UserID: f.learner}
	if _, err := f.svc.StartAttempt(t.Context(), f.tenant, f.lesson, f.learner, learner); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := f.svc.SaveAnswer(t.Context(), f.tenant, f.lesson, f.learner, choiceID,
		assess.Response{Choices: []uuid.UUID{f.correctOption(t, choiceID)}}); err != nil {
		t.Fatalf("save choice: %v", err)
	}
	if err := f.svc.SaveAnswer(t.Context(), f.tenant, f.lesson, f.learner, essay.ID,
		assess.Response{Text: "A considered answer."}); err != nil {
		t.Fatalf("save essay: %v", err)
	}

	submitted, err := f.svc.SubmitAttempt(t.Context(), f.tenant, f.lesson, f.learner, learner)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	graded, err := f.svc.GradeAttempt(t.Context(), f.tenant, submitted.ID)
	if err != nil {
		t.Fatalf("grade: %v", err)
	}
	if graded.Status != assess.StatusAwaitingReview {
		t.Fatalf("attempt is %q, want %q", graded.Status, assess.StatusAwaitingReview)
	}

	return submitted.ID, essay.ID
}

// Marking the last essay is what turns `awaiting_review` into `graded`, and only
// then does the attempt acquire a pass. The roll-up moves in the same transaction
// as the answer it summarises.
func TestMarkingTheLastEssayFinishesTheAttempt(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	attemptID, essayID := essayAttempt(t, f, 60)

	// 3 machine points of 10 so far, and no verdict while an essay is unmarked.
	before, _, _, err := f.svc.Submission(t.Context(), f.tenant, attemptID)
	if err != nil {
		t.Fatalf("submission: %v", err)
	}
	if before.Points != 3 || before.MaxPoints != 10 || before.Passed != nil {
		t.Fatalf("before marking: %d/%d passed=%v", before.Points, before.MaxPoints, before.Passed)
	}

	marked, err := f.svc.MarkAnswer(t.Context(), f.tenant, attemptID, essayID,
		assess.Mark{Points: 4, Feedback: "Good, but you never defined the term."}, f.author)
	if err != nil {
		t.Fatalf("mark: %v", err)
	}

	if marked.Status != assess.StatusGraded {
		t.Errorf("status = %q, want %q", marked.Status, assess.StatusGraded)
	}
	if marked.Points != 7 || marked.MaxPoints != 10 {
		t.Errorf("scored %d/%d, want 7/10", marked.Points, marked.MaxPoints)
	}
	if marked.Passed == nil || !*marked.Passed {
		t.Errorf("7 of 10 at a 60%% bar: passed = %v", marked.Passed)
	}
	if marked.GradedAt == nil {
		t.Error("a graded attempt has no graded_at")
	}

	// The learner sees the mark, the feedback, and now the explanations.
	review, err := f.svc.Review(t.Context(), f.tenant, f.lesson, f.learner, 1)
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	for _, item := range review.Items {
		if !item.Graded {
			t.Errorf("question %q is still ungraded", item.Prompt)
		}
		if item.Type == assess.TypeOpenEnded {
			if item.Points != 4 || item.Feedback == "" {
				t.Errorf("the essay reads %d points, feedback %q", item.Points, item.Feedback)
			}
			// Four of five is neither right nor wrong. The tick is for full marks.
			if item.Correct {
				t.Error("an essay awarded 4 of 5 was marked correct")
			}
		}
	}
}

// An award of everything is a tick; an award of nothing is not a pass.
func TestAMarkOfFullPointsIsCorrectAndAMarkOfZeroFails(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	attemptID, essayID := essayAttempt(t, f, 60)

	marked, err := f.svc.MarkAnswer(t.Context(), f.tenant, attemptID, essayID, assess.Mark{Points: 0}, f.author)
	if err != nil {
		t.Fatalf("mark: %v", err)
	}
	if marked.Points != 3 || marked.Passed == nil || *marked.Passed {
		t.Errorf("3 of 10 at a 60%% bar: %d points, passed = %v", marked.Points, marked.Passed)
	}

	_, _, answers, err := f.svc.Submission(t.Context(), f.tenant, attemptID)
	if err != nil {
		t.Fatalf("submission: %v", err)
	}
	if answer := answers[essayID]; !answer.Graded || answer.Correct {
		t.Errorf("a zero-mark essay: graded=%v correct=%v", answer.Graded, answer.Correct)
	}
}

// A marker may not award more than the question is worth, nor a negative mark.
func TestAMarkMustFitTheQuestion(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	attemptID, essayID := essayAttempt(t, f, 60)

	for _, points := range []int{-1, 6, 100} {
		_, err := f.svc.MarkAnswer(t.Context(), f.tenant, attemptID, essayID, assess.Mark{Points: points}, f.author)
		if !errors.Is(err, assess.ErrInvalidGrade) {
			t.Errorf("awarding %d of a 5-point essay: %v, want ErrInvalidGrade", points, err)
		}
	}

	// And the attempt is untouched by the refusals.
	attempt, _, _, err := f.svc.Submission(t.Context(), f.tenant, attemptID)
	if err != nil {
		t.Fatalf("submission: %v", err)
	}
	if attempt.Status != assess.StatusAwaitingReview || attempt.Points != 3 {
		t.Errorf("a refused mark changed the attempt: %q, %d points", attempt.Status, attempt.Points)
	}
}

// An instructor who could overwrite the machine's verdict on a multiple-choice
// question could quietly make a wrong answer right.
func TestAMachineGradedQuestionCannotBeMarkedByHand(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	attemptID, _ := essayAttempt(t, f, 60)

	_, questions, _, err := f.svc.Submission(t.Context(), f.tenant, attemptID)
	if err != nil {
		t.Fatalf("submission: %v", err)
	}

	var choiceID uuid.UUID
	for _, q := range questions {
		if q.Type == assess.TypeSingleChoice {
			choiceID = q.ID
		}
	}

	_, err = f.svc.MarkAnswer(t.Context(), f.tenant, attemptID, choiceID, assess.Mark{Points: 3}, f.author)
	if !errors.Is(err, assess.ErrNotManual) {
		t.Fatalf("marking a single-choice question: %v, want ErrNotManual", err)
	}
}

// A settled grade is not re-marked by this path. Changing one is a different act,
// with a different audit line, and it does not exist yet.
func TestAGradedAttemptIsNotReMarked(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	attemptID, essayID := essayAttempt(t, f, 60)

	if _, err := f.svc.MarkAnswer(t.Context(), f.tenant, attemptID, essayID, assess.Mark{Points: 5}, f.author); err != nil {
		t.Fatalf("mark: %v", err)
	}

	_, err := f.svc.MarkAnswer(t.Context(), f.tenant, attemptID, essayID, assess.Mark{Points: 0}, f.author)
	if !errors.Is(err, assess.ErrNotAwaitingReview) {
		t.Fatalf("re-marking a graded attempt: %v, want ErrNotAwaitingReview", err)
	}
}

// An author who edits the quiz while an essay waits must not restate what the
// learner was scored out of. max_points was frozen when the machine graded it.
func TestEditingTheQuizDoesNotRestateAWaitingAttempt(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	attemptID, essayID := essayAttempt(t, f, 60)

	// A new 50-point question appears after the attempt was graded.
	if _, err := f.svc.AddQuestion(t.Context(), f.tenant, f.lesson, assess.NewQuestion{
		Type: assess.TypeShortAnswer, Prompt: "And this?", Points: 50, Accepted: [][]string{{"yes"}},
	}, f.author); err != nil {
		t.Fatalf("add question: %v", err)
	}

	marked, err := f.svc.MarkAnswer(t.Context(), f.tenant, attemptID, essayID, assess.Mark{Points: 5}, f.author)
	if err != nil {
		t.Fatalf("mark: %v", err)
	}

	if marked.MaxPoints != 10 {
		t.Errorf("max_points = %d, want the 10 the attempt was graded out of", marked.MaxPoints)
	}
	if marked.Points != 8 || marked.Status != assess.StatusGraded {
		t.Errorf("scored %d/%d, %q", marked.Points, marked.MaxPoints, marked.Status)
	}
}

// The marking queue names the learner and counts what is left, in one query per
// page — never one per attempt.
func TestTheMarkingQueue(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	attemptID, essayID := essayAttempt(t, f, 60)

	counter := &database.Counter{}
	ctx := database.WithCounter(t.Context(), counter)

	waiting, err := f.svc.Submissions(ctx, f.tenant, f.lesson, true, 50)
	if err != nil {
		t.Fatalf("submissions: %v", err)
	}
	if len(waiting) != 1 {
		t.Fatalf("the queue holds %d attempts, want 1", len(waiting))
	}
	if waiting[0].Attempt.ID != attemptID {
		t.Errorf("the queue holds the wrong attempt")
	}
	if waiting[0].LearnerEmail == "" || waiting[0].Unmarked != 1 {
		t.Errorf("submission: learner %q, %d unmarked", waiting[0].LearnerEmail, waiting[0].Unmarked)
	}

	// quiz, then the list. The unmarked count rides along in the same query, or a
	// queue of a hundred attempts would issue a hundred more.
	if got := counter.Count(); got != 2 {
		t.Errorf("listing the queue issued %d queries, want 2", got)
	}

	// Marking it empties the queue, and the attempt stays in the full history.
	if _, err := f.svc.MarkAnswer(t.Context(), f.tenant, attemptID, essayID, assess.Mark{Points: 5}, f.author); err != nil {
		t.Fatalf("mark: %v", err)
	}

	if waiting, err = f.svc.Submissions(t.Context(), f.tenant, f.lesson, true, 50); err != nil {
		t.Fatalf("submissions: %v", err)
	}
	if len(waiting) != 0 {
		t.Errorf("a marked attempt is still in the queue")
	}

	all, err := f.svc.Submissions(t.Context(), f.tenant, f.lesson, false, 50)
	if err != nil {
		t.Fatalf("submissions: %v", err)
	}
	if len(all) != 1 || all[0].Attempt.Status != assess.StatusGraded {
		t.Errorf("the history holds %d attempts", len(all))
	}
}

// A marker has no business reading a half-written essay, and an attempt still
// being taken is not a submission.
func TestTheQueueExcludesAttemptsStillBeingTaken(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.quiz(t, assess.NewQuiz{})

	if _, err := f.svc.StartAttempt(t.Context(), f.tenant, f.lesson, f.learner, assess.Author{UserID: f.learner}); err != nil {
		t.Fatalf("start: %v", err)
	}

	all, err := f.svc.Submissions(t.Context(), f.tenant, f.lesson, false, 50)
	if err != nil {
		t.Fatalf("submissions: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("an unsubmitted attempt appeared in the marking queue: %+v", all[0].Attempt)
	}
}
