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
	neturl "net/url"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/assess"
	"github.com/ebnsina/muallim-api/internal/auth"
	"github.com/ebnsina/muallim-api/internal/catalog"
	"github.com/ebnsina/muallim-api/internal/certify"
	"github.com/ebnsina/muallim-api/internal/enroll"
	"github.com/ebnsina/muallim-api/internal/notify"
	"github.com/ebnsina/muallim-api/internal/platform/database"
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

	// assignmentRate is the share of courses whose last lesson is work to hand in.
	assignmentRate float64

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
	flag.IntVar(&cfg.topics, "topics", 4, "most topics per course (each has 2..N+1)")
	flag.IntVar(&cfg.lessons, "lessons", 5, "most lessons per topic (each has 3..N+2)")
	flag.Float64Var(&cfg.quizRate, "quiz-rate", 0.5, "share of courses with a quiz")
	flag.Float64Var(&cfg.assignmentRate, "assignment-rate", 0.4, "share of courses with an assignment")
	flag.Float64Var(&cfg.enrolRate, "enrol-rate", 0.25, "share of student/course pairs that enrol")
	flag.Float64Var(&cfg.attemptRate, "attempt-rate", 0.6, "share of enrolled learners who sit the quiz")
	flag.BoolVar(&cfg.reset, "reset", false, "delete the seeded workspaces first, and everything in them")
	flag.Uint64Var(&cfg.seed, "seed", 1, "the random seed; the same seed builds the same data")
	flag.Parse()

	url := os.Getenv("MUALLIM_DATABASE_URL")
	if url == "" {
		return errors.New("MUALLIM_DATABASE_URL is required")
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

	report(cfg, url, total, time.Since(started))
	return nil
}

// counts is what was written, for the summary.
type counts struct {
	tenants, users, courses, lessons, quizzes, questions int
	assignments                                          int
	enrolments, progress, attempts, answers              int
	grades, certificates                                 int
	reviews, discussions, announcements                  int
	notifications                                        int
	spaces, threads, posts                               int
}

func (c *counts) add(other counts) {
	c.tenants += other.tenants
	c.users += other.users
	c.courses += other.courses
	c.lessons += other.lessons
	c.quizzes += other.quizzes
	c.questions += other.questions
	c.assignments += other.assignments
	c.enrolments += other.enrolments
	c.progress += other.progress
	c.attempts += other.attempts
	c.answers += other.answers
	c.grades += other.grades
	c.certificates += other.certificates
	c.reviews += other.reviews
	c.discussions += other.discussions
	c.announcements += other.announcements
	c.notifications += other.notifications
	c.spaces += other.spaces
	c.threads += other.threads
	c.posts += other.posts
}

func subdomain(index int) string {
	if index == 0 {
		// The one the dev server resolves for `localhost`, so `make run` works.
		return "localhost"
	}
	return fmt.Sprintf("demo-%d", index)
}

// seedNamespace names this command, so a workspace id derived under it cannot
// collide with a uuid derived anywhere else.
var seedNamespace = uuid.MustParse("6f9619ff-8b86-d011-b42d-00c04fc964ff")

// tenantID is derived from the subdomain rather than invented.
//
// `-reset` deletes a workspace and builds it again. If the new row took a fresh
// uuid, a `cmd/api` that happened to be running would keep serving the old one:
// tenant resolution is cached for five minutes, and the id it hands out would no
// longer exist. The first write that referenced it — an audit line, in practice —
// would fail a foreign key, and the login it belonged to would 500.
//
// Deriving the id means reseeding is invisible to a running server, and it makes
// the whole database reproducible: the same subdomain is always the same
// workspace, so a bookmarked URL and a saved session survive a reseed.
func tenantID(index int) uuid.UUID {
	return uuid.NewSHA1(seedNamespace, []byte(subdomain(index)))
}

