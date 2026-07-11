package gamify_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/lms-api/internal/gamify"
	"github.com/ebnsina/lms-api/internal/platform/database"
)

func testDB(t *testing.T) *database.DB {
	t.Helper()
	url := os.Getenv("LMS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LMS_TEST_DATABASE_URL is not set; skipping database tests")
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
		// A membership, so the user is visible under the tenant's RLS — the leaderboard
		// joins users, and a real learner is always a member.
		_, err := tx.Exec(ctx,
			`INSERT INTO memberships (tenant_id, user_id, role) VALUES ($1, $2, 'student')`, tenantID, id)
		return err
	})
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

func newService(db *database.DB) *gamify.Service {
	return gamify.NewService(db, gamify.NewPostgresRepository())
}

// awardLesson and awardCourse run an award in a throwaway transaction, the way a
// producer would inside its own event.
func awardLesson(t *testing.T, db *database.DB, svc *gamify.Service, tenantID, userID, lessonID uuid.UUID) {
	t.Helper()
	err := db.WithTenant(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return svc.AwardLesson(ctx, tx, tenantID, userID, lessonID)
	})
	if err != nil {
		t.Fatalf("award lesson: %v", err)
	}
}

func awardCourse(t *testing.T, db *database.DB, svc *gamify.Service, tenantID, userID, courseID uuid.UUID) {
	t.Helper()
	err := db.WithTenant(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return svc.AwardCourse(ctx, tx, tenantID, userID, courseID)
	})
	if err != nil {
		t.Fatalf("award course: %v", err)
	}
}

func TestPointsAreIdempotentAndBadgesAreEarned(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	learner := seedUser(t, db, tenantID, "Learner")

	lessonA, lessonB := uuid.New(), uuid.New()

	// First lesson: points and the first-steps badge.
	awardLesson(t, db, svc, tenantID, learner, lessonA)
	// Same lesson again — a re-completion — earns nothing new.
	awardLesson(t, db, svc, tenantID, learner, lessonA)
	awardLesson(t, db, svc, tenantID, learner, lessonB)

	standing, err := svc.MyStanding(t.Context(), tenantID, learner)
	if err != nil {
		t.Fatalf("standing: %v", err)
	}
	if standing.Points != 2*gamify.PointsLesson {
		t.Fatalf("points = %d, want %d (two lessons, one counted once)", standing.Points, 2*gamify.PointsLesson)
	}
	if !hasBadge(standing.Badges, gamify.BadgeFirstLesson) {
		t.Fatalf("no first-lesson badge: %+v", standing.Badges)
	}

	// Finishing a course adds its points and the graduate badge.
	awardCourse(t, db, svc, tenantID, learner, uuid.New())
	standing, _ = svc.MyStanding(t.Context(), tenantID, learner)
	if standing.Points != 2*gamify.PointsLesson+gamify.PointsCourse {
		t.Fatalf("points after course = %d", standing.Points)
	}
	if !hasBadge(standing.Badges, gamify.BadgeGraduate) {
		t.Fatalf("no graduate badge")
	}

	// Five courses in total earns the honour roll.
	for range gamify.HonorRollCourses - 1 {
		awardCourse(t, db, svc, tenantID, learner, uuid.New())
	}
	standing, _ = svc.MyStanding(t.Context(), tenantID, learner)
	if !hasBadge(standing.Badges, gamify.BadgeHonorRoll) {
		t.Fatalf("no honour-roll badge after %d courses", gamify.HonorRollCourses)
	}
}

func TestLeaderboardRanksByPoints(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)

	top := seedUser(t, db, tenantID, "Top")
	mid := seedUser(t, db, tenantID, "Middle")

	// top: two courses; mid: one lesson.
	awardCourse(t, db, svc, tenantID, top, uuid.New())
	awardCourse(t, db, svc, tenantID, top, uuid.New())
	awardLesson(t, db, svc, tenantID, mid, uuid.New())

	board, err := svc.Leaderboard(t.Context(), tenantID, 20)
	if err != nil {
		t.Fatalf("leaderboard: %v", err)
	}
	if len(board) != 2 {
		t.Fatalf("board has %d entries, want 2", len(board))
	}
	if board[0].Name != "Top" || board[0].Rank != 1 {
		t.Fatalf("top of the board is %+v", board[0])
	}
	if board[1].Name != "Middle" || board[1].Rank != 2 {
		t.Fatalf("second is %+v", board[1])
	}

	// The middle learner's own standing: rank 2 of 2.
	standing, _ := svc.MyStanding(t.Context(), tenantID, mid)
	if standing.Rank != 2 || standing.OutOf != 2 {
		t.Fatalf("standing rank %d of %d, want 2 of 2", standing.Rank, standing.OutOf)
	}
}

func hasBadge(badges []gamify.EarnedBadge, code string) bool {
	for _, b := range badges {
		if b.Code == code {
			return true
		}
	}
	return false
}
