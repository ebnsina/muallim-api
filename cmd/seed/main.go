// Command seed fills a development database with a plausible, large dataset.
//
// It exists so that a page can be judged at the size it will really be. A
// marking queue of three rows tells you nothing about a marking queue of four
// hundred, and a catalogue of two courses hides every N+1 in the code that lists
// them.
//
// Everything is written inside a transaction bound to its tenant, so every row
// passes the same row-level security policy a request would — a seeder that
// reaches around RLS is a seeder that proves nothing. That rules out COPY:
// Postgres refuses `COPY FROM` outright on a table where RLS is active for the
// current role, with `0A000: COPY FROM not supported with row-level security`.
// So the bulk load is chunked multi-row INSERTs instead. Slower than COPY, and
// still four orders of magnitude faster than a statement per row.
//
//	make seed              # a small workspace, enough to click around
//	make seed-huge         # a few hundred thousand rows
//	go run ./cmd/seed -help
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/lms-api/internal/assess"
	"github.com/ebnsina/lms-api/internal/auth"
	"github.com/ebnsina/lms-api/internal/catalog"
	"github.com/ebnsina/lms-api/internal/enroll"
	"github.com/ebnsina/lms-api/internal/platform/database"
)

// The demo account. Named in the output, so nobody has to read this file to
// sign in.
const (
	DemoPassword = "demo-password-please-change"

	demoOwner      = "demo@muallim.test"
	demoInstructor = "instructor@muallim.test"
	demoMarker     = "marker@muallim.test"
	demoStudent    = "student@muallim.test"
)

type config struct {
	workspaces int
	courses    int
	students   int
	topics     int
	lessons    int

	// quizRate is the share of courses whose last lesson carries a quiz.
	quizRate float64

	// enrolRate is the share of (student, course) pairs that become enrolments.
	enrolRate float64

	// attemptRate is the share of enrolled learners who sit a course's quiz.
	attemptRate float64

	reset bool
	seed  uint64
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var cfg config

	flag.IntVar(&cfg.workspaces, "workspaces", 1, "how many tenants; the first is always `localhost`")
	flag.IntVar(&cfg.courses, "courses", 8, "courses per workspace")
	flag.IntVar(&cfg.students, "students", 40, "students per workspace")
	flag.IntVar(&cfg.topics, "topics", 4, "topics per course")
	flag.IntVar(&cfg.lessons, "lessons", 5, "lessons per topic")
	flag.Float64Var(&cfg.quizRate, "quiz-rate", 0.5, "share of courses with a quiz")
	flag.Float64Var(&cfg.enrolRate, "enrol-rate", 0.25, "share of student/course pairs that enrol")
	flag.Float64Var(&cfg.attemptRate, "attempt-rate", 0.6, "share of enrolled learners who sit the quiz")
	flag.BoolVar(&cfg.reset, "reset", false, "delete the seeded workspaces first, and everything in them")
	flag.Uint64Var(&cfg.seed, "seed", 1, "the random seed; the same seed builds the same data")
	flag.Parse()

	url := os.Getenv("LMS_DATABASE_URL")
	if url == "" {
		return errors.New("LMS_DATABASE_URL is required")
	}

	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	db, err := database.New(ctx, database.Options{URL: url, MaxConns: 4, StatementTimeout: 10 * time.Minute}, log)
	if err != nil {
		return err
	}
	defer db.Close()

	// One hash, reused by every account.
	//
	// Argon2id allocates 64 MiB and burns a tenth of a second by design; hashing
	// three thousand demo students would take five minutes and prove nothing. Every
	// seeded account shares one password, so every account shares one digest. This
	// is a development fixture and it says so on the tin.
	started := time.Now()
	hash, err := auth.HashPassword(DemoPassword)
	if err != nil {
		return fmt.Errorf("hash the demo password: %w", err)
	}

	if cfg.reset {
		if err := reset(ctx, db, cfg); err != nil {
			return err
		}
	}

	var total counts
	for index := range cfg.workspaces {
		// Deterministic, and different per workspace: the same `-seed` rebuilds the
		// same database, which is what makes a screenshot of it worth comparing.
		rng := rand.New(rand.NewPCG(cfg.seed, uint64(index)))

		got, err := seedWorkspace(ctx, db, cfg, index, hash, rng)
		if err != nil {
			return fmt.Errorf("workspace %d: %w", index, err)
		}
		total.add(got)
	}

	report(cfg, total, time.Since(started))
	return nil
}

