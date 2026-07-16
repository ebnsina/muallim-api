package learnpath_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/learnpath"
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
	sub := "p" + id.String()[:8]
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
			`INSERT INTO courses (tenant_id, slug, title) VALUES ($1, $2, $3) RETURNING id`,
			tenant, slug, "Course "+slug).Scan(&id)
	})
	if err != nil {
		t.Fatalf("seed course: %v", err)
	}
	return id
}

type stubAuditor struct{}

func (stubAuditor) Record(context.Context, pgx.Tx, uuid.UUID, learnpath.AuditEntry) error { return nil }

func newService(db *database.DB) *learnpath.Service {
	return learnpath.NewService(db, learnpath.NewPostgresRepository(), stubAuditor{})
}

func TestPathFlow(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)
	author := learnpath.Author{UserID: uuid.New()}

	c1 := seedCourse(t, db, tenant, "algebra")
	c2 := seedCourse(t, db, tenant, "geometry")
	c3 := seedCourse(t, db, tenant, "calculus")

	// Create a draft path.
	path, err := svc.Create(t.Context(), tenant, learnpath.NewPath{
		Slug: "maths-track", Title: "Mathematics Track", Description: "In order.",
	}, author)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if path.Status != learnpath.StatusDraft {
		t.Fatalf("status %q, want draft", path.Status)
	}

	// A second path with the same slug is a conflict.
	if _, err := svc.Create(t.Context(), tenant, learnpath.NewPath{
		Slug: "maths-track", Title: "Another",
	}, author); !errors.Is(err, learnpath.ErrDuplicate) {
		t.Fatalf("a duplicate slug was accepted: %v", err)
	}

	// Set the ordered course list.
	if err := svc.SetCourses(t.Context(), tenant, path.ID, []uuid.UUID{c1, c2, c3}, author); err != nil {
		t.Fatalf("set courses: %v", err)
	}

	// A reorder names every course, in a new order.
	if err := svc.SetCourses(t.Context(), tenant, path.ID, []uuid.UUID{c3, c1, c2}, author); err != nil {
		t.Fatalf("reorder: %v", err)
	}

	// An order that names a course twice is refused rather than half-applied.
	if err := svc.SetCourses(t.Context(), tenant, path.ID, []uuid.UUID{c1, c1, c2}, author); !errors.Is(err, learnpath.ErrIncompleteOrder) {
		t.Fatalf("a partial order was accepted: %v", err)
	}

	// Get returns the courses in the reordered order.
	got, err := svc.Get(t.Context(), tenant, "maths-track", true)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	want := []uuid.UUID{c3, c1, c2}
	if fmt.Sprint(got.Courses) != fmt.Sprint(want) {
		t.Fatalf("courses %v, want %v", got.Courses, want)
	}

	// CourseIDs returns the same ordered list — what the coordinator maps onto.
	ids, err := svc.CourseIDs(t.Context(), tenant, path.ID)
	if err != nil {
		t.Fatalf("course ids: %v", err)
	}
	if fmt.Sprint(ids) != fmt.Sprint(want) {
		t.Fatalf("course ids %v, want %v", ids, want)
	}

	// Get loads the courses in a constant number of queries, whatever their count.
	counter := &database.Counter{}
	cctx := database.WithCounter(t.Context(), counter)
	if _, err := svc.Get(cctx, tenant, "maths-track", true); err != nil {
		t.Fatalf("get counted: %v", err)
	}
	small := counter.Count()

	fourth := seedCourse(t, db, tenant, "topology")
	if err := svc.SetCourses(t.Context(), tenant, path.ID, []uuid.UUID{c1, c2, c3, fourth}, author); err != nil {
		t.Fatalf("grow: %v", err)
	}
	counter.Reset()
	if _, err := svc.Get(cctx, tenant, "maths-track", true); err != nil {
		t.Fatalf("get counted grown: %v", err)
	}
	if grown := counter.Count(); grown != small {
		t.Fatalf("Get issued %d queries for 4 courses, %d for 3 — an N+1", grown, small)
	}

	// Publish, then find it under the status filter.
	published := learnpath.StatusPublished
	if _, err := svc.Update(t.Context(), tenant, "maths-track", learnpath.PathPatch{Status: &published}, author); err != nil {
		t.Fatalf("publish: %v", err)
	}
	page, err := svc.List(t.Context(), tenant, learnpath.PathFilter{IncludeDrafts: true, Status: learnpath.StatusPublished}, learnpath.PageParams{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Paths) != 1 || page.Paths[0].Status != learnpath.StatusPublished {
		t.Fatalf("published listing: %+v", page.Paths)
	}
	draftPage, err := svc.List(t.Context(), tenant, learnpath.PathFilter{IncludeDrafts: true, Status: learnpath.StatusDraft}, learnpath.PageParams{})
	if err != nil {
		t.Fatalf("list drafts: %v", err)
	}
	if len(draftPage.Paths) != 0 {
		t.Fatalf("draft listing not empty: %+v", draftPage.Paths)
	}

	// Delete removes it; a second Get is a 404.
	if err := svc.Delete(t.Context(), tenant, "maths-track", author); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := svc.Get(t.Context(), tenant, "maths-track", true); !errors.Is(err, learnpath.ErrNotFound) {
		t.Fatalf("deleted path still resolves: %v", err)
	}
}

