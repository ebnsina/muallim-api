package academics_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/academics"
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
	sub := "t" + id.String()[:8]
	err := db.WithoutTenant(t.Context(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, subdomain, name) VALUES ($1, $2, $3)`,
			id, sub, "Test "+sub)
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

// seedUser creates a global user so a marked_by / actor foreign key resolves.
func seedUser(t *testing.T, db *database.DB) uuid.UUID {
	t.Helper()

	id := uuid.New()
	err := db.WithoutTenant(t.Context(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
			id, "u"+id.String()[:8]+"@example.test")
		return err
	})
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		_ = db.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM users WHERE id = $1`, id)
			return err
		})
	})
	return id
}

type stubAuditor struct{}

func (stubAuditor) Record(context.Context, pgx.Tx, uuid.UUID, academics.AuditEntry) error {
	return nil
}

func newService(db *database.DB) *academics.Service {
	return academics.NewService(db, academics.NewPostgresRepository(), stubAuditor{})
}

func date(s string) time.Time {
	d, _ := time.Parse("2006-01-02", s)
	return d
}

// The institution type defaults to school and can be set to one of the known kinds;
// an unknown kind is refused.
func TestInstitutionType(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)

	kind, err := svc.InstitutionType(t.Context(), tenant)
	if err != nil {
		t.Fatalf("read type: %v", err)
	}
	if kind != academics.TypeSchool {
		t.Fatalf("default type = %q, want school", kind)
	}

	if err := svc.SetInstitutionType(t.Context(), tenant, academics.TypeMadrasa); err != nil {
		t.Fatalf("set type: %v", err)
	}
	kind, _ = svc.InstitutionType(t.Context(), tenant)
	if kind != academics.TypeMadrasa {
		t.Fatalf("type after set = %q, want madrasa", kind)
	}

	if err := svc.SetInstitutionType(t.Context(), tenant, "prison"); !errors.Is(err, academics.ErrInvalidInstitutionType) {
		t.Fatalf("an unknown type was accepted: %v", err)
	}
}

// A workspace has at most one current year: setting a second moves the flag rather
// than raising two.
func TestAcademicYears(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)
	author := academics.Author{UserID: uuid.New()}

	y2025, err := svc.CreateYear(t.Context(), tenant,
		academics.NewAcademicYear{Name: "2025", StartsOn: date("2025-01-01"), EndsOn: date("2025-12-31")}, author)
	if err != nil {
		t.Fatalf("create 2025: %v", err)
	}
	y2026, err := svc.CreateYear(t.Context(), tenant,
		academics.NewAcademicYear{Name: "2026", StartsOn: date("2026-01-01"), EndsOn: date("2026-12-31")}, author)
	if err != nil {
		t.Fatalf("create 2026: %v", err)
	}

	// A duplicate name is refused.
	if _, err := svc.CreateYear(t.Context(), tenant,
		academics.NewAcademicYear{Name: "2025", StartsOn: date("2025-01-01"), EndsOn: date("2025-12-31")}, author); !errors.Is(err, academics.ErrNameTaken) {
		t.Fatalf("a duplicate year name was accepted: %v", err)
	}

	// An end before its start is refused.
	if _, err := svc.CreateYear(t.Context(), tenant,
		academics.NewAcademicYear{Name: "backwards", StartsOn: date("2025-12-31"), EndsOn: date("2025-01-01")}, author); !errors.Is(err, academics.ErrInvalidYear) {
		t.Fatalf("a year ending before it starts was accepted: %v", err)
	}

	if _, err := svc.SetCurrentYear(t.Context(), tenant, y2025.ID, author); err != nil {
		t.Fatalf("set 2025 current: %v", err)
	}
	if _, err := svc.SetCurrentYear(t.Context(), tenant, y2026.ID, author); err != nil {
		t.Fatalf("set 2026 current: %v", err)
	}

	years, err := svc.Years(t.Context(), tenant)
	if err != nil {
		t.Fatalf("list years: %v", err)
	}
	current := 0
	for _, y := range years {
		if y.IsCurrent {
			current++
			if y.ID != y2026.ID {
				t.Errorf("current year is %s, want 2026", y.Name)
			}
		}
	}
	if current != 1 {
		t.Fatalf("%d current years, want exactly 1", current)
	}
}

