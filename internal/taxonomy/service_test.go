package taxonomy_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/platform/database"
	"github.com/ebnsina/muallim-api/internal/taxonomy"
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
	sub := "x" + id.String()[:8]
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
			`INSERT INTO courses (tenant_id, slug, title, status) VALUES ($1, $2, 'Course', 'published') RETURNING id`,
			tenant, slug).Scan(&id)
	})
	if err != nil {
		t.Fatalf("seed course: %v", err)
	}
	return id
}

type stubAuditor struct{}

func (stubAuditor) Record(context.Context, pgx.Tx, uuid.UUID, taxonomy.AuditEntry) error { return nil }

func newService(db *database.DB) *taxonomy.Service {
	return taxonomy.NewService(db, taxonomy.NewPostgresRepository(), stubAuditor{})
}

func TestCategoryAndTagFlow(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)
	author := taxonomy.Author{UserID: uuid.New()}

	courseSlug := "intro-fiqh"
	seedCourse(t, db, tenant, courseSlug)

	// Create a category; a slug derives from the name when omitted.
	fiqh, err := svc.CreateCategory(t.Context(), tenant, taxonomy.NewTerm{Name: "Islamic Law"}, author)
	if err != nil {
		t.Fatalf("create category: %v", err)
	}
	if fiqh.Slug != "islamic-law" {
		t.Fatalf("slug %q, want islamic-law", fiqh.Slug)
	}

	// A clashing slug is a duplicate.
	if _, err := svc.CreateCategory(t.Context(), tenant, taxonomy.NewTerm{Name: "Different", Slug: "islamic-law"}, author); !errors.Is(err, taxonomy.ErrDuplicate) {
		t.Fatalf("a clashing category slug was accepted: %v", err)
	}

	cats, err := svc.ListCategories(t.Context(), tenant, taxonomy.PageParams{})
	if err != nil {
		t.Fatalf("list categories: %v", err)
	}
	if len(cats.Categories) != 1 {
		t.Fatalf("listed %d categories, want 1", len(cats.Categories))
	}

	// Two tags.
	beginner, err := svc.CreateTag(t.Context(), tenant, taxonomy.NewTerm{Name: "Beginner"}, author)
	if err != nil {
		t.Fatalf("create tag: %v", err)
	}
	arabic, err := svc.CreateTag(t.Context(), tenant, taxonomy.NewTerm{Name: "Arabic"}, author)
	if err != nil {
		t.Fatalf("create tag: %v", err)
	}
	tags, err := svc.ListTags(t.Context(), tenant, taxonomy.PageParams{})
	if err != nil {
		t.Fatalf("list tags: %v", err)
	}
	if len(tags.Tags) != 2 {
		t.Fatalf("listed %d tags, want 2", len(tags.Tags))
	}

	// Set the course's category and tags together.
	cat, courseTags, err := svc.SetCourseTaxonomyBySlug(t.Context(), tenant, courseSlug, &fiqh.ID, []uuid.UUID{beginner.ID, arabic.ID}, author)
	if err != nil {
		t.Fatalf("set course taxonomy: %v", err)
	}
	if cat == nil || cat.ID != fiqh.ID {
		t.Fatalf("category not set: %+v", cat)
	}
	if len(courseTags) != 2 {
		t.Fatalf("course has %d tags, want 2", len(courseTags))
	}

	// The category is one per course: setting it again replaces it.
	law2, err := svc.CreateCategory(t.Context(), tenant, taxonomy.NewTerm{Name: "Foundations"}, author)
	if err != nil {
		t.Fatalf("create second category: %v", err)
	}
	if err := svc.SetCourseCategory(t.Context(), tenant, mustCourseID(t, svc, tenant, courseSlug), &law2.ID, author); err != nil {
		t.Fatalf("replace category: %v", err)
	}
	got, err := svc.CourseCategory(t.Context(), tenant, mustCourseID(t, svc, tenant, courseSlug))
	if err != nil {
		t.Fatalf("course category: %v", err)
	}
	if got == nil || got.ID != law2.ID {
		t.Fatalf("category not replaced: %+v", got)
	}

	// Course ids in a category and with a tag.
	inCat, err := svc.CourseIDsInCategory(t.Context(), tenant, law2.ID)
	if err != nil {
		t.Fatalf("course ids in category: %v", err)
	}
	if len(inCat) != 1 {
		t.Fatalf("category has %d courses, want 1", len(inCat))
	}
	withTag, err := svc.CourseIDsWithTag(t.Context(), tenant, beginner.ID)
	if err != nil {
		t.Fatalf("course ids with tag: %v", err)
	}
	if len(withTag) != 1 {
		t.Fatalf("tag has %d courses, want 1", len(withTag))
	}

	// Replace the tag set with just one, then clear it.
	if err := svc.SetCourseTags(t.Context(), tenant, mustCourseID(t, svc, tenant, courseSlug), []uuid.UUID{arabic.ID}, author); err != nil {
		t.Fatalf("replace tags: %v", err)
	}
	one, err := svc.CourseTags(t.Context(), tenant, mustCourseID(t, svc, tenant, courseSlug))
	if err != nil {
		t.Fatalf("course tags: %v", err)
	}
	if len(one) != 1 || one[0].ID != arabic.ID {
		t.Fatalf("tags not replaced: %+v", one)
	}
	if err := svc.SetCourseTags(t.Context(), tenant, mustCourseID(t, svc, tenant, courseSlug), nil, author); err != nil {
		t.Fatalf("clear tags: %v", err)
	}
	none, err := svc.CourseTags(t.Context(), tenant, mustCourseID(t, svc, tenant, courseSlug))
	if err != nil {
		t.Fatalf("course tags after clear: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("tags not cleared: %+v", none)
	}

	// Clearing the category with a nil id removes it.
	if err := svc.SetCourseCategory(t.Context(), tenant, mustCourseID(t, svc, tenant, courseSlug), nil, author); err != nil {
		t.Fatalf("clear category: %v", err)
	}
	cleared, err := svc.CourseCategory(t.Context(), tenant, mustCourseID(t, svc, tenant, courseSlug))
	if err != nil {
		t.Fatalf("course category after clear: %v", err)
	}
	if cleared != nil {
		t.Fatalf("category not cleared: %+v", cleared)
	}

	// Deletes.
	if err := svc.DeleteTag(t.Context(), tenant, arabic.ID, author); err != nil {
		t.Fatalf("delete tag: %v", err)
	}
	if err := svc.DeleteCategory(t.Context(), tenant, law2.ID, author); err != nil {
		t.Fatalf("delete category: %v", err)
	}
}