func TestListKeysetPaginates(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)
	author := learnpath.Author{UserID: uuid.New()}

	for i := range 3 {
		if _, err := svc.Create(t.Context(), tenant, learnpath.NewPath{
			Slug: fmt.Sprintf("track-%d", i), Title: fmt.Sprintf("Track %d", i),
		}, author); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}

	first, err := svc.List(t.Context(), tenant, learnpath.PathFilter{IncludeDrafts: true}, learnpath.PageParams{Limit: 2})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if len(first.Paths) != 2 || !first.HasMore || first.NextCursor == "" {
		t.Fatalf("first page: got %d, hasMore=%v cursor=%q", len(first.Paths), first.HasMore, first.NextCursor)
	}
	second, err := svc.List(t.Context(), tenant, learnpath.PathFilter{IncludeDrafts: true}, learnpath.PageParams{Limit: 2, Cursor: first.NextCursor})
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if len(second.Paths) != 1 || second.HasMore {
		t.Fatalf("second page: got %d, hasMore=%v", len(second.Paths), second.HasMore)
	}
}

func TestGetUnknownIsNotFound(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)

	if _, err := svc.Get(t.Context(), tenant, "no-such-path", true); !errors.Is(err, learnpath.ErrNotFound) {
		t.Fatalf("unknown path did not 404: %v", err)
	}
}

func TestSetCoursesOnUnknownPathIsNotFound(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)

	if err := svc.SetCourses(t.Context(), tenant, uuid.New(), nil, learnpath.Author{UserID: uuid.New()}); !errors.Is(err, learnpath.ErrNotFound) {
		t.Fatalf("set courses on a missing path did not 404: %v", err)
	}
}

// A draft path belongs to the people writing it. Everybody else is told the same
// thing they are told about a path that was never written — nothing. The reader's
// authority decides this, so no query parameter can widen it.
func TestDraftPathIsInvisibleToAReader(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)
	author := learnpath.Author{UserID: uuid.New()}

	if _, err := svc.Create(t.Context(), tenant, learnpath.NewPath{
		Slug: "secret-track", Title: "Unannounced",
	}, author); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := svc.Create(t.Context(), tenant, learnpath.NewPath{
		Slug: "open-track", Title: "Announced",
	}, author); err != nil {
		t.Fatalf("create published: %v", err)
	}
	published := learnpath.StatusPublished
	if _, err := svc.Update(t.Context(), tenant, "open-track",
		learnpath.PathPatch{Status: &published}, author); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// A reader fetching the draft by name is told it does not exist.
	if _, err := svc.Get(t.Context(), tenant, "secret-track", false); !errors.Is(err, learnpath.ErrNotFound) {
		t.Fatalf("a draft path was served to a reader: %v", err)
	}
	// The published one they may have.
	if _, err := svc.Get(t.Context(), tenant, "open-track", false); err != nil {
		t.Fatalf("published path withheld from a reader: %v", err)
	}
	// An author sees the draft.
	if _, err := svc.Get(t.Context(), tenant, "secret-track", true); err != nil {
		t.Fatalf("draft withheld from its author: %v", err)
	}

	// The listing shows a reader the published path and nothing else.
	page, err := svc.List(t.Context(), tenant, learnpath.PathFilter{}, learnpath.PageParams{})
	if err != nil {
		t.Fatalf("reader list: %v", err)
	}
	if len(page.Paths) != 1 || page.Paths[0].Slug != "open-track" {
		t.Fatalf("a reader's listing leaked a draft: %+v", page.Paths)
	}

	// Asking for drafts does not conjure them: the status narrows what a reader may
	// see, and can never widen it. This is the vulnerability, so it is asserted.
	asked, err := svc.List(t.Context(), tenant,
		learnpath.PathFilter{Status: learnpath.StatusDraft}, learnpath.PageParams{})
	if err != nil {
		t.Fatalf("reader asking for drafts: %v", err)
	}
	if len(asked.Paths) != 0 {
		t.Fatalf("?status=draft handed a reader %d drafts", len(asked.Paths))
	}

	// The same request from somebody who may write finds it.
	authored, err := svc.List(t.Context(), tenant,
		learnpath.PathFilter{IncludeDrafts: true, Status: learnpath.StatusDraft}, learnpath.PageParams{})
	if err != nil {
		t.Fatalf("author list: %v", err)
	}
	if len(authored.Paths) != 1 || authored.Paths[0].Slug != "secret-track" {
		t.Fatalf("an author cannot see their own draft: %+v", authored.Paths)
	}
}
