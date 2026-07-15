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
