package enroll_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/enroll"
	"github.com/ebnsina/muallim-api/internal/platform/database"
)

func explainEnroll(t *testing.T, db *database.DB, tenantID uuid.UUID, sql string, args ...any) string {
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
			plan.WriteString(line + "\n")
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	return plan.String()
}

// The target course gets a modest number of enrolments; a second "noise" course
// gets many more. That is the realistic shape — one course is a small fraction of
// a workspace's enrolments — and it is the shape that makes the (tenant, course)
// index cheaper than reading the whole table. A fixture where the target course is
// most of the table would (correctly) be seq-scanned and prove nothing.
func seedManyEnrolments(t *testing.T, db *database.DB, tenantID, courseID uuid.UUID) {
	t.Helper()
	const (
		target = 1_000
		noise  = 30_000
	)
	salt := uuid.NewString()
	err := db.WithTenant(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		// Enough users for the noise course; the target reuses the first `target`.
		if _, err := tx.Exec(ctx,
			`INSERT INTO users (id, email, password_hash, name)
			 SELECT md5($1 || g::text)::uuid, md5($1 || g::text) || '@x.test', 'x', 'L'
			 FROM generate_series(1, $2) g`, salt, noise); err != nil {
			return err
		}

		// A noise course, and its many enrolments — the bulk of the table.
		var noiseCourse uuid.UUID
		if err := tx.QueryRow(ctx,
			`INSERT INTO courses (tenant_id, slug, title, status) VALUES ($1, $2, 'Noise', 'published') RETURNING id`,
			tenantID, "noise-"+salt[:8]).Scan(&noiseCourse); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO enrolments (tenant_id, course_id, user_id, status, source)
			 SELECT $1, $2, md5($3 || g::text)::uuid, 'active', 'self'
			 FROM generate_series(1, $4) g`, tenantID, noiseCourse, salt, noise); err != nil {
			return err
		}

		// The target course's enrolments and their progress.
		if _, err := tx.Exec(ctx,
			`INSERT INTO enrolments (tenant_id, course_id, user_id, status, source)
			 SELECT $1, $2, md5($3 || g::text)::uuid,
			        (ARRAY['active','completed','cancelled'])[1 + g % 3], 'self'
			 FROM generate_series(1, $4) g`, tenantID, courseID, salt, target); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO course_progress (tenant_id, user_id, course_id, lessons_completed, lessons_total, percent)
			 SELECT $1, md5($2 || g::text)::uuid, $3, g % 10, 10, (g % 10) * 10
			 FROM generate_series(1, $4) g`, tenantID, salt, courseID, target)
		return err
	})
	if err != nil {
		t.Fatalf("seed enrolments: %v", err)
	}
	if err := db.WithoutTenant(t.Context(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `ANALYZE enrolments, course_progress`)
		return err
	}); err != nil {
		t.Fatalf("analyze: %v", err)
	}
}

// Course analytics must seek enrolments on (tenant, course), not scan the table —
// enrolments is the largest table in the schema, so a Seq Scan here is the worst
// offender at size.
func TestCourseAnalyticsSeeksEnrolmentsOnItsIndex(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenantID := seedTenant(t, db)
	slug, _ := seedCourse(t, db, tenantID, 1, true)

	var courseID uuid.UUID
	if err := db.WithTenantReadOnly(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM courses WHERE tenant_id = $1 AND slug = $2`, tenantID, slug).Scan(&courseID)
	}); err != nil {
		t.Fatalf("course id: %v", err)
	}
	seedManyEnrolments(t, db, tenantID, courseID)

	plan := explainEnroll(t, db, tenantID, enroll.CourseStatsSQL, tenantID, courseID)

	if strings.Contains(plan, "Seq Scan on enrolments") {
		t.Errorf("analytics reads the whole enrolments table:\n%s", plan)
	}
	if !strings.Contains(plan, "enrolments_course_user_key") {
		t.Errorf("analytics does not seek on the (tenant, course, user) index:\n%s", plan)
	}
}

// The review wall must read course_reviews_course_idx in order, never a Sort.
func TestReviewListIsAnIndexScanWithoutASortNode(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenantID := seedTenant(t, db)
	slug, _ := seedCourse(t, db, tenantID, 1, true)

	var courseID uuid.UUID
	if err := db.WithTenantReadOnly(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM courses WHERE tenant_id = $1 AND slug = $2`, tenantID, slug).Scan(&courseID)
	}); err != nil {
		t.Fatalf("course id: %v", err)
	}

	// Many reviewers, so the ordered index beats a sort.
	salt := uuid.NewString()
	if err := db.WithTenant(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO users (id, email, password_hash, name)
			 SELECT md5($1 || g::text)::uuid, md5($1 || g::text) || '@x.test', 'x', 'R'
			 FROM generate_series(1, $2) g`, salt, 3000); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO course_reviews (tenant_id, course_id, user_id, rating, body, created_at)
			 SELECT $1, $2, md5($3 || g::text)::uuid, 1 + g % 5, 'ok', now() - (g || ' minutes')::interval
			 FROM generate_series(1, $4) g`, tenantID, courseID, salt, 3000)
		return err
	}); err != nil {
		t.Fatalf("seed reviews: %v", err)
	}
	if err := db.WithoutTenant(t.Context(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `ANALYZE course_reviews`)
		return err
	}); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	plan := explainEnroll(t, db, tenantID, enroll.ListReviewsSQL, tenantID, courseID, 20)
	if strings.Contains(plan, "Seq Scan on course_reviews") {
		t.Errorf("review wall reads the whole table:\n%s", plan)
	}
	if strings.Contains(plan, "Sort") {
		t.Errorf("review wall sorts rather than reading the ordered index:\n%s", plan)
	}
}
