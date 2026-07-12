package grade_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/grade"
	"github.com/ebnsina/muallim-api/internal/platform/database"
)

func testDB(t *testing.T) *database.DB {
	t.Helper()

	url := os.Getenv("MUALLIM_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("MUALLIM_TEST_DATABASE_URL is not set; skipping database tests")
	}

	log := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	db, err := database.New(t.Context(), database.Options{URL: url, MaxConns: 4}, log)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(db.Close)
	return db
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

func seedUser(t *testing.T, db *database.DB, tenantID uuid.UUID, name string) uuid.UUID {
	t.Helper()

	id := uuid.New()
	err := db.WithTenant(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO users (id, email, password_hash, name) VALUES ($1, $2, 'x', $3)`,
			id, "u-"+id.String()[:8]+"@example.test", name); err != nil {
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

type fixture struct {
	db     *database.DB
	svc    *grade.Service
	tenant uuid.UUID
	slug   string

	// Two lessons, so a course can hold two assessments.
	lessons []uuid.UUID
}

func newFixture(t *testing.T, lessons int) fixture {
	t.Helper()

	db := testDB(t)
	tenantID := seedTenant(t, db)
	slug := "c-" + uuid.NewString()[:8]

	ids := make([]uuid.UUID, 0, lessons)
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
		for i := range lessons {
			var lessonID uuid.UUID
			if err := tx.QueryRow(ctx,
				`INSERT INTO lessons (tenant_id, topic_id, title, content_type, position)
				 VALUES ($1, $2, $3, 'quiz', $4) RETURNING id`,
				tenantID, topicID, fmt.Sprintf("Lesson %d", i+1), i).Scan(&lessonID); err != nil {
				return err
			}
			ids = append(ids, lessonID)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed course: %v", err)
	}

	return fixture{
		db:      db,
		svc:     grade.NewService(db, grade.NewPostgresRepository()),
		tenant:  tenantID,
		slug:    slug,
		lessons: ids,
	}
}

// enrol puts a learner on the course, because a gradebook lists who is enrolled.
func (f fixture) enrol(t *testing.T, userID uuid.UUID) {
	t.Helper()

	err := f.db.WithTenant(t.Context(), f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO enrolments (tenant_id, course_id, user_id)
			 SELECT $1, id, $3 FROM courses WHERE tenant_id = $1 AND slug = $2`,
			f.tenant, f.slug, userID)
		return err
	})
	if err != nil {
		t.Fatalf("enrol: %v", err)
	}
}

// record is what `assess` and `assign` do, through an interface they declare.
func (f fixture) record(t *testing.T, score grade.Score) {
	t.Helper()

	err := f.db.WithTenant(t.Context(), f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		return f.svc.Record(ctx, tx, f.tenant, score)
	})
	if err != nil {
		t.Fatalf("record %s: %v", score.Title, err)
	}
}

func quizScore(lesson, user, quiz uuid.UUID, points, max int) grade.Score {
	return grade.Score{
		LessonID: lesson, UserID: user,
		Source: grade.SourceQuiz, SourceID: quiz,
		Title: "Chapter one", Points: points, MaxPoints: max,
	}
}

// A mark reaches the gradebook, and reaches the learner who earned it.
func TestAMarkIsRecordedAndReadBack(t *testing.T) {
	t.Parallel()

	f := newFixture(t, 1)
	learner := seedUser(t, f.db, f.tenant, "Al-Khwarizmi")
	f.enrol(t, learner)

	f.record(t, quizScore(f.lessons[0], learner, uuid.New(), 9, 10))

	grades, err := f.svc.LearnerGrades(t.Context(), f.tenant, f.slug, learner)
	if err != nil {
		t.Fatalf("learner grades: %v", err)
	}

	if len(grades.Items) != 1 || len(grades.Entries) != 1 {
		t.Fatalf("%d items and %d entries, want 1 and 1", len(grades.Items), len(grades.Entries))
	}
	if grades.Result.Percent != 90 || grades.Result.Band.Label != "A" {
		t.Errorf("graded %d%% (%q), want 90%% (A)", grades.Result.Percent, grades.Result.Band.Label)
	}
	if !grades.Scale.Builtin {
		t.Error("a course with no scale of its own did not grade by the built-in default")
	}
}