// counts is what was written, for the summary.
type counts struct {
	tenants, users, courses, lessons, quizzes, questions int
	enrolments, progress, attempts, answers              int
}

func (c *counts) add(other counts) {
	c.tenants += other.tenants
	c.users += other.users
	c.courses += other.courses
	c.lessons += other.lessons
	c.quizzes += other.quizzes
	c.questions += other.questions
	c.enrolments += other.enrolments
	c.progress += other.progress
	c.attempts += other.attempts
	c.answers += other.answers
}

func subdomain(index int) string {
	if index == 0 {
		// The one the dev server resolves for `localhost`, so `make run` works.
		return "localhost"
	}
	return fmt.Sprintf("demo-%d", index)
}

// reset deletes the seeded workspaces, and then the people who were only ever in
// them.
//
// Deleting the tenants is not enough. `users` is a global table — one account,
// however many schools a person teaches at — so dropping a tenant cascades away
// its memberships and leaves the accounts behind, orphaned, holding the email
// addresses the next run wants to reuse. Seeding twice used to fail on a unique
// violation for exactly this reason.
//
// The sweep runs unbound, which is the only state in which an orphan is visible
// at all: `users_orphan_visible` shows a row with no membership only when
// app.tenant_id is null, and `users_erase_orphan` lets it be deleted only then.
// An account that still belongs to some workspace this command did not create is
// invisible here and survives, which is the whole point of that policy.
//
// It is behind a flag, because "wipe the database" is not a thing to do because
// an argument was misread.
func reset(ctx context.Context, db *database.DB, cfg config) error {
	names := make([]string, 0, cfg.workspaces)
	for index := range cfg.workspaces {
		names = append(names, subdomain(index))
	}

	return db.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM tenants WHERE subdomain = ANY($1)`, names); err != nil {
			return fmt.Errorf("delete workspaces: %w", err)
		}

		// Only the accounts this command invents, and only while they belong to
		// nobody. `@muallim.test` is a reserved TLD; nothing real can live there.
		if _, err := tx.Exec(ctx, `DELETE FROM users WHERE email LIKE '%@muallim.test'`); err != nil {
			return fmt.Errorf("erase the orphaned demo accounts: %w", err)
		}
		return nil
	})
}

func seedWorkspace(ctx context.Context, db *database.DB, cfg config, index int, hash string, rng *rand.Rand) (counts, error) {
	var got counts

	tenantID := uuid.New()
	name := workspaceNames[index%len(workspaceNames)]

	// The tenant row is not tenant-scoped, so it goes in unbound.
	err := db.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO tenants (id, subdomain, name) VALUES ($1, $2, $3)
			 ON CONFLICT (lower(subdomain)) DO UPDATE SET name = excluded.name
			 RETURNING id`,
			tenantID, subdomain(index), name).Scan(&tenantID)
	})
	if err != nil {
		return got, fmt.Errorf("create tenant: %w", err)
	}
	got.tenants = 1

	// Everything below is written inside a transaction bound to this tenant, so
	// every insert passes the same RLS policy a request would.
	err = db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		people, err := seedPeople(ctx, tx, tenantID, index, hash, cfg.students)
		if err != nil {
			return fmt.Errorf("people: %w", err)
		}
		got.users = len(people.everyone)

		catalogue, err := seedCatalogue(ctx, tx, tenantID, cfg, rng)
		if err != nil {
			return fmt.Errorf("catalogue: %w", err)
		}
		got.courses = len(catalogue)
		for _, course := range catalogue {
			got.lessons += len(course.lessons)
			if course.quiz != nil {
				got.quizzes++
				got.questions += len(course.quiz.questions)
			}
		}

		learning, err := seedLearning(ctx, tx, tenantID, cfg, catalogue, people.students, rng)
		if err != nil {
			return fmt.Errorf("learning: %w", err)
		}
		got.enrolments = learning.enrolments
		got.progress = learning.progress
		got.attempts = learning.attempts
		got.answers = learning.answers

		return nil
	})

	return got, err
}

