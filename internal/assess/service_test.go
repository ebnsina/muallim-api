package assess_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/lms-api/internal/assess"
	"github.com/ebnsina/lms-api/internal/audit"
	"github.com/ebnsina/lms-api/internal/platform/database"
)

func testDB(t *testing.T) *database.DB {
	t.Helper()

	url := os.Getenv("LMS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LMS_TEST_DATABASE_URL is not set; skipping database tests")
	}

	log := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	db, err := database.New(t.Context(), database.Options{URL: url, MaxConns: 4}, log)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

type assessAuditor struct{ recorder *audit.Recorder }

func (a assessAuditor) Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e assess.AuditEntry) error {
	return a.recorder.Record(ctx, tx, tenantID, audit.Entry{
		ActorID: e.ActorID, Action: e.Action,
		TargetType: e.TargetType, TargetID: e.TargetID,
		IP: e.IP, UserAgent: e.UserAgent, Metadata: e.Metadata,
	})
}

// recordingEnqueuer stands in for River. It records what was queued, on the
// transaction it was queued in — so a test can assert that the job was enqueued
// and that it did not survive a rollback.
type recordingEnqueuer struct {
	mu       sync.Mutex
	attempts []uuid.UUID
	err      error
}

func (e *recordingEnqueuer) GradeAttempt(_ context.Context, _ pgx.Tx, _, attemptID uuid.UUID) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.err != nil {
		return e.err
	}
	e.attempts = append(e.attempts, attemptID)
	return nil
}

func (e *recordingEnqueuer) queued() []uuid.UUID {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]uuid.UUID(nil), e.attempts...)
}

func newService(db *database.DB, jobs assess.Enqueuer) *assess.Service {
	return assess.NewService(db, assess.NewPostgresRepository(), assessAuditor{audit.NewRecorder()}, jobs)
}

func seedTenant(t *testing.T, db *database.DB) uuid.UUID {
	t.Helper()

	id := uuid.New()
	err := db.WithoutTenant(t.Context(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, subdomain, name) VALUES ($1, $2, 'Test')`,
			id, "t"+id.String()[:8])
		return err
	})
	if err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	t.Cleanup(func() {
		_ = db.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM tenants WHERE id = $1`, id)
			return err
		})
	})
	return id
}

func seedUser(t *testing.T, db *database.DB, tenantID uuid.UUID) uuid.UUID {
	t.Helper()

	id := uuid.New()
	err := db.WithTenant(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO users (id, email, password_hash, name) VALUES ($1, $2, 'x', 'U')`,
			id, "u-"+id.String()[:8]+"@example.test"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO memberships (tenant_id, user_id, role) VALUES ($1, $2, 'student')`, tenantID, id)
		return err
	})
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