/*
Recording the same score twice is recording it once.

The grading job is retried, and `assess` calls this from inside it. A second entry
would double a learner's marks and halve their percentage — silently, and only for
the learners whose job happened to retry.
*/
func TestRecordingTwiceIsRecordingOnce(t *testing.T) {
	t.Parallel()

	f := newFixture(t, 1)
	learner := seedUser(t, f.db, f.tenant, "Ibn Sina")
	f.enrol(t, learner)

	quiz := uuid.New()
	f.record(t, quizScore(f.lessons[0], learner, quiz, 5, 10))
	f.record(t, quizScore(f.lessons[0], learner, quiz, 5, 10))

	grades, err := f.svc.LearnerGrades(t.Context(), f.tenant, f.slug, learner)
	if err != nil {
		t.Fatalf("learner grades: %v", err)
	}

	if len(grades.Items) != 1 {
		t.Errorf("%d items, want 1", len(grades.Items))
	}
	if len(grades.Entries) != 1 {
		t.Errorf("%d entries, want 1", len(grades.Entries))
	}
	if grades.Result.Percent != 50 {
		t.Errorf("percent = %d, want 50", grades.Result.Percent)
	}
}

// Re-marking replaces the mark. An instructor who corrects a typo has corrected
// the gradebook too, in the transaction that corrected the submission.
func TestRemarkingReplacesTheEntry(t *testing.T) {
	t.Parallel()

	f := newFixture(t, 1)
	learner := seedUser(t, f.db, f.tenant, "Al-Biruni")
	f.enrol(t, learner)

	quiz := uuid.New()
	f.record(t, quizScore(f.lessons[0], learner, quiz, 3, 10))
	f.record(t, quizScore(f.lessons[0], learner, quiz, 8, 10))

	grades, _ := f.svc.LearnerGrades(t.Context(), f.tenant, f.slug, learner)
	if grades.Result.Points != 8 {
		t.Errorf("points = %d, want the 8 it was re-marked to", grades.Result.Points)
	}
}

// A learner reads their own grades and nobody else's. The user id comes from the
// token; there is no path that takes one from a request.
func TestALearnerSeesOnlyTheirOwnMarks(t *testing.T) {
	t.Parallel()

	f := newFixture(t, 1)
	mine := seedUser(t, f.db, f.tenant, "Mine")
	theirs := seedUser(t, f.db, f.tenant, "Theirs")
	f.enrol(t, mine)
	f.enrol(t, theirs)

	quiz := uuid.New()
	f.record(t, quizScore(f.lessons[0], mine, quiz, 2, 10))
	f.record(t, quizScore(f.lessons[0], theirs, quiz, 10, 10))

	grades, err := f.svc.LearnerGrades(t.Context(), f.tenant, f.slug, mine)
	if err != nil {
		t.Fatalf("learner grades: %v", err)
	}

	if len(grades.Entries) != 1 {
		t.Fatalf("%d entries, want only my own", len(grades.Entries))
	}
	if grades.Entries[0].Points != 2 {
		t.Errorf("read back %d points, want my 2", grades.Entries[0].Points)
	}
}

// Everybody enrolled appears, marked or not. A gradebook that listed only the
// learners with marks would hide the ones who have handed nothing in, who are
// exactly the ones a teacher opens it to find.
func TestTheGradebookListsLearnersWithNoMarksAtAll(t *testing.T) {
	t.Parallel()

	f := newFixture(t, 1)
	marked := seedUser(t, f.db, f.tenant, "Aaa Marked")
	silent := seedUser(t, f.db, f.tenant, "Zzz Silent")
	f.enrol(t, marked)
	f.enrol(t, silent)

	f.record(t, quizScore(f.lessons[0], marked, uuid.New(), 7, 10))

	book, err := f.svc.CourseGradebook(t.Context(), f.tenant, f.slug)
	if err != nil {
		t.Fatalf("gradebook: %v", err)
	}

	if len(book.Learners) != 2 {
		t.Fatalf("%d learners, want 2", len(book.Learners))
	}

	// Ordered by name, so the marked one is first.
	if book.Learners[0].Result.Percent != 70 {
		t.Errorf("the marked learner is at %d%%, want 70", book.Learners[0].Result.Percent)
	}
	if book.Learners[1].Result.Graded != 0 {
		t.Errorf("the silent learner has %d graded items, want 0", book.Learners[1].Result.Graded)
	}
	if book.Learners[1].Result.Band.Label != "" {
		t.Errorf("the silent learner was graded %q; nothing has been marked",
			book.Learners[1].Result.Band.Label)
	}
}

