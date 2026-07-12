package catalog_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/catalog"
	"github.com/ebnsina/muallim-api/internal/platform/database"
)

// seedManyCourses inserts enough rows that the planner has a real choice. A
// sequential scan over two hundred rows looks fine and is a time bomb; the plan
// only becomes informative once an index is cheaper than reading the table.
func seedManyCourses(t *testing.T, db *database.DB, tenantID uuid.UUID, n int) {
	t.Helper()

	err := db.WithTenant(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO courses (tenant_id, slug, title, status, published_at, created_at)
			SELECT $1,
			       'c-' || g,
			       'Course ' || g,
			       CASE WHEN g % 3 = 0 THEN 'draft' ELSE 'published' END,
			       CASE WHEN g % 3 = 0 THEN NULL ELSE now() - (g || ' minutes')::interval END,
			       now() - (g || ' minutes')::interval
			FROM generate_series(1, $2) AS g`, tenantID, n)
		return err
	})
	if err != nil {
		t.Fatalf("seed courses: %v", err)
	}

	// Without fresh statistics the planner is guessing, and a plan produced from a
	// guess proves nothing about the plan production will use.
	err = db.WithoutTenant(t.Context(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `ANALYZE courses`)
		return err
	})
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
}

// explain returns the plan for a statement, as Postgres would execute it.
func explain(t *testing.T, db *database.DB, tenantID uuid.UUID, sql string, args ...any) string {
	t.Helper()

	var plan strings.Builder
	err := db.WithTenantReadOnly(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, "EXPLAIN (ANALYZE, COSTS OFF) "+sql, args...)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				return err
			}
			plan.WriteString(line)
			plan.WriteString("\n")
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	return plan.String()
}

// Both course listings must be served by an index that covers the filter and the
// sort together. A Sort node means the database read every matching row and
// ordered them in memory; a Seq Scan means it read the whole table. Either on a
// request path is a defect, and the last page of a large catalog is where it
// shows up first.
func TestCourseListingsAreIndexScansWithoutASortNode(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenantID := seedTenant(t, db)
	seedManyCourses(t, db, tenantID, 3000)

	tests := []struct {
		name string
		sql  string
	}{
		{"published listing", catalog.ListPublishedCoursesSQL},
		{"authored listing, every status", catalog.ListAllCoursesSQL},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// A first page: no cursor, and no search or difficulty, so every
			// residual predicate is inert and the index alone shapes the scan.
			plan := explain(t, db, tenantID, tt.sql, tenantID, nil, nil, 21, nil, nil)

			if strings.Contains(plan, "Seq Scan") {
				t.Errorf("plan reads the whole table:\n%s", plan)
			}
			if strings.Contains(plan, "Sort") {
				t.Errorf("plan sorts in memory rather than reading an ordered index:\n%s", plan)
			}
			if !strings.Contains(plan, "Index Scan") {
				t.Errorf("plan is not an index scan:\n%s", plan)
			}
		})
	}
}

// A deep page must cost what the first page costs. The keyset seeks straight to
// its position on the same index; an OFFSET would read and discard everything in
// front of it.
func TestDeepCoursePageStillSeeksOnTheIndex(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	seedManyCourses(t, db, tenantID, 3000)

	// Walk far enough in that a scan-and-discard plan would be obvious.
	params := catalog.ListParams{Limit: 20, IncludeDrafts: true}
	var page catalog.Page
	for i := range 10 {
		var err error
		page, err = svc.ListCourses(t.Context(), tenantID, params)
		if err != nil {
			t.Fatalf("page %d: %v", i, err)
		}
		if !page.HasMore {
			t.Fatalf("ran out of pages at %d; the fixture is too small to be interesting", i)
		}
		params.Cursor = page.NextCursor
	}

	createdAt, id, err := catalog.DecodeCursorForTest(page.NextCursor)
	if err != nil {
		t.Fatalf("decode cursor: %v", err)
	}

	plan := explain(t, db, tenantID, catalog.ListAllCoursesSQL, tenantID, createdAt, id, 21, nil, nil)

	if strings.Contains(plan, "Seq Scan") || strings.Contains(plan, "Sort") {
		t.Errorf("a deep page does not seek:\n%s", plan)
	}
	if !strings.Contains(plan, "Index Scan") {
		t.Errorf("deep page is not an index scan:\n%s", plan)
	}
}
