package bundle_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/bundle"
	"github.com/ebnsina/muallim-api/internal/platform/database"
)

func testDB(t *testing.T) *database.DB {
	t.Helper()
	url := os.Getenv("MUALLIM_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("MUALLIM_TEST_DATABASE_URL is not set")
	}
	db, err := database.New(t.Context(), database.Options{URL: url, MaxConns: 4},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

func seedTenant(t *testing.T, db *database.DB) uuid.UUID {
	t.Helper()
	id := uuid.New()
	sub := "b" + id.String()[:8]
	err := db.WithoutTenant(t.Context(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, subdomain, name) VALUES ($1, $2, $3)`, id, sub, "Test "+sub)
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

func seedCourse(t *testing.T, db *database.DB, tenant uuid.UUID, slug string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := db.WithTenant(t.Context(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO courses (tenant_id, slug, title, status) VALUES ($1, $2, $3, 'published') RETURNING id`,
			tenant, slug, "Course "+slug).Scan(&id)
	})
	if err != nil {
		t.Fatalf("seed course: %v", err)
	}
	return id
}

type stubAuditor struct{}

func (stubAuditor) Record(context.Context, pgx.Tx, uuid.UUID, bundle.AuditEntry) error { return nil }

func newService(db *database.DB) *bundle.Service {
	return bundle.NewService(db, bundle.NewPostgresRepository(), stubAuditor{})
}

func TestBundleFlow(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)
	author := bundle.Author{UserID: uuid.New()}

	c1 := seedCourse(t, db, tenant, "algebra-"+uuid.NewString()[:8])
	c2 := seedCourse(t, db, tenant, "geometry-"+uuid.NewString()[:8])

	// A bundle defaults to BDT and a free (0) price.
	b, err := svc.Create(t.Context(), tenant, bundle.NewBundle{
		Slug: "starter", Name: "Starter Pack", Description: "Two courses",
	}, author)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if b.Currency != "BDT" || b.PriceAmount != 0 {
		t.Fatalf("bundle is %+v, want free BDT default", b)
	}

	// A duplicate slug is a conflict, not a 500.
	if _, err := svc.Create(t.Context(), tenant, bundle.NewBundle{Slug: "starter", Name: "Dup"}, author); !errors.Is(err, bundle.ErrDuplicate) {
		t.Fatalf("duplicate slug accepted: %v", err)
	}

	// Set the ordered course list, then read it back in exactly two queries.
	if _, err := svc.SetCourses(t.Context(), tenant, b.ID, []uuid.UUID{c1, c2}, author); err != nil {
		t.Fatalf("set courses: %v", err)
	}

	counter := &database.Counter{}
	ctx := database.WithCounter(t.Context(), counter)
	got, err := svc.Get(ctx, tenant, "starter")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if n := counter.Count(); n != 2 {
		t.Fatalf("get issued %d queries, want 2 (bundle, its courses) — no N+1", n)
	}
	if len(got.Courses) != 2 || got.Courses[0].CourseID != c1 || got.Courses[1].CourseID != c2 {
		t.Fatalf("courses are %+v, want [c1, c2] in order", got.Courses)
	}

	// Reorder: naming the same courses in reverse rewrites positions in one statement.
	if _, err := svc.SetCourses(t.Context(), tenant, b.ID, []uuid.UUID{c2, c1}, author); err != nil {
		t.Fatalf("reorder: %v", err)
	}
	ids, err := svc.CourseIDs(t.Context(), tenant, b.ID)
	if err != nil {
		t.Fatalf("course ids: %v", err)
	}
	if len(ids) != 2 || ids[0] != c2 || ids[1] != c1 {
		t.Fatalf("course ids are %v, want [c2, c1]", ids)
	}

	// Remove a course by naming only the survivor.
	if _, err := svc.SetCourses(t.Context(), tenant, b.ID, []uuid.UUID{c1}, author); err != nil {
		t.Fatalf("remove: %v", err)
	}
	ids, err = svc.CourseIDs(t.Context(), tenant, b.ID)
	if err != nil {
		t.Fatalf("course ids: %v", err)
	}
	if len(ids) != 1 || ids[0] != c1 {
		t.Fatalf("after removal course ids are %v, want [c1]", ids)
	}

	// Update the name and price.
	name := "Renamed Pack"
	price := int64(150000)
	updated, err := svc.Update(t.Context(), tenant, "starter", bundle.BundlePatch{Name: &name, PriceAmount: &price}, author)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Name != name || updated.PriceAmount != price {
		t.Fatalf("update gave %+v, want name %q price %d", updated, name, price)
	}

	// List returns the bundle, keyset-paginated.
	page, err := svc.List(t.Context(), tenant, bundle.PageParams{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Bundles) != 1 {
		t.Fatalf("listed %d bundles, want 1", len(page.Bundles))
	}

	// Delete it; a second get is a 404.
	if err := svc.Delete(t.Context(), tenant, "starter", author); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := svc.Get(t.Context(), tenant, "starter"); !errors.Is(err, bundle.ErrNotFound) {
		t.Fatalf("get after delete: %v, want ErrNotFound", err)
	}
}

func TestGetUnknownBundleIsNotFound(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)

	if _, err := svc.Get(t.Context(), tenant, "no-such-bundle"); !errors.Is(err, bundle.ErrNotFound) {
		t.Fatalf("unknown bundle gave %v, want ErrNotFound", err)
	}
}