/*
The gradebook is four queries, whatever the size of the class.

Asserted across a growing class, because a constant is what an N+1 looks like at
n=1. This is the test that makes a gradebook slow for one customer into a build
failure for everybody.
*/
func TestTheGradebookDoesNotQueryPerLearner(t *testing.T) {
	t.Parallel()

	const want = 4 // course, items, entries, learners.

	for _, learners := range []int{1, 5, 20} {
		f := newFixture(t, 2)
		quizzes := []uuid.UUID{uuid.New(), uuid.New()}

		for i := range learners {
			learner := seedUser(t, f.db, f.tenant, fmt.Sprintf("Learner %02d", i))
			f.enrol(t, learner)
			f.record(t, quizScore(f.lessons[0], learner, quizzes[0], i%11, 10))
			f.record(t, quizScore(f.lessons[1], learner, quizzes[1], i%11, 10))
		}

		counter := &database.Counter{}
		ctx := database.WithCounter(t.Context(), counter)

		book, err := f.svc.CourseGradebook(ctx, f.tenant, f.slug)
		if err != nil {
			t.Fatalf("%d learners: %v", learners, err)
		}
		if len(book.Learners) != learners {
			t.Fatalf("%d learners came back, want %d", len(book.Learners), learners)
		}

		if got := counter.Count(); got != want {
			t.Errorf("%d learners cost %d queries, want %d", learners, got, want)
		}
	}
}

/*
A course grades by the scale it points at.

And the scale is the workspace's, not this package's opinion: a pass/fail scale
calls 70% a Pass where the default calls it a C.
*/
func TestACourseGradesByItsOwnScale(t *testing.T) {
	t.Parallel()

	f := newFixture(t, 1)
	learner := seedUser(t, f.db, f.tenant, "Fatima")
	f.enrol(t, learner)
	f.record(t, quizScore(f.lessons[0], learner, uuid.New(), 7, 10))

	before, _ := f.svc.LearnerGrades(t.Context(), f.tenant, f.slug, learner)
	if before.Scale.Builtin != true || before.Result.Band.Label != "C" {
		t.Fatalf("the default scale graded 70%% as %q, want C", before.Result.Band.Label)
	}

	scale, err := f.svc.CreateScale(t.Context(), f.tenant, grade.Scale{
		Name: "Pass/fail",
		Bands: []grade.Band{
			{Label: "Pass", Min: 50, IsPass: true},
			{Label: "Fail", Min: 0},
		},
	})
	if err != nil {
		t.Fatalf("create scale: %v", err)
	}

	if err := f.svc.SetCourseScale(t.Context(), f.tenant, f.slug, &scale.ID); err != nil {
		t.Fatalf("set course scale: %v", err)
	}

	after, err := f.svc.LearnerGrades(t.Context(), f.tenant, f.slug, learner)
	if err != nil {
		t.Fatalf("learner grades: %v", err)
	}
	if after.Result.Band.Label != "Pass" {
		t.Errorf("70%% graded %q, want Pass", after.Result.Band.Label)
	}
	if after.Scale.Builtin {
		t.Error("the course still reports the built-in scale")
	}
}

// Deleting a scale does not delete the courses that used it. They fall back to
// the default, which is what `ON DELETE SET NULL` arranges.
func TestDeletingAScaleFallsTheCourseBackToTheDefault(t *testing.T) {
	t.Parallel()

	f := newFixture(t, 1)
	learner := seedUser(t, f.db, f.tenant, "Maryam")
	f.enrol(t, learner)
	f.record(t, quizScore(f.lessons[0], learner, uuid.New(), 7, 10))

	scale, err := f.svc.CreateScale(t.Context(), f.tenant, grade.Scale{
		Name:  "Pass/fail",
		Bands: []grade.Band{{Label: "Pass", Min: 50, IsPass: true}, {Label: "Fail", Min: 0}},
	})
	if err != nil {
		t.Fatalf("create scale: %v", err)
	}
	if err := f.svc.SetCourseScale(t.Context(), f.tenant, f.slug, &scale.ID); err != nil {
		t.Fatalf("set scale: %v", err)
	}
	if err := f.svc.DeleteScale(t.Context(), f.tenant, scale.ID); err != nil {
		t.Fatalf("delete scale: %v", err)
	}

	grades, err := f.svc.LearnerGrades(t.Context(), f.tenant, f.slug, learner)
	if err != nil {
		t.Fatalf("the course died with its scale: %v", err)
	}
	if !grades.Scale.Builtin || grades.Result.Band.Label != "C" {
		t.Errorf("graded %q by %q, want C by the default", grades.Result.Band.Label, grades.Scale.Name)
	}
}