// Terms append in order; classes and sections nest and enforce their own name
// uniqueness.
func TestTermsClassesSections(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)
	author := academics.Author{UserID: uuid.New()}

	year, err := svc.CreateYear(t.Context(), tenant,
		academics.NewAcademicYear{Name: "2025", StartsOn: date("2025-01-01"), EndsOn: date("2025-12-31")}, author)
	if err != nil {
		t.Fatalf("create year: %v", err)
	}

	for _, name := range []string{"First Term", "Second Term"} {
		if _, err := svc.CreateTerm(t.Context(), tenant, year.ID,
			academics.NewTerm{Name: name, StartsOn: date("2025-01-01"), EndsOn: date("2025-06-30")}); err != nil {
			t.Fatalf("create term %s: %v", name, err)
		}
	}
	terms, err := svc.Terms(t.Context(), tenant, year.ID)
	if err != nil {
		t.Fatalf("list terms: %v", err)
	}
	if len(terms) != 2 || terms[0].Position != 0 || terms[1].Position != 1 {
		t.Fatalf("terms not appended in order: %+v", terms)
	}

	// A term against a year that does not exist is a not-found, not a stray row.
	if _, err := svc.CreateTerm(t.Context(), tenant, uuid.New(),
		academics.NewTerm{Name: "orphan", StartsOn: date("2025-01-01"), EndsOn: date("2025-06-30")}); !errors.Is(err, academics.ErrNotFound) {
		t.Fatalf("a term against a missing year was accepted: %v", err)
	}

	class, err := svc.CreateClass(t.Context(), tenant, academics.NewGradeLevel{Name: "Class 6", Rank: 6}, author)
	if err != nil {
		t.Fatalf("create class: %v", err)
	}
	if _, err := svc.CreateSection(t.Context(), tenant, class.ID, academics.NewSection{Name: "A", Capacity: 40}); err != nil {
		t.Fatalf("create section A: %v", err)
	}
	// A second section named A in the same class is refused.
	if _, err := svc.CreateSection(t.Context(), tenant, class.ID, academics.NewSection{Name: "A"}); !errors.Is(err, academics.ErrNameTaken) {
		t.Fatalf("a duplicate section name was accepted: %v", err)
	}
	sections, err := svc.Sections(t.Context(), tenant, class.ID)
	if err != nil {
		t.Fatalf("list sections: %v", err)
	}
	if len(sections) != 1 {
		t.Fatalf("%d sections, want 1", len(sections))
	}
}