/*
id derives a stable uuid from what a row *is*.

Every id in this file used to be `uuid.New()`, which made the `-seed` flag a lie:
the same seed dealt the same hand of decisions and then gave every card a new
face. Reseeding changed every course slug's id, every lesson's, every option's,
so a page anyone had open was a 404 and any script holding an id had to be
rewritten. Only the workspace survived, and only because it was derived.

The key is the row's natural name — the workspace and the email, the course and
the position — never a counter that shifts when something before it is added.
`kind` keeps the namespaces apart: a topic at position 2 of a course and a lesson
at position 2 of a topic must not collide, and without a prefix they would.
*/
func id(kind string, parts ...any) uuid.UUID {
	key := kind
	for _, part := range parts {
		// The separator is what makes this injective. Without it, ("ab", "c") and
		// ("a", "bc") are the same key, and two rows are the same row.
		key += fmt.Sprintf("/%v", part)
	}
	return uuid.NewSHA1(seedNamespace, []byte(key))
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

	id := tenantID(index)
	name := workspaceNames[index%len(workspaceNames)]

	// The tenant row is not tenant-scoped, so it goes in unbound.
	err := db.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO tenants (id, subdomain, name) VALUES ($1, $2, $3)
			 ON CONFLICT (lower(subdomain)) DO UPDATE SET name = excluded.name
			 RETURNING id`,
			id, subdomain(index), name).Scan(&id)
	})
	if err != nil {
		return got, fmt.Errorf("create tenant: %w", err)
	}
	tenantID := id
	got.tenants = 1

	// Everything below is written inside a transaction bound to this tenant, so
	// every insert passes the same RLS policy a request would.
	err = db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		people, err := seedPeople(ctx, tx, tenantID, index, hash, cfg.students)
		if err != nil {
			return fmt.Errorf("people: %w", err)
		}
		got.users = len(people.everyone)

		catalogue, err := seedCatalogue(ctx, tx, tenantID, cfg, people.instructor, rng)
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
			if course.assignment {
				got.assignments++
			}
		}

		learning, err := seedLearning(ctx, tx, tenantID, cfg, catalogue, people.students, people.demo, people.instructor, people.names, rng)
		if err != nil {
			return fmt.Errorf("learning: %w", err)
		}
		got.enrolments = learning.enrolments
		got.progress = learning.progress
		got.attempts = learning.attempts
		got.answers = learning.answers
		got.grades = learning.grades
		got.certificates = learning.certificates
		got.reviews = learning.reviews
		got.discussions = learning.discussions
		got.announcements = learning.announcements
		got.notifications = learning.notifications

		community, err := seedCommunity(ctx, tx, tenantID, catalogue, people, rng)
		if err != nil {
			return fmt.Errorf("community: %w", err)
		}
		got.spaces = community.spaces
		got.threads = community.threads
		got.posts = community.posts
		got.notifications += community.notifications

		if err := seedGamification(ctx, tx, tenantID); err != nil {
			return fmt.Errorf("gamification: %w", err)
		}

		if err := seedBank(ctx, tx, tenantID); err != nil {
			return fmt.Errorf("bank: %w", err)
		}

		return nil
	})

	return got, err
}

// seedBank fills the question bank with a few reusable questions, so an author
// signing in finds something to add to a quiz.
func seedBank(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) error {
	type bq struct {
		category, prompt, right, wrong string
	}
	items := []bq{
		{"Mathematics", "Which is a prime number?", "7", "9"},
		{"Mathematics", "What is 12 × 12?", "144", "124"},
		{"History", "Where was the House of Wisdom?", "Baghdad", "Cairo"},
		{"Astronomy", "Which instrument measures a star's altitude?", "Astrolabe", "Compass"},
	}
	var questions, options [][]any
	for i, it := range items {
		qid := id("bank", tenantID, i)
		questions = append(questions, []any{qid, tenantID, "single_choice", it.prompt, 1, it.category})
		options = append(options,
			[]any{id("bankopt", qid, 0), tenantID, qid, it.right, 0, true},
			[]any{id("bankopt", qid, 1), tenantID, qid, it.wrong, 1, false},
		)
	}
	for _, c := range []struct {
		table string
		cols  []string
		rows  [][]any
	}{
		{"bank_questions", []string{"id", "tenant_id", "type", "prompt", "points", "category"}, questions},
		{"bank_question_options", []string{"id", "tenant_id", "bank_question_id", "content", "position", "is_correct"}, options},
	} {
		if err := bulkInsert(ctx, tx, c.table, c.cols, c.rows); err != nil {
			return err
		}
	}
	return nil
}

// seedGamification derives points and badges from the progress already seeded,
// rather than replaying the award logic — a completed lesson is worth its points
// whether a learner or the seeder recorded it. One pass of set-based SQL, so it
// costs the same whatever the row count.
func seedGamification(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) error {
	// Points: ten per completed lesson, a hundred per completed course.
	if _, err := tx.Exec(ctx, `
		INSERT INTO user_points (tenant_id, user_id, points)
		SELECT tenant_id, user_id, SUM(points) FROM (
			SELECT tenant_id, user_id, lessons_completed * 10 AS points
			FROM course_progress WHERE tenant_id = $1
			UNION ALL
			SELECT tenant_id, user_id, 100 AS points
			FROM enrolments WHERE tenant_id = $1 AND status = 'completed'
		) x
		GROUP BY tenant_id, user_id
		HAVING SUM(points) > 0
		ON CONFLICT (tenant_id, user_id) DO UPDATE SET points = EXCLUDED.points`,
		tenantID); err != nil {
		return err
	}

	// Badges, from the same seeded state.
	badges := []struct {
		badge, from string
	}{
		{"first_lesson", `SELECT DISTINCT tenant_id, user_id FROM course_progress WHERE tenant_id = $1 AND lessons_completed > 0`},
		{"graduate", `SELECT DISTINCT tenant_id, user_id FROM enrolments WHERE tenant_id = $1 AND status = 'completed'`},
		{"honor_roll", `SELECT tenant_id, user_id FROM enrolments WHERE tenant_id = $1 AND status = 'completed' GROUP BY tenant_id, user_id HAVING count(*) >= 5`},
	}
	for _, b := range badges {
		if _, err := tx.Exec(ctx, `
			INSERT INTO user_badges (tenant_id, user_id, badge)
			SELECT tenant_id, user_id, '`+b.badge+`' FROM ( `+b.from+` ) s
			ON CONFLICT (tenant_id, user_id, badge) DO NOTHING`,
			tenantID); err != nil {
			return err
		}
	}
	return nil
}

type communityCounts struct {
	spaces, threads, posts, notifications int
}

// seedCommunity fills the forum: a workspace-wide board and one course board,
// each with a few threads and replies — including one thread a demo learner
// started and the instructor answered, so signing in as student@ shows a live
// community and a reply notification.
func seedCommunity(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, catalogue []seededCourse, people people, rng *rand.Rand) (communityCounts, error) {
	var (
		got     communityCounts
		spaces  [][]any
		threads [][]any
		posts   [][]any
		notifs  [][]any
		now     = time.Now()
	)

	// A pool of authors: a learner to lead threads, the instructor, and a few
	// students. The named demo accounts only exist in the first workspace, so a
	// later workspace has none — fall back to a real student there.
	student := people.instructor
	if len(people.demo) > 0 {
		student = people.demo[len(people.demo)-1] // the last named account is student@
	} else if len(people.students) > 0 {
		student = people.students[0]
	}
	authors := append([]uuid.UUID{student, people.instructor}, people.students[:min(4, len(people.students))]...)
	author := func(i int) uuid.UUID { return authors[i%len(authors)] }

	// The workspace board.
	general := id("space", "general", tenantID)
	spaces = append(spaces, []any{general, tenantID, nil, "General discussion", "Introduce yourself and ask anything.", 0})
	got.spaces++

	// One course board, on the first course.
	if len(catalogue) > 0 {
		course := catalogue[0]
		cspace := id("space", "course", course.id)
		spaces = append(spaces, []any{cspace, tenantID, course.id, course.title + " — discussion", "For learners taking this course.", 1})
		got.spaces++
	}

	// A thread helper: writes a thread and n replies, keeping the roll-up honest.
	addThread := func(space uuid.UUID, key string, authorID uuid.UUID, title, body string, pinned bool, replies int, at time.Time) uuid.UUID {
		tid := id("thread", space, key)
		last := at
		for r := range replies {
			rt := at.Add(time.Duration(r+1) * 3 * time.Hour)
			posts = append(posts, []any{
				id("post", tid, r), tenantID, tid, author(r + 1),
				forumReplies[(int(tid[0])+r)%len(forumReplies)], rt,
			})
			last = rt
			got.posts++
		}
		threads = append(threads, []any{
			tid, tenantID, space, authorID, title, body, pinned, false, replies, last, at,
		})
		got.threads++
		return tid
	}

	addThread(general, "welcome", people.instructor,
		"Welcome to the community", "Say hello, share what you're studying, and be kind. Questions about a specific course belong on its board.",
		true, 3, now.Add(-20*24*time.Hour))

	// The demo learner's thread, answered by the instructor — the one that
	// produces a notification for student@.
	studyThread := id("thread", general, "study")
	studyAt := now.Add(-4 * 24 * time.Hour)
	replyAt := studyAt.Add(6 * time.Hour)
	threads = append(threads, []any{
		studyThread, tenantID, general, student, "Anyone up for a study group?",
		"I'm working through the first few courses and would love to compare notes. Reply if you're keen.",
		false, false, 1, replyAt, studyAt,
	})
	got.threads++
	posts = append(posts, []any{
		id("post", studyThread, 0), tenantID, studyThread, people.instructor,
		"Great idea — I'll pin a schedule once a few of you have signed up.", replyAt,
	})
	got.posts++
	notifs = append(notifs, []any{
		id("notification", "forum", studyThread), tenantID, student, "reply",
		"New reply in your thread", "Great idea — I'll pin a schedule once a few of you have signed up.",
		"/forum/threads/" + studyThread.String(), nil, replyAt,
	})
	got.notifications++

	if len(catalogue) > 0 {
		cspace := id("space", "course", catalogue[0].id)
		addThread(cspace, "q1", author(2), "Stuck on the second topic", "Did anyone else find the jump in the second topic steep? How did you approach it?", false, 2, now.Add(-8*24*time.Hour))
	}

	copies := []struct {
		table string
		cols  []string
		rows  [][]any
	}{
		{"forum_spaces", []string{"id", "tenant_id", "course_id", "title", "description", "position"}, spaces},
		{"forum_threads", []string{"id", "tenant_id", "space_id", "author_id", "title", "body", "pinned", "locked", "reply_count", "last_activity_at", "created_at"}, threads},
		{"forum_posts", []string{"id", "tenant_id", "thread_id", "author_id", "body", "created_at"}, posts},
		{"notifications", []string{"id", "tenant_id", "user_id", "kind", "title", "body", "link", "read_at", "created_at"}, notifs},
	}
	for _, c := range copies {
		if err := bulkInsert(ctx, tx, c.table, c.cols, c.rows); err != nil {
			return communityCounts{}, err
		}
	}
	return got, nil
}

// ---------------------------------------------------------------------- people

type people struct {
	owner      uuid.UUID
	instructor uuid.UUID
	students   []uuid.UUID
	everyone   []uuid.UUID

	// The named accounts a person actually signs in as. Kept out of `students` so
	// their enrolments are dealt by hand rather than by a coin toss — a demo
	// dashboard that is empty because the dice said so is a demo of nothing.
	demo []uuid.UUID

	// Every account's name, by id. A certificate copies the name in, and the
	// seeder is the transaction that would have read it.
	names map[uuid.UUID]string
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
	named := map[string]bool{}

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
			accounts = append(accounts, account{id("user", seat.email), seat.email, seat.name, seat.role})
			named[seat.email] = true
		}
	} else {
		owner := fmt.Sprintf("owner-%d@muallim.test", index)
		instructor := fmt.Sprintf("instructor-%d@muallim.test", index)

		accounts = append(accounts,
			account{id("user", owner), owner, scholars[0], auth.RoleOwner},
			account{id("user", instructor), instructor, scholars[1], auth.RoleInstructor},
		)
	}

	for student := range howMany {
		email := fmt.Sprintf("learner-%d-%d@muallim.test", index, student)
		accounts = append(accounts, account{
			id:    id("user", email),
			email: email,
			name:  scholars[student%len(scholars)],
			role:  auth.RoleStudent,
		})
	}

	users := make([][]any, 0, len(accounts))
	memberships := make([][]any, 0, len(accounts))
	verified := time.Now().Add(-30 * 24 * time.Hour)

	p.names = make(map[uuid.UUID]string, len(accounts))

	for _, a := range accounts {
		users = append(users, []any{a.id, a.email, hash, a.name, verified})
		memberships = append(memberships, []any{id("membership", tenantID, a.email), tenantID, a.id, a.role, "active"})

		p.names[a.id] = a.name
		p.everyone = append(p.everyone, a.id)
		if named[a.email] {
			p.demo = append(p.demo, a.id)
		}

		switch a.role {
		case auth.RoleOwner:
			p.owner = a.id
		case auth.RoleInstructor:
			if p.instructor == uuid.Nil {
				p.instructor = a.id
			}
		case auth.RoleStudent:
			if !named[a.email] {
				p.students = append(p.students, a.id)
			}
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
	title   string
	lessons []uuid.UUID
	quiz    *seededQuiz

	// The assignment that ends the course, when it has one. Its id and worth feed
	// the gradebook item; there are no submissions, so no grades against it.
	assignment       bool
	assignmentID     uuid.UUID
	assignmentLesson uuid.UUID
	assignmentPoints int
}

func seedCatalogue(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, cfg config, instructor uuid.UUID, rng *rand.Rand) ([]seededCourse, error) {
	var (
		courses     [][]any
		topics      [][]any
		lessons     [][]any
		quizzes     [][]any
		question    [][]any
		options     [][]any
		assignments [][]any
		gradeItems  [][]any
	)

	seeded := make([]seededCourse, 0, cfg.courses)
	now := time.Now()

	// How many assignments have been dealt so far. It picks the deadline, so the
	// three states a learner can be in — open, overdue, no deadline at all — are
	// all on the dashboard rather than all up to the dice.
	assigned := 0

	for index := range cfg.courses {
		// The title list is shorter than the catalogue, so a course past the end of it
		// takes a part number. Two courses with the same name on one dashboard is a
		// dashboard nobody can navigate — and it is the kind of thing only a realistic
		// volume of data ever shows you.
		title := courseTitles[index%len(courseTitles)]
		if part := index / len(courseTitles); part > 0 {
			title = fmt.Sprintf("%s, Part %s", title, roman[part%len(roman)])
		}

		slug := fmt.Sprintf("%s-%d", slugify(title), index)
		course := seededCourse{id: id("course", tenantID, slug), slug: slug, title: title}

		// One course in six stays a draft. A catalogue where everything is published
		// never exercises the query filter that hides the rest.
		status, published := "published", any(now.Add(-time.Duration(index)*24*time.Hour))
		if index%6 == 5 {
			status, published = "draft", nil
		}

		drip := []string{"none", "none", "none", "scheduled", "after_enrolment", "sequential"}[index%6]

		courses = append(courses, []any{
			course.id, tenantID, course.slug, title,
			courseSummaries[index%len(courseSummaries)],
			[]string{"beginner", "intermediate", "advanced", "expert"}[index%4],
			status, published, drip,
			courseDescriptions[index%len(courseDescriptions)],
			courseObjectives[index%len(courseObjectives)],
			courseRequirements[index%len(courseRequirements)],
			[]string{"en", "en", "en", "ar"}[index%4],
			instructor,
		})

		// Courses vary in size, so a catalogue shows a spread of lesson counts
		// rather than the same number on every card. Both draws come from the
		// course's own deterministic rng, so a reseed reproduces the exact shape;
		// the flags set the ceiling of each range, not a fixed count.
		topicCount := 2 + rng.IntN(cfg.topics)

		lastTopicLessons := 0
		for t := range topicCount {
			topicID := id("topic", course.id, t)
			topics = append(topics, []any{topicID, tenantID, course.id, topicTitles[t%len(topicTitles)], t})

			lessonCount := 3 + rng.IntN(cfg.lessons)
			lastTopicLessons = lessonCount
			for l := range lessonCount {
				lessonID := id("lesson", topicID, l)
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

		// Whatever the last topic ends with sits after the lessons already in it.
		// Positions are dense, so they are counted rather than assumed.
		lastTopic := topics[len(topics)-1][0].(uuid.UUID)
		tail := lastTopicLessons

		// Half the courses end with a quiz, on a lesson of their own — and the first
		// always does, because the demo accounts finish it, and a finished course
		// with no assessment is a gradebook with nothing in it.
		if index == 0 || rng.Float64() < cfg.quizRate {
			quizLesson := id("lesson", course.id, "quiz")

			lessons = append(lessons, []any{
				quizLesson, tenantID, lastTopic, "End-of-course quiz",
				"quiz", 600, false, tail,
				"", "none", "", "",
			})
			tail++
			course.lessons = append(course.lessons, quizLesson)

			quiz := &seededQuiz{id: id("quiz", course.id), lessonID: quizLesson, passing: 60}
			quizzes = append(quizzes, []any{
				quiz.id, tenantID, quizLesson, "End-of-course quiz",
				"Everything from the four sections above.", 0, 3, quiz.passing,
			})

			for q := range 5 {
				item := seededQuestion{id: id("question", quiz.id, q), kind: assess.TypeSingleChoice, points: 2}

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
						optionID := id("option", item.id, o)
						if o == correctAt {
							item.correct = optionID
						}
						options = append(options, []any{
							optionID, tenantID, item.id, optionContents[o], o, o == correctAt,
							id("match", item.id, o), "",
						})
					}
				}

				quiz.questions = append(quiz.questions, item)
			}

			course.quiz = quiz

			// The gradebook item for the quiz. One per assessment, worth what the
			// assessment is worth, written when the assessment is — not when the first
			// learner is graded, or a course with an unmarked quiz would claim it has
			// nothing to grade.
			gradeItems = append(gradeItems, []any{
				id("grade_item", "quiz", quiz.id), tenantID, course.id, quizLesson,
				"quiz", quiz.id, "End-of-course quiz", quiz.maxPoints,
			})
		}

		/*
			Some courses end with work to hand in.

			The assignment is seeded; no submission is. A submission's files live in an
			object store this command cannot reach — it holds a database connection and
			nothing else — and a row pointing at a key with no object behind it is a
			download that 404s, a marking queue full of work nobody can open, and a demo
			that lies about the one thing this feature does. `student@` uploads a real
			file in a second, which is the honest version of the same thing.

			Never on the first course, and always on the second and third. The first is
			the one the demo accounts finish outright, and a course at 100% whose last
			lesson is work nobody handed in is a course claiming something untrue. The
			next two are the ones they are part-way through, so whoever signs in has an
			assignment open in front of them rather than whatever the dice said.
		*/
		if index != 0 && (index <= 2 || rng.Float64() < cfg.assignmentRate) {
			handInLesson := id("lesson", course.id, "assignment")
			title := assignmentTitles[assigned%len(assignmentTitles)]

			lessons = append(lessons, []any{
				handInLesson, tenantID, lastTopic, title,
				"assignment", 3600, false, tail,
				"", "none", "", "",
			})
			tail++
			course.lessons = append(course.lessons, handInLesson)

			// Open, overdue, and no deadline, in that order. Late work is accepted in
			// every case: a demo whose only button is disabled demonstrates nothing.
			// The page says "was due", the submission is flagged late, and the marking
			// queue tells the marker so.
			var dueAt any
			switch assigned % 3 {
			case 0:
				dueAt = now.Add(14 * 24 * time.Hour)
			case 1:
				dueAt = now.Add(-3 * 24 * time.Hour)
			}

			assignmentID := id("assignment", course.id)
			assignments = append(assignments, []any{
				assignmentID, tenantID, handInLesson, title,
				assignmentBrief, 100, 60, 3, int64(25 << 20), dueAt, true,
			})

			// A gradebook item, but no grades: submissions live in an object store this
			// command cannot reach, so nobody has handed anything in. The item shows the
			// assignment is out there to be marked, which is the truth.
			gradeItems = append(gradeItems, []any{
				id("grade_item", "assignment", assignmentID), tenantID, course.id, handInLesson,
				"assignment", assignmentID, title, 100,
			})

			course.assignment = true
			course.assignmentID = assignmentID
			course.assignmentLesson = handInLesson
			course.assignmentPoints = 100
			assigned++
		}

		seeded = append(seeded, course)
	}

	copies := []struct {
		table string
		cols  []string
		rows  [][]any
	}{
		{"courses", []string{"id", "tenant_id", "slug", "title", "summary", "difficulty", "status", "published_at", "drip_mode", "description", "objectives", "requirements", "language", "created_by"}, courses},
		{"topics", []string{"id", "tenant_id", "course_id", "title", "position"}, topics},
		{"lessons", []string{"id", "tenant_id", "topic_id", "title", "content_type", "duration_seconds", "is_preview", "position", "content", "video_source", "video_url", "video_embed_url"}, lessons},
		{"quizzes", []string{"id", "tenant_id", "lesson_id", "title", "description", "time_limit_seconds", "max_attempts", "passing_percent"}, quizzes},
		{"questions", []string{"id", "tenant_id", "quiz_id", "type", "prompt", "points", "position", "explanation", "case_sensitive", "accepted"}, question},
		{"question_options", []string{"id", "tenant_id", "question_id", "content", "position", "is_correct", "match_id", "match_content"}, options},
		{"assignments", []string{"id", "tenant_id", "lesson_id", "title", "instructions", "points", "passing_points", "max_files", "max_bytes", "due_at", "allow_late"}, assignments},
		{"grade_items", []string{"id", "tenant_id", "course_id", "lesson_id", "source", "source_id", "title", "max_points"}, gradeItems},
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
	grades, certificates                    int
	reviews, discussions, announcements     int
	notifications                           int
}

func seedLearning(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, cfg config, catalogue []seededCourse, students, demo []uuid.UUID, instructor uuid.UUID, names map[uuid.UUID]string, rng *rand.Rand) (learningCounts, error) {
	var got learningCounts

	var (
		enrolments    [][]any
		progress      [][]any
		rollups       [][]any
		attempts      [][]any
		answers       [][]any
		gradeEntries  [][]any
		certificates  [][]any
		reviews       [][]any
		questions     [][]any
		qaAnswers     [][]any
		announcements [][]any
		notifications [][]any
	)

	now := time.Now()

	/*
		The named accounts get a hand-dealt hand.

		Whoever signs in as demo@ must land on a dashboard with something on it — a
		course nearly done, a course just begun, a course untouched — so the page can
		be judged. Leaving it to `enrolRate` means the owner's dashboard is empty
		whenever the dice say so, which is most of the time.
	*/
	demoShare := []float64{1.0, 0.6, 0.15, 0}
	for _, person := range demo {
		for i, course := range catalogue {
			if i >= len(demoShare) {
				break
			}

			done := int(float64(len(course.lessons)) * demoShare[i])
			enrolledAt := now.Add(-time.Duration(20+i*10) * 24 * time.Hour)

			status := enroll.StatusActive
			var completedAt any
			if done == len(course.lessons) {
				status = enroll.StatusCompleted
				completedAt = enrolledAt.Add(9 * 24 * time.Hour)
			}

			enrolments = append(enrolments, []any{
				id("enrolment", course.id, person), tenantID, course.id, person,
				status, enroll.SourceSelf, enrolledAt, completedAt,
			})
			got.enrolments++

			for l := range done {
				progress = append(progress, []any{
					id("progress", person, course.lessons[l]), tenantID, person, course.lessons[l], course.id,
					enrolledAt.Add(time.Duration(l) * 12 * time.Hour), 240 + rng.IntN(400),
				})
				got.progress++
			}

			percent := 0
			if len(course.lessons) > 0 {
				percent = done * 100 / len(course.lessons)
			}
			rollups = append(rollups, []any{tenantID, person, course.id, done, len(course.lessons), percent})

			// On their in-progress course, a named demo account has asked a question the
			// instructor answered — so signing in as demo@ or student@ lands on a bell
			// with something in it, and a lesson with a live discussion. Left unread.
			if i == 1 && len(course.lessons) > 0 {
				qID := id("question", "demo", course.id, person)
				askedAt := enrolledAt.Add(3 * 24 * time.Hour)
				answerBody := discussionAnswers[i%len(discussionAnswers)]
				questions = append(questions, []any{
					qID, tenantID, course.lessons[0], person,
					discussionQuestions[i%len(discussionQuestions)], askedAt,
				})
				qaAnswers = append(qaAnswers, []any{
					id("answer", "demo", qID), tenantID, qID, instructor,
					answerBody, true, askedAt.Add(5 * time.Hour),
				})
				got.discussions++
				notifications = append(notifications, []any{
					id("notification", "demo", qID), tenantID, person, notify.KindAnswer,
					"New answer to your question", answerBody,
					"/courses/" + course.slug + "/lessons/" + course.lessons[0].String(),
					nil, askedAt.Add(5 * time.Hour),
				})
				got.notifications++
			}

			// A finished course earns a certificate and, if it has a quiz, a mark on
			// it. The demo accounts do not sit quizzes elsewhere in the seed, so this is
			// where `student@`'s own grades page gets something to show.
			if status == enroll.StatusCompleted {
				certificates = append(certificates,
					certificateRow(tenantID, course, person, names[person], completedAt))
				got.certificates++

				if course.quiz != nil {
					submitted := enrolledAt.Add(7 * 24 * time.Hour)
					attempt, answerRows, points := gradedQuizAttempt(tenantID, course.quiz, person, submitted, false, true, rng)
					attempts = append(attempts, attempt)
					got.attempts++
					answers = append(answers, answerRows...)
					got.answers += len(answerRows)
					gradeEntries = append(gradeEntries, gradeEntryRow(tenantID, course.quiz, person, points))
					got.grades++

					// A "your quiz was graded" bell, the way a marked essay produces one,
					// so a demo account's notifications show a grade too. Left read — it
					// is old news by the time anyone signs in.
					notifications = append(notifications, []any{
						id("notification", "grade", course.id, person), tenantID, person, notify.KindGrade,
						"Your quiz was graded", fmt.Sprintf("You scored %d of %d.", points, course.quiz.maxPoints),
						"/courses/" + course.slug + "/grades", submitted.Add(time.Hour), submitted.Add(time.Hour),
					})
					got.notifications++
				}
			}
		}
	}

	for _, course := range catalogue {
		// One or two notices per course, pinned by the instructor. Announcements are
		// read by whoever may see the course, so they land on the course page.
		for a := range 1 + rng.IntN(2) {
			posted := now.Add(-time.Duration(3+a*5) * 24 * time.Hour)
			headline := announcementHeadlines[(int(course.id[0])+a)%len(announcementHeadlines)]
			announcements = append(announcements, []any{
				id("announcement", course.id, a), tenantID, course.id, instructor,
				headline.title, headline.body, posted,
			})
			got.announcements++
		}

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
				id("enrolment", course.id, student), tenantID, course.id, student, status,
				enroll.SourceSelf, enrolledAt, completedAt,
			})
			got.enrolments++

			for i, lesson := range course.lessons {
				if i >= done {
					break
				}
				progress = append(progress, []any{
					id("progress", student, lesson), tenantID, student, lesson, course.id,
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

			// A certificate for whoever finished the course, in the same spirit as the
			// completion it records.
			if status == enroll.StatusCompleted {
				certificates = append(certificates,
					certificateRow(tenantID, course, student, names[student], completedAt))
				got.certificates++
			}

			// A learner who has got somewhere in the course may leave a review. Ratings
			// lean high — most people who bother to rate a course they stuck with liked
			// it — so the average is a believable 4-ish, not a flat 3.
			if done > 0 && rng.Float64() < 0.45 {
				rating := 3 + rng.IntN(3) // 3..5
				if rng.Float64() < 0.15 {
					rating = 1 + rng.IntN(2) // an occasional 1..2 keeps it honest
				}
				body := reviewBodies[rng.IntN(len(reviewBodies))]
				reviewedAt := enrolledAt.Add(time.Duration(done) * 24 * time.Hour)
				reviews = append(reviews, []any{
					id("review", course.id, student), tenantID, course.id, student,
					rating, body, reviewedAt,
				})
				got.reviews++
			}

			// Some enrolled learners ask a question on the first lesson, and the
			// instructor answers it — so a lesson's discussion is not always empty.
			if len(course.lessons) > 0 && rng.Float64() < 0.2 {
				qID := id("question", "discussion", course.id, student)
				askedAt := enrolledAt.Add(2 * 24 * time.Hour)
				answerBody := discussionAnswers[rng.IntN(len(discussionAnswers))]
				answeredAt := askedAt.Add(6 * time.Hour)
				questions = append(questions, []any{
					qID, tenantID, course.lessons[0], student,
					discussionQuestions[rng.IntN(len(discussionQuestions))], askedAt,
				})
				qaAnswers = append(qaAnswers, []any{
					id("answer", "discussion", qID), tenantID, qID, instructor,
					answerBody, true, answeredAt,
				})
				got.discussions++

				// The answer notifies the asker, the way the running app does. A few are
				// left unread so a demo bell has a number on it.
				readAt := any(answeredAt.Add(2 * time.Hour))
				if rng.Float64() < 0.4 {
					readAt = nil
				}
				notifications = append(notifications, []any{
					id("notification", "answer", qID), tenantID, student, notify.KindAnswer,
					"New answer to your question", answerBody,
					"/courses/" + course.slug + "/lessons/" + course.lessons[0].String(),
					readAt, answeredAt,
				})
				got.notifications++
			}

			if course.quiz == nil || rng.Float64() >= cfg.attemptRate {
				continue
			}

			submitted := enrolledAt.Add(time.Duration(rng.IntN(60)) * 24 * time.Hour)

			// One attempt in four is still waiting for a person, so the marking queue
			// is never empty. The rest are graded, and a graded attempt is a grade.
			awaiting := rng.Float64() < 0.25

			attempt, answerRows, points := gradedQuizAttempt(tenantID, course.quiz, student, submitted, awaiting, false, rng)
			attempts = append(attempts, attempt)
			got.attempts++
			answers = append(answers, answerRows...)
			got.answers += len(answerRows)

			if !awaiting {
				gradeEntries = append(gradeEntries, gradeEntryRow(tenantID, course.quiz, student, points))
				got.grades++
			}
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
		{"grade_entries", []string{"id", "tenant_id", "grade_item_id", "user_id", "points", "max_points"}, gradeEntries},
		{"certificates", []string{"id", "tenant_id", "course_id", "user_id", "serial", "learner_name", "course_title", "issued_at", "title", "body", "signatory"}, certificates},
		{"announcements", []string{"id", "tenant_id", "course_id", "author_id", "title", "body", "created_at"}, announcements},
		{"course_reviews", []string{"id", "tenant_id", "course_id", "user_id", "rating", "body", "created_at"}, reviews},
		{"lesson_questions", []string{"id", "tenant_id", "lesson_id", "author_id", "body", "created_at"}, questions},
		{"lesson_answers", []string{"id", "tenant_id", "question_id", "author_id", "body", "by_instructor", "created_at"}, qaAnswers},
		{"notifications", []string{"id", "tenant_id", "user_id", "kind", "title", "body", "link", "read_at", "created_at"}, notifications},
	}

	for _, c := range copies {
		if err := bulkInsert(ctx, tx, c.table, c.cols, c.rows); err != nil {
			return got, err
		}
	}

	return got, nil
}

// gradedQuizAttempt builds one attempt over a quiz, its answers, and the total.
//
// Extracted so the student loop and the demo loop share it rather than diverge:
// two copies of the same grading are two chances to seed a score the gradebook
// then disagrees with. `awaiting` leaves the essay unmarked, and the attempt in
// the marking queue.
func gradedQuizAttempt(tenantID uuid.UUID, quiz *seededQuiz, student uuid.UUID, submitted time.Time, awaiting, best bool, rng *rand.Rand) ([]any, [][]any, int) {
	attemptID := id("attempt", quiz.id, student)
	answerRows := make([][]any, 0, len(quiz.questions))
	var points int

	for _, item := range quiz.questions {
		switch {
		case item.kind == assess.TypeOpenEnded && awaiting:
			answerRows = append(answerRows, []any{
				id("answer", attemptID, item.id), tenantID, attemptID, item.id,
				`{"text":"An answer of some length, written under time pressure."}`,
				false, false, 0, "", nil,
			})

		case item.kind == assess.TypeOpenEnded:
			awarded := rng.IntN(item.points + 1)
			if best {
				awarded = item.points
			}
			points += awarded
			answerRows = append(answerRows, []any{
				id("answer", attemptID, item.id), tenantID, attemptID, item.id,
				`{"text":"An answer of some length, written under time pressure."}`,
				true, awarded == item.points, awarded, "Good, though you never define the term.", submitted,
			})

		default:
			// Two learners in three pick the right option — but a learner who went on to
			// finish the course got this one right.
			right := best || rng.Float64() < 0.66
			chosen := item.correct
			if !right {
				// An option belonging to nothing: a wrong answer. Derived under its own
				// prefix, so it can never accidentally name a real option.
				chosen = id("wrong", attemptID, item.id)
			}
			awarded := 0
			if right {
				awarded = item.points
			}
			points += awarded

			answerRows = append(answerRows, []any{
				id("answer", attemptID, item.id), tenantID, attemptID, item.id,
				fmt.Sprintf(`{"choices":["%s"]}`, chosen),
				true, right, awarded, "", submitted,
			})
		}
	}

	status := assess.StatusGraded
	var graded, passed any = submitted, points*100 >= quiz.passing*quiz.maxPoints
	if awaiting {
		status, graded, passed = assess.StatusAwaitingReview, nil, nil
	}

	attempt := []any{
		attemptID, tenantID, quiz.id, student, 1, status,
		submitted.Add(-30 * time.Minute), submitted, graded, nil,
		points, quiz.maxPoints, passed,
	}
	return attempt, answerRows, points
}

// gradeEntryRow is one learner's mark on a quiz, for the gradebook. `max_points`
// is copied from the quiz as it stands, the same as the application copies it.
func gradeEntryRow(tenantID uuid.UUID, quiz *seededQuiz, student uuid.UUID, points int) []any {
	return []any{
		id("grade_entry", "quiz", quiz.id, student), tenantID,
		id("grade_item", "quiz", quiz.id), student, points, quiz.maxPoints,
	}
}

// certificateRow is the certificate a finished course issues.
//
// The learner's name and the course title are copied in, exactly as the domain
// copies them: a certificate is a record of an event, and the event does not
// change when a name or a title later does. The serial is derived from the row's
// identity, so a reseed issues the same number and a bookmarked certificate URL
// survives.
func certificateRow(tenantID uuid.UUID, course seededCourse, student uuid.UUID, name string, issuedAt any) []any {
	certID := id("certificate", course.id, student)
	serial := seedSerial(certID)
	template := certify.DefaultTemplate()

	when, _ := issuedAt.(time.Time)
	body := certify.Render(template.Body, certify.Fields{
		Learner: name, Course: course.title,
		Date: when.Format(certify.DateFormat), Serial: serial,
	})

	return []any{
		certID, tenantID, course.id, student, serial,
		name, course.title, issuedAt, template.Title, body, "",
	}
}

// seedSerial is a plausible, unguessable certificate number derived from a uuid.
//
// Not certify.NewSerial: that draws from crypto/rand, and a seed that cannot be
// reproduced is a seed whose URLs break on every reseed. Uppercase hex has no
// letter a reader misreads for a digit, so it needs no special alphabet.
func seedSerial(u uuid.UUID) string {
	hex := strings.ToUpper(strings.ReplaceAll(u.String(), "-", ""))[:16]
	return fmt.Sprintf("%s-%s-%s-%s-%s", certify.SerialPrefix, hex[0:4], hex[4:8], hex[8:12], hex[12:16])
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

// databaseName is the database a URL points at, and nothing else from the URL.
// The password lives in there too, and a seeder that prints one has put a secret
// in a terminal, a scrollback, and whatever CI keeps.
func databaseName(url string) string {
	parsed, err := neturl.Parse(url)
	if err != nil {
		return "?"
	}
	return strings.TrimPrefix(parsed.Path, "/")
}

func report(cfg config, url string, got counts, took time.Duration) {
	rows := got.users + got.courses + got.lessons + got.quizzes + got.questions +
		got.assignments + got.enrolments + got.progress + got.attempts + got.answers +
		got.grades + got.certificates + got.reviews + got.discussions + got.announcements + got.notifications + got.spaces + got.threads + got.posts

	fmt.Printf("seeded %d rows in %s\n\n", rows, took.Round(time.Millisecond))
	fmt.Printf("  workspaces  %d\n  people      %d\n  courses     %d\n  lessons     %d\n",
		got.tenants, got.users, got.courses, got.lessons)
	fmt.Printf("  quizzes     %d (%d questions)\n  assignments %d\n  enrolments  %d\n  progress    %d\n",
		got.quizzes, got.questions, got.assignments, got.enrolments, got.progress)
	fmt.Printf("  attempts    %d (%d answers)\n  grades      %d\n  certificates %d\n",
		got.attempts, got.answers, got.grades, got.certificates)
	fmt.Printf("  reviews     %d\n  discussions %d\n  announcements %d\n  notifications %d\n",
		got.reviews, got.discussions, got.announcements, got.notifications)
	fmt.Printf("  forum       %d spaces, %d threads, %d posts\n\n",
		got.spaces, got.threads, got.posts)

	// Which database, because these accounts are only in this one. `pnpm test:e2e`
	// runs against `muallim_test`, which has no demo accounts at all — and an API left
	// pointing there answers "those credentials are not valid" to every one of them.
	fmt.Printf("written to %s. sign in at http://localhost:5173 — every account shares one password\n\n",
		databaseName(url))
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

	// The landing page's copy, in the same order as the summaries above: a pitch
	// long enough to need a "show more", and lists long enough to wrap two columns.
	courseDescriptions = []string{
		"Al-Khwarizmi did not invent the equation. He invented the idea that an equation " +
			"could be solved by a procedure — the same steps, in the same order, whoever is " +
			"holding the pen. That is the idea this course is about.\n\n" +
			"We begin where he began: with completion and balancing, the two moves that turn " +
			"a tangle into a form you already know how to solve. You will work the six canonical " +
			"cases by hand, and you will prove each one geometrically, because a procedure you " +
			"cannot picture is a procedure you will misapply.\n\n" +
			"By the end you will not be reciting the quadratic formula. You will be deriving it, " +
			"and you will understand why it was worth deriving.",

		"Ibn al-Haytham settled an argument that had run for a thousand years — does the eye " +
			"reach out to the world, or does the world arrive at the eye? — by doing something " +
			"nobody had thought to do. He built the apparatus and looked.\n\n" +
			"This course follows the Book of Optics as a working manual. You will darken a room, " +
			"admit a single ray, and measure what it does. Reflection, refraction, the camera " +
			"obscura, the anatomy of the eye: each is a claim, and each claim is one you will test.\n\n" +
			"The method is the subject. The optics is how we practise it.",

		"An astrolabe is a model of the sky you can hold. Learning to read one teaches you the " +
			"sky itself, because every line on the instrument answers to something overhead.\n\n" +
			"You will start with the celestial sphere and the coordinates that describe it, then " +
			"take an altitude, find your latitude, tell the hour by a star, and determine the " +
			"direction of prayer from any city on the plate. Then you will build one, in brass or " +
			"in card, and check it against the night.\n\n" +
			"Nothing here needs a telescope. Everything here needs patience and a clear evening.",

		"The Canon organised everything a physician of its age knew, and it did so well enough " +
			"to be taught in Europe for six hundred years. This course reads it the way it was " +
			"meant to be read: as a syllabus, not a relic.\n\n" +
			"We work from examination to diagnosis to treatment. You will learn the pulse and what " +
			"it was thought to reveal, the fevers and how they were distinguished, and the materia " +
			"medica — the plants, their preparations, their doses, and the honest limits of each.\n\n" +
			"Where the Canon is right, we will say why it is right. Where it is wrong, we will say " +
			"how it was found out. Both are how medicine learns.",
	}

	courseObjectives = [][]string{
		{
			"Solve any quadratic by completion and balancing, without reaching for a formula",
			"Derive the quadratic formula yourself, and explain each step to somebody else",
			"Prove an algebraic identity geometrically, the way al-Khwarizmi proved his",
			"Recognise the six canonical cases on sight, and know why there are exactly six",
			"Translate a word problem into a form that a procedure can finish",
			"Read a medieval mathematical argument without needing it modernised first",
		},
		{
			"Set up a camera obscura and predict what it will show before you look",
			"Measure the angle of refraction between two media, and account for what you measure",
			"State the intromission theory and give the experiment that decides it",
			"Trace a ray through the eye's structures and say where the image forms",
			"Design a controlled optical experiment: one variable, one claim, one result",
			"Tell a demonstration from an assertion in any scientific text you read",
		},
		{
			"Take the altitude of a star or the sun with an astrolabe, to within a degree",
			"Find your latitude from a single sighting, at night or at noon",
			"Tell the time by the stars, and know how far to trust the answer",
			"Determine the qibla from any city on the instrument's plate",
			"Build a working astrolabe and test it against a sky you have predicted",
			"Read the celestial sphere as coordinates rather than as pictures",
		},
		{
			"Take a history and an examination in the order the Canon lays down",
			"Distinguish the classical fevers by their described course, and say what each would be called today",
			"Prepare a simple from a plant you have grown, at a dose you can defend",
			"Read a pharmacological entry critically: what is observed, what is inferred, what is asserted",
			"Explain how a treatment that works for the wrong reason is eventually found out",
			"Trace one doctrine from the Canon to the moment medicine abandoned it",
		},
	}

	courseRequirements = [][]string{
		{
			"Arithmetic, and the patience to do it by hand",
			"No prior algebra. We build it from nothing, which is the point",
			"Paper. A great deal of paper",
		},
		{
			"A room you can darken completely",
			"Curiosity about how a claim is settled, rather than who settled it",
			"No physics beyond school. The geometry is drawn, not assumed",
		},
		{
			"A clear night, and somewhere to stand under it",
			"Basic geometry: angles, circles, and the confidence to draw both",
			"An astrolabe is provided as a printable plate. Brass is optional",
		},
		{
			"No medical training is assumed, and none is conferred",
			"A willingness to read a text that is sometimes wrong, and to find out where",
			"This is a history and philosophy of medicine. Do not treat anybody with it",
		},
	}

	roman = []string{"I", "II", "III", "IV", "V", "VI", "VII", "VIII", "IX", "X"}

	topicTitles = []string{"Foundations", "The method", "In practice", "Where it breaks", "Further reading"}

	lessonTitles = []string{"An introduction to", "Working through", "A closer look at", "Exercises on", "Notes on"}

	questionPrompts = []string{
		"Which of these follows from the definition given in section two?",
		"A merchant divides an estate into unequal shares. Which method applies?",
		"Which instrument measures the altitude of a star above the horizon?",
		"Which statement about refraction is true?",
		"Explain, in your own words, why the method matters more than the answer.",
	}

	reviewBodies = []string{
		"Clear from the first lesson. The worked examples are what made it click.",
		"Dense, but worth it. I came back to the middle section twice.",
		"Exactly the pace I needed — nothing skipped, nothing laboured.",
		"The instructor answers questions properly. That is rarer than it should be.",
		"Good throughout, though the last topic could use another example.",
		"I have taken three courses on this and only this one made me confident.",
		"Practical and honest about where the method breaks down.",
		"", // some reviewers rate without writing a word
	}

	discussionQuestions = []string{
		"In the second lesson, is the step from the definition to the lemma assumed, or is it proved somewhere?",
		"How does this apply when the remainder is not zero? The example only covers the clean case.",
		"Could you recommend something to read alongside this? I want to go deeper on the middle section.",
		"I follow the method but not why it works. Is there an intuition behind it?",
	}

	discussionAnswers = []string{
		"Good question — it is proved in the appendix to that lesson, which is easy to miss. I have added a note pointing to it.",
		"When the remainder is non-zero you carry it into the next step; the worked example in lesson three shows exactly that.",
		"Yes — the further-reading note at the end of the topic lists two. Start with the shorter one.",
		"The intuition is in the diagram: each step preserves the balance, so nothing is lost from one line to the next.",
	}

	forumReplies = []string{
		"Same here — the worked examples in the third lesson finally made it click for me.",
		"I'd start with the summary at the end and work backwards. It helped me see where it was going.",
		"Count me in. I usually study in the evenings, if that suits anyone.",
		"Thanks for starting this. I thought I was the only one finding it tricky.",
		"There's a good note in the further-reading section that covers exactly this.",
	}

	announcementHeadlines = []struct{ title, body string }{
		{"Welcome to the course", "Start with the first lesson and take your time. Ask questions on any lesson — I read them."},
		{"Live session this week", "I will hold an optional walkthrough of the middle topic. A recording follows for anyone who cannot attend."},
		{"New examples added", "I have added two worked examples to the section people asked about most. They are in the lesson notes."},
		{"A note on the assignment", "Read the brief twice before you start. Partial working earns partial credit, so show your steps."},
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

	assignmentTitles = []string{
		"Final assignment: a method of your own",
		"Final assignment: the proof, written out",
		"Final assignment: an instrument and its error",
		"Final assignment: a translation, and what it cost",
	}

	assignmentBrief = strings.TrimSpace(`
Choose one problem from the last section and solve it in full. Show the method,
not only the answer — a reader who disagrees with you should be able to find the
step where you parted company.

Hand in one file. A scan of handwriting is fine, provided it is legible; so is a
document. You may attach up to three files if you have working to include, and
each may be up to 25 MB.

You will be marked out of 100. Sixty is a pass, and a pass completes the course.`)
)

// Compile-time proof that the constants this file writes are the ones the domain
// packages recognise. A seeder that invents its own status strings is a seeder
// whose data the application will not read.
var (
	_ = catalog.StatusPublished
	_ = enroll.StatusActive
	_ = assess.StatusGraded
)