// A scale belongs to a workspace. Pointing a course at another workspace's scale
// is refused by the service, before RLS ever has to catch it.
func TestACourseCannotGradeByAnotherWorkspacesScale(t *testing.T) {
	t.Parallel()

	mine := newFixture(t, 1)
	theirs := newFixture(t, 1)

	scale, err := theirs.svc.CreateScale(t.Context(), theirs.tenant, grade.Scale{
		Name:  "Theirs",
		Bands: []grade.Band{{Label: "Pass", Min: 50, IsPass: true}, {Label: "Fail", Min: 0}},
	})
	if err != nil {
		t.Fatalf("create scale: %v", err)
	}

	if err := mine.svc.SetCourseScale(t.Context(), mine.tenant, mine.slug, &scale.ID); err == nil {
		t.Fatal("a course was pointed at another workspace's grading scale")
	}
}

// Two scales cannot share a name, or an author choosing one from a list is
// choosing between two identical rows.
func TestScaleNamesAreUniquePerWorkspace(t *testing.T) {
	t.Parallel()

	f := newFixture(t, 1)
	scale := grade.Scale{
		Name:  "Honours",
		Bands: []grade.Band{{Label: "Pass", Min: 40, IsPass: true}, {Label: "Fail", Min: 0}},
	}

	if _, err := f.svc.CreateScale(t.Context(), f.tenant, scale); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := f.svc.CreateScale(t.Context(), f.tenant, scale); err == nil {
		t.Fatal("a second scale took the same name")
	}
}