// ---------------------------------------------------------------------- people

type people struct {
	owner      uuid.UUID
	instructor uuid.UUID
	students   []uuid.UUID
	everyone   []uuid.UUID
}

func seedPeople(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, index int, hash string, howMany int) (people, error) {
	var p people

	type account struct {
		id    uuid.UUID
		email string
		name  string
		role  string
	}

	accounts := make([]account, 0, howMany+4)

	// The named accounts exist only in the first workspace. A second `demo@` in a
	// second tenant would collide on the users table, which is global — one person,
	// however many schools they teach at.
	if index == 0 {
		for _, seat := range []struct{ email, name, role string }{
			{demoOwner, "Fatima al-Fihri", auth.RoleOwner},
			{demoInstructor, "Ibn al-Haytham", auth.RoleInstructor},
			{demoMarker, "Maryam al-Astrulabi", auth.RoleInstructor},
			{demoStudent, "Al-Khwarizmi", auth.RoleStudent},
		} {
			accounts = append(accounts, account{uuid.New(), seat.email, seat.name, seat.role})
		}
	} else {
		accounts = append(accounts,
			account{uuid.New(), fmt.Sprintf("owner-%d@muallim.test", index), scholars[0], auth.RoleOwner},
			account{uuid.New(), fmt.Sprintf("instructor-%d@muallim.test", index), scholars[1], auth.RoleInstructor},
		)
	}

	for student := range howMany {
		accounts = append(accounts, account{
			id:    uuid.New(),
			email: fmt.Sprintf("learner-%d-%d@muallim.test", index, student),
			name:  scholars[student%len(scholars)],
			role:  auth.RoleStudent,
		})
	}

	users := make([][]any, 0, len(accounts))
	memberships := make([][]any, 0, len(accounts))
	verified := time.Now().Add(-30 * 24 * time.Hour)

	for _, a := range accounts {
		users = append(users, []any{a.id, a.email, hash, a.name, verified})
		memberships = append(memberships, []any{uuid.New(), tenantID, a.id, a.role, "active"})

		p.everyone = append(p.everyone, a.id)
		switch a.role {
		case auth.RoleOwner:
			p.owner = a.id
		case auth.RoleInstructor:
			if p.instructor == uuid.Nil {
				p.instructor = a.id
			}
		case auth.RoleStudent:
			p.students = append(p.students, a.id)
		}
	}

	if err := bulkInsert(ctx, tx, "users",
		[]string{"id", "email", "password_hash", "name", "email_verified_at"}, users); err != nil {
		return p, err
	}

	if err := bulkInsert(ctx, tx, "memberships",
		[]string{"id", "tenant_id", "user_id", "role", "status"}, memberships); err != nil {
		return p, err
	}

	return p, nil
}

/*
One INSERT per chunk of rows, rather than one per row.

Postgres caps a statement at 65535 bind parameters, so the chunk is sized by
the column count and not guessed. Above about a thousand rows the win flattens
and the statement gets long enough that parsing it starts to cost something, so
it is capped there too.
*/
func bulkInsert(ctx context.Context, tx pgx.Tx, table string, cols []string, rows [][]any) error {
	if len(rows) == 0 {
		return nil
	}

	const maxParams = 65535
	chunk := min(maxParams/len(cols), 1000)

	for start := 0; start < len(rows); start += chunk {
		end := min(start+chunk, len(rows))
		batch := rows[start:end]

		var sql strings.Builder
		fmt.Fprintf(&sql, "INSERT INTO %s (%s) VALUES ", table, strings.Join(cols, ", "))

		args := make([]any, 0, len(batch)*len(cols))
		for i, row := range batch {
			if i > 0 {
				sql.WriteByte(',')
			}
			sql.WriteByte('(')
			for j := range cols {
				if j > 0 {
					sql.WriteByte(',')
				}
				fmt.Fprintf(&sql, "$%d", len(args)+j+1)
			}
			sql.WriteByte(')')
			args = append(args, row...)
		}

		if _, err := tx.Exec(ctx, sql.String(), args...); err != nil {
			return fmt.Errorf("insert into %s: %w", table, err)
		}
	}
	return nil
}