// A student is admitted, appears on the roster, gains a guardian, and the admission
// number is unique.
func TestStudentsAndGuardians(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)
	author := academics.Author{UserID: uuid.New()}

	class, err := svc.CreateClass(t.Context(), tenant, academics.NewGradeLevel{Name: "Class 6", Rank: 6}, author)
	if err != nil {
		t.Fatalf("create class: %v", err)
	}

	amina, err := svc.AdmitStudent(t.Context(), tenant, academics.NewStudent{
		AdmissionNo: "2025-001", FullName: "Amina Rahman", GradeLevelID: &class.ID, Roll: 1,
	}, author)
	if err != nil {
		t.Fatalf("admit amina: %v", err)
	}
	if _, err := svc.AdmitStudent(t.Context(), tenant, academics.NewStudent{
		AdmissionNo: "2025-002", FullName: "Bilal Ahmed", GradeLevelID: &class.ID, Roll: 2,
	}, author); err != nil {
		t.Fatalf("admit bilal: %v", err)
	}

	// A duplicate admission number is refused.
	if _, err := svc.AdmitStudent(t.Context(), tenant, academics.NewStudent{
		AdmissionNo: "2025-001", FullName: "Someone Else",
	}, author); !errors.Is(err, academics.ErrAdmissionTaken) {
		t.Fatalf("a duplicate admission number was accepted: %v", err)
	}

	// The roster is sorted by name, and the grade filter narrows it.
	page, err := svc.Roster(t.Context(), tenant, academics.RosterFilter{GradeLevelID: &class.ID},
		academics.PageParams{Limit: 50})
	if err != nil {
		t.Fatalf("roster: %v", err)
	}
	if len(page.Students) != 2 || page.Students[0].FullName != "Amina Rahman" {
		t.Fatalf("roster not sorted by name: %+v", page.Students)
	}

	// A guardian, made primary, then a second: the primary flag is exclusive.
	if _, err := svc.AddGuardian(t.Context(), tenant, amina.ID, academics.NewGuardian{
		FullName: "Rahman Ali", Relation: "father", Phone: "01700000000", IsPrimary: true,
	}); err != nil {
		t.Fatalf("add guardian: %v", err)
	}
	if _, err := svc.AddGuardian(t.Context(), tenant, amina.ID, academics.NewGuardian{
		FullName: "Fatima Rahman", Relation: "mother", IsPrimary: true,
	}); err != nil {
		t.Fatalf("add second guardian: %v", err)
	}

	guardians, err := svc.Guardians(t.Context(), tenant, amina.ID)
	if err != nil {
		t.Fatalf("list guardians: %v", err)
	}
	if len(guardians) != 2 {
		t.Fatalf("%d guardians, want 2", len(guardians))
	}
	primaries := 0
	for _, g := range guardians {
		if g.IsPrimary {
			primaries++
			if g.FullName != "Fatima Rahman" {
				t.Errorf("primary is %s, want the most recent (Fatima)", g.FullName)
			}
		}
	}
	if primaries != 1 {
		t.Fatalf("%d primary guardians, want exactly 1", primaries)
	}

	// A guardian against a student that does not exist is a not-found.
	if _, err := svc.AddGuardian(t.Context(), tenant, uuid.New(), academics.NewGuardian{FullName: "Nobody"}); !errors.Is(err, academics.ErrNotFound) {
		t.Fatalf("a guardian against a missing student was accepted: %v", err)
	}
}

// A subject is added to the catalog, listed by name, and its name is unique.
func TestSubjects(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)

	if _, err := svc.CreateSubject(t.Context(), tenant, academics.NewSubject{Name: "Mathematics", Code: "MATH"}); err != nil {
		t.Fatalf("create subject: %v", err)
	}
	if _, err := svc.CreateSubject(t.Context(), tenant, academics.NewSubject{Name: "Bangla"}); err != nil {
		t.Fatalf("create bangla: %v", err)
	}

	// A duplicate name is refused.
	if _, err := svc.CreateSubject(t.Context(), tenant, academics.NewSubject{Name: "mathematics"}); !errors.Is(err, academics.ErrNameTaken) {
		t.Fatalf("a duplicate subject name was accepted: %v", err)
	}

	subjects, err := svc.Subjects(t.Context(), tenant)
	if err != nil {
		t.Fatalf("list subjects: %v", err)
	}
	if len(subjects) != 2 || subjects[0].Name != "Bangla" {
		t.Fatalf("subjects not sorted by name: %+v", subjects)
	}
}

func TestTimetable(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)
	author := academics.Author{UserID: uuid.New()}

	class, err := svc.CreateClass(t.Context(), tenant, academics.NewGradeLevel{Name: "Class 6", Rank: 6}, author)
	if err != nil {
		t.Fatalf("create class: %v", err)
	}
	section, err := svc.CreateSection(t.Context(), tenant, class.ID, academics.NewSection{Name: "A"})
	if err != nil {
		t.Fatalf("create section: %v", err)
	}

	// A slot ending before it starts is refused.
	if _, err := svc.AddPeriod(t.Context(), tenant, academics.NewPeriod{
		SectionID: section.ID, DayOfWeek: 0, StartsAt: "10:00", EndsAt: "09:00",
	}); !errors.Is(err, academics.ErrInvalidPeriod) {
		t.Fatalf("a backwards period was accepted: %v", err)
	}

	if _, err := svc.AddPeriod(t.Context(), tenant, academics.NewPeriod{
		SectionID: section.ID, DayOfWeek: 0, StartsAt: "09:00", EndsAt: "09:45", TeacherName: "Mr Karim", Room: "201",
	}); err != nil {
		t.Fatalf("add period: %v", err)
	}
	if _, err := svc.AddPeriod(t.Context(), tenant, academics.NewPeriod{
		SectionID: section.ID, DayOfWeek: 0, StartsAt: "10:00", EndsAt: "10:45",
	}); err != nil {
		t.Fatalf("add second period: %v", err)
	}

	// The same slot twice is a conflict, not a duplicate.
	if _, err := svc.AddPeriod(t.Context(), tenant, academics.NewPeriod{
		SectionID: section.ID, DayOfWeek: 0, StartsAt: "09:00", EndsAt: "09:45",
	}); !errors.Is(err, academics.ErrInvalidPeriod) {
		t.Fatalf("a duplicate slot was accepted: %v", err)
	}

	grid, err := svc.SectionTimetable(t.Context(), tenant, section.ID)
	if err != nil {
		t.Fatalf("timetable: %v", err)
	}
	if len(grid) != 2 || grid[0].StartsAt != "09:00" {
		t.Fatalf("timetable not in grid order: %+v", grid)
	}
}

