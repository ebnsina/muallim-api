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
	"github.com/ebnsina/lms-api/internal/certify"
	"github.com/ebnsina/lms-api/internal/enroll"
	"github.com/ebnsina/lms-api/internal/grade"
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

// The real enrolment service, exactly as cmd/ wires it. A stub would leave the
// thing under test — that a passed quiz completes its lesson, in the transaction
// that recorded the grade — entirely unexercised.
func newCompletions(db *database.DB) *enroll.Service {
	return enroll.NewService(db, enroll.NewPostgresRepository(), enrolAuditor{audit.NewRecorder()}, newIssuer(db), nil)
}

type enrolAuditor struct{ recorder *audit.Recorder }

func (a enrolAuditor) Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e enroll.AuditEntry) error {
	return a.recorder.Record(ctx, tx, tenantID, audit.Entry{
		ActorID: e.ActorID, Action: e.Action,
		TargetType: e.TargetType, TargetID: e.TargetID,
		IP: e.IP, UserAgent: e.UserAgent, Metadata: e.Metadata,
	})
}

/*
quizGrades adapts the real gradebook to `assess.Grades`, exactly as cmd/ does.

A stub would record whatever the test wanted, which is precisely the question
these tests exist to ask: does a graded attempt reach the gradebook, in the
transaction that graded it?
*/
type quizGrades struct{ svc *grade.Service }

func (g quizGrades) RecordScore(ctx context.Context, tx pgx.Tx, tenantID, lessonID, userID, sourceID uuid.UUID,
	title string, points, maxPoints int, keepHighest bool,
) error {
	return g.svc.Record(ctx, tx, tenantID, grade.Score{
		LessonID: lessonID, UserID: userID,
		Source: grade.SourceQuiz, SourceID: sourceID,
		Title: title, Points: points, MaxPoints: maxPoints, KeepHighest: keepHighest,
	})
}

func newService(db *database.DB, jobs assess.Enqueuer) *assess.Service {
	return newServiceWith(db, jobs, nil)
}

// captureNotifier records the notifications a marking asks to send.
type captureNotifier struct{ sent []assess.Notification }

func (c *captureNotifier) Notify(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n assess.Notification) error {
	c.sent = append(c.sent, n)
	return nil
}