// The built-in default is in the list once, and it is not a row anybody can edit.
func TestScalesListsTheDefaultFirst(t *testing.T) {
	t.Parallel()

	f := newFixture(t, 1)
	if _, err := f.svc.CreateScale(t.Context(), f.tenant, grade.Scale{
		Name:  "Alpha",
		Bands: []grade.Band{{Label: "Pass", Min: 50, IsPass: true}, {Label: "Fail", Min: 0}},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	scales, err := f.svc.Scales(t.Context(), f.tenant)
	if err != nil {
		t.Fatalf("scales: %v", err)
	}
	if len(scales) != 2 {
		t.Fatalf("%d scales, want the default and mine", len(scales))
	}
	if !scales[0].Builtin {
		t.Error("the built-in default is not first")
	}
	if len(scales[1].Bands) != 2 {
		t.Errorf("the stored scale came back with %d bands, want 2", len(scales[1].Bands))
	}
}

// A score against a lesson in another workspace has no course to hang on, and is
// refused rather than recorded against whatever the join happened to find.
func TestAScoreForAnotherWorkspacesLessonIsRefused(t *testing.T) {
	t.Parallel()

	mine := newFixture(t, 1)
	theirs := newFixture(t, 1)
	learner := seedUser(t, mine.db, mine.tenant, "Learner")

	err := mine.db.WithTenant(t.Context(), mine.tenant, func(ctx context.Context, tx pgx.Tx) error {
		return mine.svc.Record(ctx, tx, mine.tenant,
			quizScore(theirs.lessons[0], learner, uuid.New(), 5, 10))
	})
	if err == nil {
		t.Fatal("a grade was recorded against another workspace's lesson")
	}
}

/*
A retry that goes worse does not lower the grade.

A quiz may be attempted several times, and a learner who scores 10 of 10 and then
retries out of curiosity should not be punished for the curiosity. `assess` sets
`KeepHighest`; `assign` does not, because a marker correcting a typo means it.
*/
func TestKeepHighestLeavesABetterMarkAlone(t *testing.T) {
	t.Parallel()

	f := newFixture(t, 1)
	learner := seedUser(t, f.db, f.tenant, "Curious")
	f.enrol(t, learner)

	quiz := uuid.New()
	best := quizScore(f.lessons[0], learner, quiz, 10, 10)
	best.KeepHighest = true
	worse := quizScore(f.lessons[0], learner, quiz, 4, 10)
	worse.KeepHighest = true

	f.record(t, best)
	f.record(t, worse)

	grades, _ := f.svc.LearnerGrades(t.Context(), f.tenant, f.slug, learner)
	if grades.Result.Points != 10 {
		t.Errorf("points = %d after a worse retry, want the 10 already earned", grades.Result.Points)
	}

	// A better attempt does replace it. `KeepHighest` keeps the highest, not the
	// first — a rule that only ever refused an update would be indistinguishable
	// from a bug that never updated.
	poor := quizScore(f.lessons[0], learner, quiz, 1, 10)
	poor.KeepHighest = true
	improved := quizScore(f.lessons[0], learner, quiz, 7, 10)
	improved.KeepHighest = true

	second := newFixture(t, 1)
	other := seedUser(t, second.db, second.tenant, "Improving")
	second.enrol(t, other)

	poor.LessonID, poor.UserID = second.lessons[0], other
	improved.LessonID, improved.UserID = second.lessons[0], other
	second.record(t, poor)
	second.record(t, improved)

	improvedGrades, _ := second.svc.LearnerGrades(t.Context(), second.tenant, second.slug, other)
	if improvedGrades.Result.Points != 7 {
		t.Errorf("points = %d after a better retry, want 7", improvedGrades.Result.Points)
	}
}

/*
The better mark is the better fraction, not the better integer percentage.

33 of 100 is 33%. 1 of 3 is 33.33%, and it is the better score — but integer
division truncates both to 33, and a comparison made on those calls it a draw and
keeps whichever arrived first. The learner loses the third of a percent, and every
tie like it.

Chosen so the two rules disagree. A pair like 9 of 10 against 89 of 100 does not
distinguish them: 90 beats 89 whether you divide first or cross-multiply.
*/
func TestKeepHighestComparesFractionsNotTruncatedPercentages(t *testing.T) {
	t.Parallel()

	f := newFixture(t, 1)
	learner := seedUser(t, f.db, f.tenant, "Precise")
	f.enrol(t, learner)

	quiz := uuid.New()

	coarse := quizScore(f.lessons[0], learner, quiz, 33, 100) // 33%
	coarse.KeepHighest = true
	fine := quizScore(f.lessons[0], learner, quiz, 1, 3) // 33.33%
	fine.KeepHighest = true

	f.record(t, coarse)
	f.record(t, fine)

	grades, _ := f.svc.LearnerGrades(t.Context(), f.tenant, f.slug, learner)
	if len(grades.Entries) != 1 {
		t.Fatalf("%d entries, want 1", len(grades.Entries))
	}
	if grades.Entries[0].MaxPoints != 3 {
		t.Errorf("kept %d of %d, want 1 of 3 — the better fraction",
			grades.Entries[0].Points, grades.Entries[0].MaxPoints)
	}

	// And the other way round: the worse fraction must not displace the better one,
	// even though their truncated percentages are equal.
	f.record(t, coarse)

	after, _ := f.svc.LearnerGrades(t.Context(), f.tenant, f.slug, learner)
	if after.Entries[0].MaxPoints != 3 {
		t.Errorf("33 of 100 displaced the better 1 of 3")
	}
}

// Without KeepHighest, the newest mark stands — including a lower one. A marker
// who corrects a grade downwards means to.
func TestAMarkerCanLowerAGrade(t *testing.T) {
	t.Parallel()

	f := newFixture(t, 1)
	learner := seedUser(t, f.db, f.tenant, "Marked")
	f.enrol(t, learner)

	assignment := uuid.New()
	high := grade.Score{
		LessonID: f.lessons[0], UserID: learner,
		Source: grade.SourceAssignment, SourceID: assignment,
		Title: "Essay", Points: 90, MaxPoints: 100,
	}
	low := high
	low.Points = 40

	f.record(t, high)
	f.record(t, low)

	grades, _ := f.svc.LearnerGrades(t.Context(), f.tenant, f.slug, learner)
	if grades.Result.Points != 40 {
		t.Errorf("points = %d, want the 40 the marker corrected it to", grades.Result.Points)
	}
}