func TestAttendance(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)
	author := academics.Author{UserID: seedUser(t, db)}

	class, err := svc.CreateClass(t.Context(), tenant, academics.NewGradeLevel{Name: "Class 6", Rank: 6}, author)
	if err != nil {
		t.Fatalf("create class: %v", err)
	}
	section, err := svc.CreateSection(t.Context(), tenant, class.ID, academics.NewSection{Name: "A"})
	if err != nil {
		t.Fatalf("create section: %v", err)
	}
	amina, err := svc.AdmitStudent(t.Context(), tenant, academics.NewStudent{
		AdmissionNo: "2025-001", FullName: "Amina Rahman", GradeLevelID: &class.ID, SectionID: &section.ID, Roll: 1,
	}, author)
	if err != nil {
		t.Fatalf("admit amina: %v", err)
	}
	bilal, err := svc.AdmitStudent(t.Context(), tenant, academics.NewStudent{
		AdmissionNo: "2025-002", FullName: "Bilal Ahmed", GradeLevelID: &class.ID, SectionID: &section.ID, Roll: 2,
	}, author)
	if err != nil {
		t.Fatalf("admit bilal: %v", err)
	}

	day := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)

	// An unknown status is refused before any row is written.
	if _, err := svc.MarkAttendance(t.Context(), tenant, academics.AttendanceMark{
		SectionID: &section.ID, OnDate: day,
		Entries: []academics.AttendanceEntry{{StudentID: amina.ID, Status: "here"}},
	}, author); !errors.Is(err, academics.ErrInvalidAttendance) {
		t.Fatalf("an unknown status was accepted: %v", err)
	}

	marked, err := svc.MarkAttendance(t.Context(), tenant, academics.AttendanceMark{
		SectionID: &section.ID, OnDate: day,
		Entries: []academics.AttendanceEntry{
			{StudentID: amina.ID, Status: academics.Present},
			{StudentID: bilal.ID, Status: academics.Absent},
		},
	}, author)
	if err != nil {
		t.Fatalf("mark: %v", err)
	}
	if marked != 2 {
		t.Fatalf("marked %d, want 2", marked)
	}

	// Re-marking the same day updates in place — no second row stacks up.
	if _, err := svc.MarkAttendance(t.Context(), tenant, academics.AttendanceMark{
		SectionID: &section.ID, OnDate: day,
		Entries: []academics.AttendanceEntry{{StudentID: bilal.ID, Status: academics.Late}},
	}, author); err != nil {
		t.Fatalf("re-mark: %v", err)
	}

	reg, err := svc.Register(t.Context(), tenant, section.ID, day)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if len(reg) != 2 || reg[0].FullName != "Amina Rahman" {
		t.Fatalf("register not sorted by name: %+v", reg)
	}
	for _, e := range reg {
		if e.StudentID == bilal.ID && e.Status != academics.Late {
			t.Errorf("bilal is %q, want the re-marked late", e.Status)
		}
	}

	// The month covers the marked day; the tally counts it once.
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)
	days, summary, err := svc.StudentAttendance(t.Context(), tenant, bilal.ID, from, to)
	if err != nil {
		t.Fatalf("student attendance: %v", err)
	}
	if len(days) != 1 || summary.Total != 1 || summary.Late != 1 {
		t.Fatalf("bilal's tally is %+v (%d days), want one late day", summary, len(days))
	}
}