// ------------------------------------------------------------------- catalogue

type seededQuiz struct {
	id        uuid.UUID
	lessonID  uuid.UUID
	questions []seededQuestion
	maxPoints int
	passing   int
}

type seededQuestion struct {
	id      uuid.UUID
	kind    string
	points  int
	correct uuid.UUID // the correct option, for the choice types
}

type seededCourse struct {
	id      uuid.UUID
	slug    string
	lessons []uuid.UUID
	quiz    *seededQuiz
}

func seedCatalogue(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, cfg config, rng *rand.Rand) ([]seededCourse, error) {
	var (
		courses  [][]any
		topics   [][]any
		lessons  [][]any
		quizzes  [][]any
		question [][]any
		options  [][]any
	)

	seeded := make([]seededCourse, 0, cfg.courses)
	now := time.Now()

	for index := range cfg.courses {
		course := seededCourse{id: uuid.New(), slug: fmt.Sprintf("%s-%d", slugify(courseTitles[index%len(courseTitles)]), index)}

		// One course in six stays a draft. A catalogue where everything is published
		// never exercises the query filter that hides the rest.
		status, published := "published", any(now.Add(-time.Duration(index)*24*time.Hour))
		if index%6 == 5 {
			status, published = "draft", nil
		}

		drip := []string{"none", "none", "none", "scheduled", "after_enrolment", "sequential"}[index%6]

		courses = append(courses, []any{
			course.id, tenantID, course.slug, courseTitles[index%len(courseTitles)],
			courseSummaries[index%len(courseSummaries)],
			[]string{"beginner", "intermediate", "advanced", "expert"}[index%4],
			status, published, drip,
		})

		for t := range cfg.topics {
			topicID := uuid.New()
			topics = append(topics, []any{topicID, tenantID, course.id, topicTitles[t%len(topicTitles)], t})

			for l := range cfg.lessons {
				lessonID := uuid.New()
				course.lessons = append(course.lessons, lessonID)

				// The first lesson of the first topic is the free sample.
				preview := t == 0 && l == 0

				lessons = append(lessons, []any{
					lessonID, tenantID, topicID,
					fmt.Sprintf("%s %d.%d", lessonTitles[(t+l)%len(lessonTitles)], t+1, l+1),
					"text", 240 + rng.IntN(900), preview, l,
					lessonBody, "none", "", "",
				})
			}
		}

		// Half the courses end with a quiz, on a lesson of their own.
		if rng.Float64() < cfg.quizRate {
			quizLesson := uuid.New()
			lastTopic := topics[len(topics)-1][0].(uuid.UUID)

			lessons = append(lessons, []any{
				quizLesson, tenantID, lastTopic, "End-of-course quiz",
				"quiz", 600, false, cfg.lessons,
				"", "none", "", "",
			})
			course.lessons = append(course.lessons, quizLesson)

			quiz := &seededQuiz{id: uuid.New(), lessonID: quizLesson, passing: 60}
			quizzes = append(quizzes, []any{
				quiz.id, tenantID, quizLesson, "End-of-course quiz",
				"Everything from the four sections above.", 0, 3, quiz.passing,
			})

			for q := range 5 {
				item := seededQuestion{id: uuid.New(), kind: assess.TypeSingleChoice, points: 2}

				// The last question is an essay, so the marking queue has work in it.
				if q == 4 {
					item.kind, item.points = assess.TypeOpenEnded, 4
				}
				quiz.maxPoints += item.points

				question = append(question, []any{
					item.id, tenantID, quiz.id, item.kind, questionPrompts[q%len(questionPrompts)],
					item.points, q, "The answer follows from the definition in section two.", false, "[]",
				})

				if item.kind == assess.TypeSingleChoice {
					correctAt := rng.IntN(4)
					for o := range 4 {
						optionID := uuid.New()
						if o == correctAt {
							item.correct = optionID
						}
						options = append(options, []any{
							optionID, tenantID, item.id, optionContents[o], o, o == correctAt, uuid.New(), "",
						})
					}
				}

				quiz.questions = append(quiz.questions, item)
			}

			course.quiz = quiz
		}

		seeded = append(seeded, course)
	}

	copies := []struct {
		table string
		cols  []string
		rows  [][]any
	}{
		{"courses", []string{"id", "tenant_id", "slug", "title", "summary", "difficulty", "status", "published_at", "drip_mode"}, courses},
		{"topics", []string{"id", "tenant_id", "course_id", "title", "position"}, topics},
		{"lessons", []string{"id", "tenant_id", "topic_id", "title", "content_type", "duration_seconds", "is_preview", "position", "content", "video_source", "video_url", "video_embed_url"}, lessons},
		{"quizzes", []string{"id", "tenant_id", "lesson_id", "title", "description", "time_limit_seconds", "max_attempts", "passing_percent"}, quizzes},
		{"questions", []string{"id", "tenant_id", "quiz_id", "type", "prompt", "points", "position", "explanation", "case_sensitive", "accepted"}, question},
		{"question_options", []string{"id", "tenant_id", "question_id", "content", "position", "is_correct", "match_id", "match_content"}, options},
	}

	for _, c := range copies {
		if err := bulkInsert(ctx, tx, c.table, c.cols, c.rows); err != nil {
			return nil, err
		}
	}

	return seeded, nil
}