// seedLesson makes the smallest course that can hold a lesson.
func seedLesson(t *testing.T, db *database.DB, tenantID uuid.UUID) uuid.UUID {
	t.Helper()

	var lessonID uuid.UUID
	err := db.WithTenant(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var courseID, topicID uuid.UUID
		if err := tx.QueryRow(ctx,
			`INSERT INTO courses (tenant_id, slug, title, status)
			 VALUES ($1, $2, 'Course', 'published') RETURNING id`,
			tenantID, "c-"+uuid.NewString()[:8]).Scan(&courseID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO topics (tenant_id, course_id, title, position) VALUES ($1, $2, 'T', 0) RETURNING id`,
			tenantID, courseID).Scan(&topicID); err != nil {
			return err
		}
		return tx.QueryRow(ctx,
			`INSERT INTO lessons (tenant_id, topic_id, title, content_type, position)
			 VALUES ($1, $2, 'Quiz lesson', 'quiz', 0) RETURNING id`,
			tenantID, topicID).Scan(&lessonID)
	})
	if err != nil {
		t.Fatalf("seed lesson: %v", err)
	}
	return lessonID
}

type fixture struct {
	db      *database.DB
	svc     *assess.Service
	jobs    *recordingEnqueuer
	tenant  uuid.UUID
	lesson  uuid.UUID
	learner uuid.UUID
	author  assess.Author
}

func newFixture(t *testing.T) fixture {
	t.Helper()

	db := testDB(t)
	jobs := &recordingEnqueuer{}
	tenantID := seedTenant(t, db)

	return fixture{
		db: db, svc: newService(db, jobs), jobs: jobs,
		tenant: tenantID, lesson: seedLesson(t, db, tenantID),
		learner: seedUser(t, db, tenantID),
		author:  assess.Author{UserID: seedUser(t, db, tenantID)},
	}
}

// quiz builds a two-question quiz: one that grades itself, one that does not.
func (f fixture) quiz(t *testing.T, settings assess.NewQuiz) (assess.Quiz, uuid.UUID, uuid.UUID) {
	t.Helper()

	if settings.Title == "" {
		settings.Title = "Chapter one"
	}

	quiz, err := f.svc.CreateQuiz(t.Context(), f.tenant, f.lesson, settings, f.author)
	if err != nil {
		t.Fatalf("create quiz: %v", err)
	}

	choice, err := f.svc.AddQuestion(t.Context(), f.tenant, f.lesson, assess.NewQuestion{
		Type: assess.TypeSingleChoice, Prompt: "Which?", Points: 3, Explanation: "Because B.",
		Options: []assess.NewOption{{Content: "A"}, {Content: "B", IsCorrect: true}},
	}, f.author)
	if err != nil {
		t.Fatalf("add question: %v", err)
	}

	typed, err := f.svc.AddQuestion(t.Context(), f.tenant, f.lesson, assess.NewQuestion{
		Type: assess.TypeShortAnswer, Prompt: "Capital of France?", Points: 2,
		Accepted: [][]string{{"Paris"}},
	}, f.author)
	if err != nil {
		t.Fatalf("add question: %v", err)
	}

	return quiz, choice.ID, typed.ID
}

// correctOption finds the id a learner would have to pick. It reads the author's
// view, which is the only view that knows.
func (f fixture) correctOption(t *testing.T, questionID uuid.UUID) uuid.UUID {
	t.Helper()

	_, questions, err := f.svc.AuthoredQuiz(t.Context(), f.tenant, f.lesson)
	if err != nil {
		t.Fatalf("authored quiz: %v", err)
	}
	for _, q := range questions {
		if q.ID != questionID {
			continue
		}
		for _, o := range q.Options {
			if o.IsCorrect {
				return o.ID
			}
		}
	}
	t.Fatalf("no correct option on question %s", questionID)
	return uuid.Nil
}

// The whole loop: start, answer, submit, grade. Nothing is graded in the request
// that submits — that is the point of the queue.
func TestTheAttemptLoop(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	_, choiceID, typedID := f.quiz(t, assess.NewQuiz{PassingPercent: 60})

	attempt, err := f.svc.StartAttempt(t.Context(), f.tenant, f.lesson, f.learner, assess.Author{UserID: f.learner})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if attempt.Number != 1 || attempt.Status != assess.StatusInProgress {
		t.Fatalf("attempt %d is %q, want the first, in progress", attempt.Number, attempt.Status)
	}

	right := f.correctOption(t, choiceID)
	if err := f.svc.SaveAnswer(t.Context(), f.tenant, f.lesson, f.learner, choiceID,
		assess.Response{Choices: []uuid.UUID{right}}); err != nil {
		t.Fatalf("save choice: %v", err)
	}
	if err := f.svc.SaveAnswer(t.Context(), f.tenant, f.lesson, f.learner, typedID,
		assess.Response{Text: "  pARIS "}); err != nil {
		t.Fatalf("save typed: %v", err)
	}

	submitted, err := f.svc.SubmitAttempt(t.Context(), f.tenant, f.lesson, f.learner, assess.Author{UserID: f.learner})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	// Submitted, not graded. A handler that graded here would hold the request open
	// for as long as grading took.
	if submitted.Status != assess.StatusGrading {
		t.Errorf("after submit the attempt is %q, want %q", submitted.Status, assess.StatusGrading)
	}
	if submitted.Points != 0 {
		t.Errorf("the submitting request scored the attempt: %d points", submitted.Points)
	}

	// And the job was queued, on that same transaction.
	if queued := f.jobs.queued(); len(queued) != 1 || queued[0] != submitted.ID {
		t.Fatalf("queued %v, want exactly the attempt %s", queued, submitted.ID)
	}

	// Now the worker's half.
	graded, err := f.svc.GradeAttempt(t.Context(), f.tenant, submitted.ID)
	if err != nil {
		t.Fatalf("grade: %v", err)
	}

	if graded.Status != assess.StatusGraded {
		t.Fatalf("graded attempt is %q", graded.Status)
	}
	if graded.Points != 5 || graded.MaxPoints != 5 {
		t.Errorf("scored %d/%d, want 5/5", graded.Points, graded.MaxPoints)
	}
	if graded.Passed == nil || !*graded.Passed {
		t.Errorf("passed = %v, want true", graded.Passed)
	}
	if graded.Percent() != 100 {
		t.Errorf("percent = %d", graded.Percent())
	}

	// The review says what was right, and now releases the explanation.
	review, err := f.svc.Review(t.Context(), f.tenant, f.lesson, f.learner, 1)
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if len(review.Items) != 2 {
		t.Fatalf("review has %d items, want 2", len(review.Items))
	}
	for _, item := range review.Items {
		if !item.Graded || !item.Correct {
			t.Errorf("question %s: graded=%v correct=%v", item.Prompt, item.Graded, item.Correct)
		}
	}
	if review.Items[0].Explanation != "Because B." {
		t.Errorf("a graded attempt withheld its explanation: %q", review.Items[0].Explanation)
	}
}

// Grading is a job, and jobs are retried. A second run must reach the same
// conclusion and must not undo a grade a person has since given.
func TestGradingIsIdempotent(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	_, choiceID, _ := f.quiz(t, assess.NewQuiz{PassingPercent: 60})

	if _, err := f.svc.StartAttempt(t.Context(), f.tenant, f.lesson, f.learner, assess.Author{UserID: f.learner}); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := f.svc.SaveAnswer(t.Context(), f.tenant, f.lesson, f.learner, choiceID,
		assess.Response{Choices: []uuid.UUID{f.correctOption(t, choiceID)}}); err != nil {
		t.Fatalf("save: %v", err)
	}

	submitted, err := f.svc.SubmitAttempt(t.Context(), f.tenant, f.lesson, f.learner, assess.Author{UserID: f.learner})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	first, err := f.svc.GradeAttempt(t.Context(), f.tenant, submitted.ID)
	if err != nil {
		t.Fatalf("grade: %v", err)
	}

	second, err := f.svc.GradeAttempt(t.Context(), f.tenant, submitted.ID)
	if err != nil {
		t.Fatalf("regrade: %v", err)
	}

	if first.Points != second.Points || first.Status != second.Status {
		t.Errorf("a retry changed the result: %d/%s then %d/%s",
			first.Points, first.Status, second.Points, second.Status)
	}
	if second.GradedAt == nil || !second.GradedAt.Equal(*first.GradedAt) {
		t.Error("a retry regraded an attempt that was already graded")
	}
}

// An unanswered question is graded, not skipped, and the quiz is scored out of
// all of its points.
func TestAnUnansweredQuestionCosts(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	// An 80% bar. Answering the 3-point question and abandoning the 2-point one
	// scores 3 of 5, which is 60% — a pass if the quiz were scored out of what was
	// attempted, and a failure because it is not.
	_, choiceID, _ := f.quiz(t, assess.NewQuiz{PassingPercent: 80})

	if _, err := f.svc.StartAttempt(t.Context(), f.tenant, f.lesson, f.learner, assess.Author{UserID: f.learner}); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := f.svc.SaveAnswer(t.Context(), f.tenant, f.lesson, f.learner, choiceID,
		assess.Response{Choices: []uuid.UUID{f.correctOption(t, choiceID)}}); err != nil {
		t.Fatalf("save: %v", err)
	}

	submitted, _ := f.svc.SubmitAttempt(t.Context(), f.tenant, f.lesson, f.learner, assess.Author{UserID: f.learner})
	graded, err := f.svc.GradeAttempt(t.Context(), f.tenant, submitted.ID)
	if err != nil {
		t.Fatalf("grade: %v", err)
	}

	if graded.Points != 3 || graded.MaxPoints != 5 {
		t.Errorf("scored %d/%d, want 3/5 — the untouched question still counts", graded.Points, graded.MaxPoints)
	}
	if graded.Passed == nil {
		t.Fatal("passed is nil; nothing here needs a person")
	}
	if *graded.Passed {
		t.Error("3 of 5 passed an 80% bar")
	}

	// The review names the question they never reached, rather than leaving a hole.
	review, err := f.svc.Review(t.Context(), f.tenant, f.lesson, f.learner, 1)
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if len(review.Items) != 2 {
		t.Fatalf("review dropped the unanswered question: %d items", len(review.Items))
	}
}

// An essay holds the attempt out of a final grade. There is a score, and no pass.
func TestAnEssayLeavesTheAttemptAwaitingReview(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	_, choiceID, _ := f.quiz(t, assess.NewQuiz{PassingPercent: 60})

	if _, err := f.svc.AddQuestion(t.Context(), f.tenant, f.lesson, assess.NewQuestion{
		Type: assess.TypeOpenEnded, Prompt: "Discuss.", Points: 5,
	}, f.author); err != nil {
		t.Fatalf("add essay: %v", err)
	}

	if _, err := f.svc.StartAttempt(t.Context(), f.tenant, f.lesson, f.learner, assess.Author{UserID: f.learner}); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := f.svc.SaveAnswer(t.Context(), f.tenant, f.lesson, f.learner, choiceID,
		assess.Response{Choices: []uuid.UUID{f.correctOption(t, choiceID)}}); err != nil {
		t.Fatalf("save: %v", err)
	}

	submitted, _ := f.svc.SubmitAttempt(t.Context(), f.tenant, f.lesson, f.learner, assess.Author{UserID: f.learner})
	graded, err := f.svc.GradeAttempt(t.Context(), f.tenant, submitted.ID)
	if err != nil {
		t.Fatalf("grade: %v", err)
	}

	if graded.Status != assess.StatusAwaitingReview {
		t.Errorf("status = %q, want %q", graded.Status, assess.StatusAwaitingReview)
	}
	if graded.Passed != nil {
		t.Errorf("passed = %v; a pass is not a thing to guess at while an essay is unmarked", *graded.Passed)
	}
	if graded.Points != 3 || graded.MaxPoints != 10 {
		t.Errorf("scored %d/%d, want 3/10", graded.Points, graded.MaxPoints)
	}
}

// Starting twice is starting once. A learner who reloads the page has not burned
// an attempt.
func TestStartingIsIdempotent(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.quiz(t, assess.NewQuiz{MaxAttempts: 1})

	first, err := f.svc.StartAttempt(t.Context(), f.tenant, f.lesson, f.learner, assess.Author{UserID: f.learner})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	second, err := f.svc.StartAttempt(t.Context(), f.tenant, f.lesson, f.learner, assess.Author{UserID: f.learner})
	if err != nil {
		t.Fatalf("start again: %v", err)
	}

	if first.ID != second.ID {
		t.Fatalf("two Starts produced two attempts, %s and %s", first.ID, second.ID)
	}
}

// The bound on attempts is real, and it counts the ones already spent.
func TestAttemptsRunOut(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.quiz(t, assess.NewQuiz{MaxAttempts: 1})

	if _, err := f.svc.StartAttempt(t.Context(), f.tenant, f.lesson, f.learner, assess.Author{UserID: f.learner}); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := f.svc.SubmitAttempt(t.Context(), f.tenant, f.lesson, f.learner, assess.Author{UserID: f.learner}); err != nil {
		t.Fatalf("submit: %v", err)
	}

	_, err := f.svc.StartAttempt(t.Context(), f.tenant, f.lesson, f.learner, assess.Author{UserID: f.learner})
	if !errors.Is(err, assess.ErrAttemptsExhausted) {
		t.Fatalf("a second start on a one-attempt quiz: %v, want ErrAttemptsExhausted", err)
	}
}

// A submitted attempt is not a draft. Answering one is refused, and submitting it
// twice does not queue two grading jobs.
func TestASubmittedAttemptIsClosed(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	_, choiceID, _ := f.quiz(t, assess.NewQuiz{})

	if _, err := f.svc.StartAttempt(t.Context(), f.tenant, f.lesson, f.learner, assess.Author{UserID: f.learner}); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := f.svc.SubmitAttempt(t.Context(), f.tenant, f.lesson, f.learner, assess.Author{UserID: f.learner}); err != nil {
		t.Fatalf("submit: %v", err)
	}

	// A double-clicked button. There is no live attempt any more.
	if _, err := f.svc.SubmitAttempt(t.Context(), f.tenant, f.lesson, f.learner, assess.Author{UserID: f.learner}); !errors.Is(err, assess.ErrNotFound) {
		t.Errorf("a second submit: %v, want ErrNotFound", err)
	}
	if queued := f.jobs.queued(); len(queued) != 1 {
		t.Errorf("queued %d grading jobs for one submission", len(queued))
	}

	// And a late answer changes nothing.
	err := f.svc.SaveAnswer(t.Context(), f.tenant, f.lesson, f.learner, choiceID,
		assess.Response{Choices: []uuid.UUID{f.correctOption(t, choiceID)}})
	if !errors.Is(err, assess.ErrNotFound) {
		t.Errorf("answering a submitted attempt: %v, want ErrNotFound", err)
	}
}

// One learner's attempt is not another's. The attempt is addressed by (quiz,
// learner, number), so there is no id to guess.
func TestALearnerReachesOnlyTheirOwnAttempt(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.quiz(t, assess.NewQuiz{})

	stranger := seedUser(t, f.db, f.tenant)

	if _, err := f.svc.StartAttempt(t.Context(), f.tenant, f.lesson, f.learner, assess.Author{UserID: f.learner}); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := f.svc.SubmitAttempt(t.Context(), f.tenant, f.lesson, f.learner, assess.Author{UserID: f.learner}); err != nil {
		t.Fatalf("submit: %v", err)
	}

	if _, err := f.svc.Review(t.Context(), f.tenant, f.lesson, stranger, 1); !errors.Is(err, assess.ErrNotFound) {
		t.Errorf("a stranger read attempt 1: %v, want ErrNotFound", err)
	}
}

// A learner is never handed the answer, and never the explanation before the
// attempt has been graded.
func TestTheLearnerViewWithholdsTheAnswers(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.quiz(t, assess.NewQuiz{})

	view, attempts, err := f.svc.LearnerView(t.Context(), f.tenant, f.lesson, f.learner)
	if err != nil {
		t.Fatalf("learner view: %v", err)
	}
	if len(attempts) != 0 {
		t.Errorf("a learner who has not started has %d attempts", len(attempts))
	}
	if len(view.Questions) != 2 || view.TotalPoints != 5 {
		t.Fatalf("view has %d questions worth %d", len(view.Questions), view.TotalPoints)
	}

	// The explanation is withheld while the attempt is still open.
	if _, err := f.svc.StartAttempt(t.Context(), f.tenant, f.lesson, f.learner, assess.Author{UserID: f.learner}); err != nil {
		t.Fatalf("start: %v", err)
	}

	review, err := f.svc.Review(t.Context(), f.tenant, f.lesson, f.learner, 1)
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	for _, item := range review.Items {
		if item.Explanation != "" {
			t.Errorf("an in-progress attempt leaked the explanation of %q: %q", item.Prompt, item.Explanation)
		}
	}
}

// A lesson has one quiz. The unique index says so, under concurrency, rather than
// a check somebody raced.
func TestALessonHasOneQuiz(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.quiz(t, assess.NewQuiz{})

	_, err := f.svc.CreateQuiz(t.Context(), f.tenant, f.lesson, assess.NewQuiz{Title: "Another"}, f.author)
	if !errors.Is(err, assess.ErrQuizExists) {
		t.Fatalf("a second quiz on one lesson: %v, want ErrQuizExists", err)
	}
}

// An empty quiz cannot be attempted: it would be graded out of zero, and everyone
// would pass it.
func TestAnEmptyQuizCannotBeAttempted(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	if _, err := f.svc.CreateQuiz(t.Context(), f.tenant, f.lesson, assess.NewQuiz{Title: "Empty"}, f.author); err != nil {
		t.Fatalf("create quiz: %v", err)
	}

	_, err := f.svc.StartAttempt(t.Context(), f.tenant, f.lesson, f.learner, assess.Author{UserID: f.learner})
	if !errors.Is(err, assess.ErrEmptyQuiz) {
		t.Fatalf("starting an empty quiz: %v, want ErrEmptyQuiz", err)
	}
}

// The job and the submission commit together. If the queue refuses, the attempt
// is not closed — because an attempt in `grading` with no job would stay there
// for ever.
func TestAFailedEnqueueRollsBackTheSubmission(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	jobs := &recordingEnqueuer{err: errors.New("the queue is down")}
	tenantID := seedTenant(t, db)

	f := fixture{
		db: db, svc: newService(db, jobs), jobs: jobs,
		tenant: tenantID, lesson: seedLesson(t, db, tenantID),
		learner: seedUser(t, db, tenantID),
		author:  assess.Author{UserID: seedUser(t, db, tenantID)},
	}
	f.quiz(t, assess.NewQuiz{})

	if _, err := f.svc.StartAttempt(t.Context(), f.tenant, f.lesson, f.learner, assess.Author{UserID: f.learner}); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := f.svc.SubmitAttempt(t.Context(), f.tenant, f.lesson, f.learner, assess.Author{UserID: f.learner}); err == nil {
		t.Fatal("submit succeeded though the queue refused the job")
	}

	// The attempt is still open, so the learner can submit again once the queue is
	// back. Had the transaction committed, it would sit in `grading` for ever.
	view, attempts, err := f.svc.LearnerView(t.Context(), f.tenant, f.lesson, f.learner)
	if err != nil {
		t.Fatalf("learner view: %v", err)
	}
	_ = view
	if len(attempts) != 1 || attempts[0].Status != assess.StatusInProgress {
		t.Fatalf("after a failed enqueue the attempt is %+v, want one still in progress", attempts)
	}
}

// Loading a quiz costs a fixed number of queries, whatever it holds. If someone
// replaces the batched option fetch with a loop over questions, this fails at the
// size where it is cheap to notice.
func TestLoadingAQuizHasNoNPlusOne(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	if _, err := f.svc.CreateQuiz(t.Context(), f.tenant, f.lesson, assess.NewQuiz{Title: "Big"}, f.author); err != nil {
		t.Fatalf("create quiz: %v", err)
	}

	for size := range 3 {
		questions := 2 + size*8
		for range questions {
			if _, err := f.svc.AddQuestion(t.Context(), f.tenant, f.lesson, assess.NewQuestion{
				Type: assess.TypeSingleChoice, Prompt: "Which?", Points: 1,
				Options: []assess.NewOption{{Content: "A", IsCorrect: true}, {Content: "B"}, {Content: "C"}},
			}, f.author); err != nil {
				t.Fatalf("add question: %v", err)
			}
		}

		counter := &database.Counter{}
		ctx := database.WithCounter(t.Context(), counter)

		view, _, err := f.svc.LearnerView(ctx, f.tenant, f.lesson, uuid.Nil)
		if err != nil {
			t.Fatalf("learner view: %v", err)
		}
		if len(view.Questions) == 0 {
			t.Fatal("the fixture loaded nothing, so the query count means nothing")
		}

		// quiz, questions, options. The attempt list is skipped for uuid.Nil.
		const want = 3
		if got := counter.Count(); got != want {
			t.Fatalf("a quiz of %d questions issued %d queries, want %d — "+
				"the count must not grow with the quiz", len(view.Questions), got, want)
		}
	}
}