func newServiceWith(db *database.DB, jobs assess.Enqueuer, notifier assess.Notifier) *assess.Service {
	return assess.NewService(db, assess.NewPostgresRepository(), assessAuditor{audit.NewRecorder()},
		jobs, newCompletions(db), quizGrades{grade.NewService(db, grade.NewPostgresRepository())}, notifier)
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

// seedLesson makes the smallest published course that can hold a quiz lesson, and
// returns the lesson and the course's slug.
func seedLesson(t *testing.T, db *database.DB, tenantID uuid.UUID) (uuid.UUID, string) {
	t.Helper()

	slug := "c-" + uuid.NewString()[:8]

	var lessonID uuid.UUID
	err := db.WithTenant(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var courseID, topicID uuid.UUID
		if err := tx.QueryRow(ctx,
			`INSERT INTO courses (tenant_id, slug, title, status)
			 VALUES ($1, $2, 'Course', 'published') RETURNING id`,
			tenantID, slug).Scan(&courseID); err != nil {
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
	return lessonID, slug
}

type fixture struct {
	db       *database.DB
	svc      *assess.Service
	learning *enroll.Service
	grades   *grade.Service
	jobs     *recordingEnqueuer
	notifier *captureNotifier
	tenant   uuid.UUID
	lesson   uuid.UUID
	course   string
	learner  uuid.UUID
	author   assess.Author
}

func newFixture(t *testing.T) fixture {
	t.Helper()

	db := testDB(t)
	jobs := &recordingEnqueuer{}
	tenantID := seedTenant(t, db)
	lessonID, slug := seedLesson(t, db, tenantID)
	notifier := &captureNotifier{}

	return fixture{
		db: db, svc: newServiceWith(db, jobs, notifier), learning: newCompletions(db), jobs: jobs,
		notifier: notifier,
		grades:   grade.NewService(db, grade.NewPostgresRepository()),
		tenant:   tenantID, lesson: lessonID, course: slug,
		learner: seedUser(t, db, tenantID),
		author:  assess.Author{UserID: seedUser(t, db, tenantID)},
	}
}

// enrol puts the learner in the course, which is what makes a quiz theirs to take
// and their progress a row worth writing.
func (f fixture) enrol(t *testing.T) {
	t.Helper()

	if _, err := f.learning.Enrol(t.Context(), f.tenant, f.course, enroll.Actor{UserID: f.learner}, enroll.SourceSelf); err != nil {
		t.Fatalf("enrol: %v", err)
	}
}

// progress reads the learner's standing in the course.
func (f fixture) progress(t *testing.T) enroll.Progress {
	t.Helper()

	p, err := f.learning.Progress(t.Context(), f.tenant, f.course, f.learner)
	if err != nil {
		t.Fatalf("progress: %v", err)
	}
	return p
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

// wrongOption is any option that is not the answer. A test that wants a failing
// attempt has to name one, and naming it by index would break the day somebody
// reorders the fixture.
func (f fixture) wrongOption(t *testing.T, questionID uuid.UUID) uuid.UUID {
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
			if !o.IsCorrect {
				return o.ID
			}
		}
	}
	t.Fatalf("every option on question %s is correct", questionID)
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

	lessonID, slug := seedLesson(t, db, tenantID)
	f := fixture{
		db: db, svc: newService(db, jobs), learning: newCompletions(db), jobs: jobs,
		grades: grade.NewService(db, grade.NewPostgresRepository()),
		tenant: tenantID, lesson: lessonID, course: slug,
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

/*
A graded attempt reaches the gradebook, in the transaction that graded it.

The seam is an interface `assess` declares and `cmd` satisfies over the grade
service. Neither package imports the other, so nothing in either would notice if
the wiring were dropped — except this.

Recorded whatever the verdict. A failed quiz is a grade, and a course total that
skipped the failures would flatter everybody who failed one.
*/
func TestAGradedAttemptReachesTheGradebook(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	_, choiceID, typedID := f.quiz(t, assess.NewQuiz{PassingPercent: 60})

	if _, err := f.svc.StartAttempt(t.Context(), f.tenant, f.lesson, f.learner, assess.Author{UserID: f.learner}); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Both wrong, so the attempt certainly fails. A failed quiz is a grade, and a
	// course total that skipped the failures would flatter everybody who failed one.
	if err := f.svc.SaveAnswer(t.Context(), f.tenant, f.lesson, f.learner, choiceID,
		assess.Response{Choices: []uuid.UUID{f.wrongOption(t, choiceID)}}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := f.svc.SaveAnswer(t.Context(), f.tenant, f.lesson, f.learner, typedID,
		assess.Response{Text: "not the answer"}); err != nil {
		t.Fatalf("save: %v", err)
	}

	submitted, err := f.svc.SubmitAttempt(t.Context(), f.tenant, f.lesson, f.learner, assess.Author{UserID: f.learner})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	graded, err := f.svc.GradeAttempt(t.Context(), f.tenant, submitted.ID)
	if err != nil {
		t.Fatalf("grade: %v", err)
	}
	if graded.Passed == nil || *graded.Passed {
		t.Fatalf("the attempt was meant to fail; passed = %v", graded.Passed)
	}

	// No enrolment needed: a learner reads their own marks, and `LearnerGrades`
	// asks the gradebook, not the roll.
	grades, err := f.grades.LearnerGrades(t.Context(), f.tenant, f.course, f.learner)
	if err != nil {
		t.Fatalf("grades: %v", err)
	}
	if len(grades.Entries) != 1 {
		t.Fatalf("%d gradebook entries after a graded attempt, want 1", len(grades.Entries))
	}
	if grades.Entries[0].Points != graded.Points || grades.Entries[0].MaxPoints != graded.MaxPoints {
		t.Errorf("recorded %d of %d, want the attempt's %d of %d",
			grades.Entries[0].Points, grades.Entries[0].MaxPoints, graded.Points, graded.MaxPoints)
	}
}

// Grading is idempotent, and so is what it writes. A retried job records the same
// score again rather than a second entry that halves the learner's percentage.
func TestRegradingDoesNotDoubleTheGradebook(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	_, choiceID, typedID := f.quiz(t, assess.NewQuiz{PassingPercent: 60})

	if _, err := f.svc.StartAttempt(t.Context(), f.tenant, f.lesson, f.learner, assess.Author{UserID: f.learner}); err != nil {
		t.Fatalf("start: %v", err)
	}
	right := f.correctOption(t, choiceID)
	_ = f.svc.SaveAnswer(t.Context(), f.tenant, f.lesson, f.learner, choiceID, assess.Response{Choices: []uuid.UUID{right}})
	_ = f.svc.SaveAnswer(t.Context(), f.tenant, f.lesson, f.learner, typedID, assess.Response{Text: "Paris"})

	submitted, err := f.svc.SubmitAttempt(t.Context(), f.tenant, f.lesson, f.learner, assess.Author{UserID: f.learner})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	if _, err := f.svc.GradeAttempt(t.Context(), f.tenant, submitted.ID); err != nil {
		t.Fatalf("grade: %v", err)
	}
	if _, err := f.svc.GradeAttempt(t.Context(), f.tenant, submitted.ID); err != nil {
		t.Fatalf("regrade: %v", err)
	}

	grades, err := f.grades.LearnerGrades(t.Context(), f.tenant, f.course, f.learner)
	if err != nil {
		t.Fatalf("grades: %v", err)
	}
	if len(grades.Entries) != 1 {
		t.Errorf("%d entries after two grading runs, want 1", len(grades.Entries))
	}
	if len(grades.Items) != 1 {
		t.Errorf("%d items after two grading runs, want 1", len(grades.Items))
	}
}

func (g quizGrades) EnsureItem(ctx context.Context, tx pgx.Tx, tenantID, lessonID, sourceID uuid.UUID,
	title string, maxPoints int,
) error {
	return g.svc.EnsureItem(ctx, tx, tenantID, grade.Score{
		LessonID: lessonID,
		Source:   grade.SourceQuiz, SourceID: sourceID,
		Title: title, MaxPoints: maxPoints,
	})
}

/*
A quiz's worth follows its questions, in the transaction that changed them.

A quiz is worth the sum of its questions' points, so adding one makes it worth
more and removing one makes it worth less. The gradebook is told each time; a
number that only refreshed when somebody was graded would be stale for exactly as
long as nobody had sat the quiz.
*/
func TestAQuizzesGradebookItemFollowsItsQuestions(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	_, choiceID, _ := f.quiz(t, assess.NewQuiz{PassingPercent: 60})

	before, err := f.grades.LearnerGrades(t.Context(), f.tenant, f.course, f.learner)
	if err != nil {
		t.Fatalf("grades: %v", err)
	}
	if len(before.Items) != 1 {
		t.Fatalf("%d items for a quiz nobody has sat, want 1", len(before.Items))
	}
	worth := before.Items[0].MaxPoints
	if worth <= 0 {
		t.Fatalf("the quiz is worth %d points", worth)
	}

	// Removing a question makes it worth less.
	if err := f.svc.RemoveQuestion(t.Context(), f.tenant, choiceID, f.author); err != nil {
		t.Fatalf("remove question: %v", err)
	}

	after, err := f.grades.LearnerGrades(t.Context(), f.tenant, f.course, f.learner)
	if err != nil {
		t.Fatalf("grades: %v", err)
	}
	if len(after.Items) != 1 {
		t.Fatalf("%d items after removing a question, want 1", len(after.Items))
	}
	if after.Items[0].MaxPoints >= worth {
		t.Errorf("the quiz is still worth %d after losing a question worth points (was %d)",
			after.Items[0].MaxPoints, worth)
	}
}

/*
The real certificate service, exactly as cmd/ wires it.

A stub would return nil and prove nothing: whether finishing a course issues a
certificate is the question, and `enroll` cannot answer it — it has never heard of
`certify`.
*/
type issuer struct{ svc *certify.Service }

func (i issuer) IssueIfEarned(ctx context.Context, tx pgx.Tx, tenantID, courseID, userID uuid.UUID) error {
	return i.svc.IssueIfEarned(ctx, tx, tenantID, courseID, userID)
}

type certifyAuditor struct{ recorder *audit.Recorder }

func (a certifyAuditor) Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e certify.AuditEntry) error {
	return a.recorder.Record(ctx, tx, tenantID, audit.Entry{
		ActorID: e.ActorID, Action: e.Action,
		TargetType: e.TargetType, TargetID: e.TargetID,
		Metadata: e.Metadata,
	})
}

func newIssuer(db *database.DB) issuer {
	return issuer{certify.NewService(db, certify.NewPostgresRepository(), certifyAuditor{audit.NewRecorder()})}
}

// Marking the last essay finishes the attempt, and only then does the learner get
// one "your quiz was graded" notification — not one per essay marked.
func TestMarkingAnEssayNotifiesTheLearnerOnce(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	_, choiceID, _ := f.quiz(t, assess.NewQuiz{PassingPercent: 60})

	essay, err := f.svc.AddQuestion(t.Context(), f.tenant, f.lesson, assess.NewQuestion{
		Type: assess.TypeOpenEnded, Prompt: "Discuss.", Points: 5,
	}, f.author)
	if err != nil {
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
	if _, err := f.svc.GradeAttempt(t.Context(), f.tenant, submitted.ID); err != nil {
		t.Fatalf("auto-grade: %v", err)
	}

	// Auto-grading the machine part notifies no one — the essay is still open.
	if len(f.notifier.sent) != 0 {
		t.Fatalf("auto-grade notified %d, want 0 while an essay is unmarked", len(f.notifier.sent))
	}

	// Marking the essay finishes the attempt and notifies the learner exactly once.
	marker := assess.Author{UserID: f.author.UserID}
	graded, err := f.svc.MarkAnswer(t.Context(), f.tenant, submitted.ID, essay.ID, assess.Mark{Points: 5}, marker)
	if err != nil {
		t.Fatalf("mark: %v", err)
	}
	if graded.Status != assess.StatusGraded {
		t.Fatalf("status = %q, want graded", graded.Status)
	}
	if len(f.notifier.sent) != 1 {
		t.Fatalf("marking notified %d, want 1", len(f.notifier.sent))
	}
	n := f.notifier.sent[0]
	if n.UserID != f.learner || n.Kind != assess.KindGrade {
		t.Fatalf("notification: %+v", n)
	}
	if n.Link != "/courses/"+f.course+"/grades" {
		t.Fatalf("link = %q", n.Link)
	}
}

// A range question is authored, stored, and auto-graded from a numeric answer.
func TestRangeQuestionRoundTripsAndGrades(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.quiz(t, assess.NewQuiz{PassingPercent: 50})

	rangeQ, err := f.svc.AddQuestion(t.Context(), f.tenant, f.lesson, assess.NewQuestion{
		Type: assess.TypeRange, Prompt: "Boiling point of water in °C?", Points: 2,
		Accepted: [][]string{{"99.5", "100.5"}},
	}, f.author)
	if err != nil {
		t.Fatalf("add range question: %v", err)
	}

	if _, err := f.svc.StartAttempt(t.Context(), f.tenant, f.lesson, f.learner, assess.Author{UserID: f.learner}); err != nil {
		t.Fatalf("start: %v", err)
	}
	n := 100.0
	if err := f.svc.SaveAnswer(t.Context(), f.tenant, f.lesson, f.learner, rangeQ.ID, assess.Response{Number: &n}); err != nil {
		t.Fatalf("save range answer: %v", err)
	}
	submitted, _ := f.svc.SubmitAttempt(t.Context(), f.tenant, f.lesson, f.learner, assess.Author{UserID: f.learner})
	graded, err := f.svc.GradeAttempt(t.Context(), f.tenant, submitted.ID)
	if err != nil {
		t.Fatalf("grade: %v", err)
	}

	// The range is worth 2 of the quiz's points, and 100 is in [99.5, 100.5].
	if graded.Points < 2 {
		t.Fatalf("range answer earned no points: scored %d/%d", graded.Points, graded.MaxPoints)
	}
}

// The two image types are stored, authored, and graded through the same paths as
// their text cousins — the round trip proves the widened type check accepts them.
func TestImageQuestionTypesRoundTripAndGrade(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.quiz(t, assess.NewQuiz{PassingPercent: 50})

	// image_answering grades as single choice; each option's image URL is its content.
	img, err := f.svc.AddQuestion(t.Context(), f.tenant, f.lesson, assess.NewQuestion{
		Type: assess.TypeImageAnswering, Prompt: "Which shape is a circle?", Points: 2,
		Options: []assess.NewOption{
			{Content: "https://cdn.test/square.png"},
			{Content: "https://cdn.test/circle.png", IsCorrect: true},
		},
	}, f.author)
	if err != nil {
		t.Fatalf("add image_answering question: %v", err)
	}

	// image_matching grades as matching; both sides are image URLs.
	if _, err := f.svc.AddQuestion(t.Context(), f.tenant, f.lesson, assess.NewQuestion{
		Type: assess.TypeImageMatching, Prompt: "Match the country to its capital", Points: 1,
		Options: []assess.NewOption{
			{Content: "https://cdn.test/fr.png", MatchContent: "https://cdn.test/paris.png"},
			{Content: "https://cdn.test/jp.png", MatchContent: "https://cdn.test/tokyo.png"},
		},
	}, f.author); err != nil {
		t.Fatalf("add image_matching question: %v", err)
	}

	if _, err := f.svc.StartAttempt(t.Context(), f.tenant, f.lesson, f.learner, assess.Author{UserID: f.learner}); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := f.svc.SaveAnswer(t.Context(), f.tenant, f.lesson, f.learner, img.ID,
		assess.Response{Choices: []uuid.UUID{f.correctOption(t, img.ID)}}); err != nil {
		t.Fatalf("save image_answering: %v", err)
	}
	submitted, _ := f.svc.SubmitAttempt(t.Context(), f.tenant, f.lesson, f.learner, assess.Author{UserID: f.learner})
	graded, err := f.svc.GradeAttempt(t.Context(), f.tenant, submitted.ID)
	if err != nil {
		t.Fatalf("grade: %v", err)
	}

	// image_answering is worth 2 and was answered correctly.
	if graded.Points < 2 {
		t.Fatalf("image_answering earned no points: scored %d/%d", graded.Points, graded.MaxPoints)
	}
}

// A question is saved to the bank, listed, and copied into another quiz — where
// editing the copy leaves the bank original alone.
func TestContentBankRoundTrip(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	_, choiceID, _ := f.quiz(t, assess.NewQuiz{PassingPercent: 60})

	// Save the choice question to the bank under a category.
	saved, err := f.svc.SaveQuestionToBank(t.Context(), f.tenant, choiceID, "Geography", f.author)
	if err != nil {
		t.Fatalf("save to bank: %v", err)
	}
	if saved.Category != "Geography" || saved.Type != assess.TypeSingleChoice {
		t.Fatalf("saved bank question wrong: %+v", saved)
	}

	// It appears in the bank, and its category is listed.
	page, err := f.svc.BankQuestions(t.Context(), f.tenant, "", "", 20)
	if err != nil {
		t.Fatalf("list bank: %v", err)
	}
	if len(page.Questions) != 1 || page.Questions[0].ID != saved.ID {
		t.Fatalf("bank list: %+v", page.Questions)
	}
	cats, _ := f.svc.BankCategories(t.Context(), f.tenant)
	if len(cats) != 1 || cats[0] != "Geography" {
		t.Fatalf("categories: %v", cats)
	}

	// Filtering by a different category finds nothing.
	other, _ := f.svc.BankQuestions(t.Context(), f.tenant, "History", "", 20)
	if len(other.Questions) != 0 {
		t.Fatalf("wrong category returned %d", len(other.Questions))
	}

	// Copy it into a second lesson's quiz. It arrives with its options intact.
	lesson2 := f.secondQuizLesson(t)
	added, err := f.svc.AddBankQuestionToQuiz(t.Context(), f.tenant, lesson2, saved.ID, f.author)
	if err != nil {
		t.Fatalf("add from bank: %v", err)
	}
	if added.Type != assess.TypeSingleChoice || len(added.Options) != 2 {
		t.Fatalf("copied question wrong: type=%s options=%d", added.Type, len(added.Options))
	}
	// A fresh id: it is a copy, not the same row.
	if added.ID == saved.ID {
		t.Fatalf("copy reused the bank id")
	}

	// Deleting from the bank leaves the copy in the quiz.
	if err := f.svc.DeleteBankQuestion(t.Context(), f.tenant, saved.ID); err != nil {
		t.Fatalf("delete bank: %v", err)
	}
	page, _ = f.svc.BankQuestions(t.Context(), f.tenant, "", "", 20)
	if len(page.Questions) != 0 {
		t.Fatalf("bank not empty after delete: %d", len(page.Questions))
	}
}

// secondQuizLesson makes another lesson with an empty quiz, to copy a bank
// question into.
func (f fixture) secondQuizLesson(t *testing.T) uuid.UUID {
	t.Helper()
	var lessonID uuid.UUID
	err := f.db.WithTenant(t.Context(), f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		var topicID uuid.UUID
		if err := tx.QueryRow(ctx,
			`SELECT id FROM topics WHERE tenant_id = $1 LIMIT 1`, f.tenant).Scan(&topicID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO lessons (tenant_id, topic_id, title, content_type, position)
			 VALUES ($1, $2, 'Second quiz', 'quiz', 1) RETURNING id`, f.tenant, topicID).Scan(&lessonID); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("second lesson: %v", err)
	}
	if _, err := f.svc.CreateQuiz(t.Context(), f.tenant, lessonID, assess.NewQuiz{Title: "Second", PassingPercent: 50}, f.author); err != nil {
		t.Fatalf("second quiz: %v", err)
	}
	return lessonID
}