// -------------------------------------------------------------------- learning

type learningCounts struct {
	enrolments, progress, attempts, answers int
}

func seedLearning(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, cfg config, catalogue []seededCourse, students []uuid.UUID, rng *rand.Rand) (learningCounts, error) {
	var got learningCounts

	var (
		enrolments [][]any
		progress   [][]any
		rollups    [][]any
		attempts   [][]any
		answers    [][]any
	)

	now := time.Now()

	for _, course := range catalogue {
		for _, student := range students {
			if rng.Float64() >= cfg.enrolRate {
				continue
			}

			enrolledAt := now.Add(-time.Duration(rng.IntN(90)) * 24 * time.Hour)

			// How far this learner got. A quarter finish; a tenth never start.
			done := rng.IntN(len(course.lessons) + 1)
			switch {
			case rng.Float64() < 0.25:
				done = len(course.lessons)
			case rng.Float64() < 0.1:
				done = 0
			}

			status := enroll.StatusActive
			var completedAt any
			if done == len(course.lessons) {
				status = enroll.StatusCompleted
				completedAt = enrolledAt.Add(14 * 24 * time.Hour)
			}

			enrolments = append(enrolments, []any{
				uuid.New(), tenantID, course.id, student, status,
				enroll.SourceSelf, enrolledAt, completedAt,
			})
			got.enrolments++

			for i, lesson := range course.lessons {
				if i >= done {
					break
				}
				progress = append(progress, []any{
					uuid.New(), tenantID, student, lesson, course.id,
					enrolledAt.Add(time.Duration(i) * 36 * time.Hour), 180 + rng.IntN(600),
				})
				got.progress++
			}

			// The roll-up is written here rather than recomputed, because the seeder is
			// the transaction that changed the rows it summarises — which is the same
			// rule the application follows.
			percent := 0
			if len(course.lessons) > 0 {
				percent = done * 100 / len(course.lessons)
			}
			rollups = append(rollups, []any{tenantID, student, course.id, done, len(course.lessons), percent})

			if course.quiz == nil || rng.Float64() >= cfg.attemptRate {
				continue
			}

			attemptID := uuid.New()
			submitted := enrolledAt.Add(time.Duration(rng.IntN(60)) * 24 * time.Hour)

			// One attempt in four is still waiting for a person, so the marking queue
			// is never empty. The rest are graded.
			awaiting := rng.Float64() < 0.25

			var points int
			answerRows := make([][]any, 0, len(course.quiz.questions))

			for _, item := range course.quiz.questions {
				switch {
				case item.kind == assess.TypeOpenEnded && awaiting:
					answerRows = append(answerRows, []any{
						uuid.New(), tenantID, attemptID, item.id,
						`{"text":"An answer of some length, written under time pressure."}`,
						false, false, 0, "", nil,
					})

				case item.kind == assess.TypeOpenEnded:
					awarded := rng.IntN(item.points + 1)
					points += awarded
					answerRows = append(answerRows, []any{
						uuid.New(), tenantID, attemptID, item.id,
						`{"text":"An answer of some length, written under time pressure."}`,
						true, awarded == item.points, awarded, "Good, though you never define the term.", submitted,
					})

				default:
					// Two learners in three pick the right option.
					right := rng.Float64() < 0.66
					chosen := item.correct
					if !right {
						chosen = uuid.New() // an option belonging to nothing: a wrong answer.
					}
					awarded := 0
					if right {
						awarded = item.points
					}
					points += awarded

					answerRows = append(answerRows, []any{
						uuid.New(), tenantID, attemptID, item.id,
						fmt.Sprintf(`{"choices":["%s"]}`, chosen),
						true, right, awarded, "", submitted,
					})
				}
			}

			status = assess.StatusGraded
			var graded, passed any = submitted, points*100 >= course.quiz.passing*course.quiz.maxPoints
			if awaiting {
				status, graded, passed = assess.StatusAwaitingReview, nil, nil
			}

			attempts = append(attempts, []any{
				attemptID, tenantID, course.quiz.id, student, 1, status,
				submitted.Add(-30 * time.Minute), submitted, graded, nil,
				points, course.quiz.maxPoints, passed,
			})
			got.attempts++

			answers = append(answers, answerRows...)
			got.answers += len(answerRows)
		}
	}

	copies := []struct {
		table string
		cols  []string
		rows  [][]any
	}{
		{"enrolments", []string{"id", "tenant_id", "course_id", "user_id", "status", "source", "enrolled_at", "completed_at"}, enrolments},
		{"lesson_progress", []string{"id", "tenant_id", "user_id", "lesson_id", "course_id", "completed_at", "seconds_watched"}, progress},
		{"course_progress", []string{"tenant_id", "user_id", "course_id", "lessons_completed", "lessons_total", "percent"}, rollups},
		{"quiz_attempts", []string{"id", "tenant_id", "quiz_id", "user_id", "number", "status", "started_at", "submitted_at", "graded_at", "expires_at", "points", "max_points", "passed"}, attempts},
		{"attempt_answers", []string{"id", "tenant_id", "attempt_id", "question_id", "response", "graded", "correct", "points", "feedback", "graded_at"}, answers},
	}

	for _, c := range copies {
		if err := bulkInsert(ctx, tx, c.table, c.cols, c.rows); err != nil {
			return got, err
		}
	}

	return got, nil
}