func TestUnknownCourseSlugIsNotFound(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)

	if _, err := svc.CourseIDBySlug(t.Context(), tenant, "no-such-course"); !errors.Is(err, taxonomy.ErrNotFound) {
		t.Fatalf("an unknown slug was resolved: %v", err)
	}
	if _, _, err := svc.CourseTaxonomyBySlug(t.Context(), tenant, "no-such-course"); !errors.Is(err, taxonomy.ErrNotFound) {
		t.Fatalf("taxonomy for an unknown course was returned: %v", err)
	}
}

func TestUnknownDeletesAndReferencesAreNotFound(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)
	author := taxonomy.Author{UserID: uuid.New()}

	if err := svc.DeleteCategory(t.Context(), tenant, uuid.New(), author); !errors.Is(err, taxonomy.ErrNotFound) {
		t.Fatalf("deleting a missing category succeeded: %v", err)
	}
	if err := svc.DeleteTag(t.Context(), tenant, uuid.New(), author); !errors.Is(err, taxonomy.ErrNotFound) {
		t.Fatalf("deleting a missing tag succeeded: %v", err)
	}

	// Filing a course under an unknown category is a 404.
	slug := "some-course"
	seedCourse(t, db, tenant, slug)
	if err := svc.SetCourseCategory(t.Context(), tenant, mustCourseID(t, svc, tenant, slug), ptr(uuid.New()), author); !errors.Is(err, taxonomy.ErrNotFound) {
		t.Fatalf("filing under a missing category succeeded: %v", err)
	}
	if err := svc.SetCourseTags(t.Context(), tenant, mustCourseID(t, svc, tenant, slug), []uuid.UUID{uuid.New()}, author); !errors.Is(err, taxonomy.ErrNotFound) {
		t.Fatalf("tagging with a missing tag succeeded: %v", err)
	}
}

func mustCourseID(t *testing.T, svc *taxonomy.Service, tenant uuid.UUID, slug string) uuid.UUID {
	t.Helper()
	id, err := svc.CourseIDBySlug(t.Context(), tenant, slug)
	if err != nil {
		t.Fatalf("resolve course id: %v", err)
	}
	return id
}

func ptr(id uuid.UUID) *uuid.UUID { return &id }