// ----------------------------------------------------------------------- extra

func slugify(title string) string {
	var out strings.Builder
	for _, r := range strings.ToLower(title) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out.WriteRune(r)
		case r == ' ' || r == '-':
			out.WriteByte('-')
		}
	}
	return strings.Trim(out.String(), "-")
}

func report(cfg config, got counts, took time.Duration) {
	rows := got.users + got.courses + got.lessons + got.quizzes + got.questions +
		got.enrolments + got.progress + got.attempts + got.answers

	fmt.Printf("seeded %d rows in %s\n\n", rows, took.Round(time.Millisecond))
	fmt.Printf("  workspaces  %d\n  people      %d\n  courses     %d\n  lessons     %d\n",
		got.tenants, got.users, got.courses, got.lessons)
	fmt.Printf("  quizzes     %d (%d questions)\n  enrolments  %d\n  progress    %d\n",
		got.quizzes, got.questions, got.enrolments, got.progress)
	fmt.Printf("  attempts    %d (%d answers)\n\n", got.attempts, got.answers)

	fmt.Printf("sign in at http://localhost:5173 — every account shares one password\n\n")
	fmt.Printf("  %-28s %s   owner\n", demoOwner, DemoPassword)
	fmt.Printf("  %-28s %s   instructor\n", demoInstructor, DemoPassword)
	fmt.Printf("  %-28s %s   marks essays\n", demoMarker, DemoPassword)
	fmt.Printf("  %-28s %s   student\n", demoStudent, DemoPassword)

	if cfg.workspaces > 1 {
		fmt.Printf("\nthe other workspaces answer to demo-1 … demo-%d; reach them by Host header\n", cfg.workspaces-1)
	}
}

// The catalogue reads like a school. Courses named after what the Islamic Golden
// Age actually studied, and learners named after the people who studied it.
var (
	workspaceNames = []string{"Bayt al-Hikma", "Al-Qarawiyyin", "Nizamiyya", "Maragheh Observatory", "Al-Azhar"}

	scholars = []string{
		"Al-Khwarizmi", "Ibn Sina", "Al-Biruni", "Ibn al-Haytham", "Fatima al-Fihri",
		"Al-Kindi", "Maryam al-Astrulabi", "Jabir ibn Hayyan", "Al-Razi", "Ibn Rushd",
		"Nasir al-Din al-Tusi", "Al-Zahrawi", "Sutayta al-Mahamali", "Ibn Battuta",
		"Al-Jazari", "Ibn Khaldun", "Al-Farabi", "Zaynab al-Shahda",
	}

	courseTitles = []string{
		"Algebra from First Principles",
		"The Book of Optics",
		"Astronomy and the Astrolabe",
		"Medicine: The Canon",
		"Cartography and the Known World",
		"Mechanical Engineering of the Automata",
		"Arabic Grammar and Rhetoric",
		"The Method of Chemistry",
		"Philosophy After Aristotle",
		"Arithmetic for Merchants",
		"Trigonometry and the Sphere",
		"The Muqaddimah: History as a Science",
	}

	courseSummaries = []string{
		"Where the word algorithm comes from, and why the method matters more than the answer.",
		"How light travels, how the eye receives it, and how to prove both with a darkened room.",
		"Read the sky, and build the instrument that reads it for you.",
		"A physician's syllabus, from diagnosis to the pharmacology of what you can grow.",
	}

	topicTitles = []string{"Foundations", "The method", "In practice", "Where it breaks", "Further reading"}

	lessonTitles = []string{"An introduction to", "Working through", "A closer look at", "Exercises on", "Notes on"}

	questionPrompts = []string{
		"Which of these follows from the definition given in section two?",
		"A merchant divides an estate into unequal shares. Which method applies?",
		"Which instrument measures the altitude of a star above the horizon?",
		"Which statement about refraction is true?",
		"Explain, in your own words, why the method matters more than the answer.",
	}

	optionContents = []string{
		"The first, by reduction",
		"The second, by balancing",
		"Neither; the premise is false",
		"Both, depending on the remainder",
	}

	lessonBody = strings.TrimSpace(`
The method begins, as every method must, with a statement of what is known and a
statement of what is sought. Between the two lies the work.

Consider the case where the unknown appears on both sides of the equality. The
older writers would despair of it. The reduction removes it from one side, and
the balancing restores the equality that the removal disturbed. What remains is
a form we have already solved.

This is worth saying plainly: the answer is not the point. Anyone can be told an
answer. The method is the thing that survives the problem it was invented for.`)
)

// Compile-time proof that the constants this file writes are the ones the domain
// packages recognise. A seeder that invents its own status strings is a seeder
// whose data the application will not read.
var (
	_ = catalog.StatusPublished
	_ = enroll.StatusActive
	_ = assess.StatusGraded
)
