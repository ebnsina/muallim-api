package gamify

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/lms-api/internal/platform/database"
)

// Repository is where points and badges live. Award methods take a transaction —
// they run inside the caller's, so a lesson's completion and its points commit
// together. Read methods own their own read-only transaction.
type Repository interface {
	// InsertAward writes one award, or nothing if it is already there. Returns
	// whether a row was newly written — the signal that the roll-up should move.
	InsertAward(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID, reason string, sourceID uuid.UUID, points int) (bool, error)

	// AddPoints moves a learner's running total by delta, creating the row if need be.
	AddPoints(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID, delta int) error

	// CountAwards counts a learner's awards of one reason, for badge thresholds.
	CountAwards(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID, reason string) (int, error)

	// GrantBadge records that a learner earned a badge, or does nothing if they
	// already hold it.
	GrantBadge(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID, badge string) error

	// Badges lists a learner's badge codes, newest first.
	Badges(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID) ([]EarnedBadge, error)

	// Standing returns a learner's points, 1-based rank, and how many learners are
	// ranked at all.
	Standing(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID) (points, rank, outOf int, err error)

	// Leaderboard returns the top learners by points, highest first.
	Leaderboard(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, limit int) ([]LeaderboardEntry, error)
}

// Service holds the rules. Award methods run in the caller's transaction; reads
// own a read-only one.
type Service struct {
	db   *database.DB
	repo Repository
}

// NewService returns a Service.
func NewService(db *database.DB, repo Repository) *Service {
	return &Service{db: db, repo: repo}
}

// AwardLesson gives a learner points for finishing a lesson, once. Idempotent:
// re-finishing the same lesson earns nothing, so the roll-up moves only on a
// genuinely new award. The first lesson ever also earns a badge.
func (s *Service) AwardLesson(ctx context.Context, tx pgx.Tx, tenantID, userID, lessonID uuid.UUID) error {
	isNew, err := s.repo.InsertAward(ctx, tx, tenantID, userID, ReasonLesson, lessonID, PointsLesson)
	if err != nil || !isNew {
		return err
	}
	if err := s.repo.AddPoints(ctx, tx, tenantID, userID, PointsLesson); err != nil {
		return err
	}

	count, err := s.repo.CountAwards(ctx, tx, tenantID, userID, ReasonLesson)
	if err != nil {
		return err
	}
	if count == 1 {
		return s.repo.GrantBadge(ctx, tx, tenantID, userID, BadgeFirstLesson)
	}
	return nil
}

// AwardCourse gives a learner points for finishing a course, once. The first
// course earns the graduate badge; the fifth, the honour roll.
func (s *Service) AwardCourse(ctx context.Context, tx pgx.Tx, tenantID, userID, courseID uuid.UUID) error {
	isNew, err := s.repo.InsertAward(ctx, tx, tenantID, userID, ReasonCourse, courseID, PointsCourse)
	if err != nil || !isNew {
		return err
	}
	if err := s.repo.AddPoints(ctx, tx, tenantID, userID, PointsCourse); err != nil {
		return err
	}

	count, err := s.repo.CountAwards(ctx, tx, tenantID, userID, ReasonCourse)
	if err != nil {
		return err
	}
	if count == 1 {
		if err := s.repo.GrantBadge(ctx, tx, tenantID, userID, BadgeGraduate); err != nil {
			return err
		}
	}
	if count >= HonorRollCourses {
		return s.repo.GrantBadge(ctx, tx, tenantID, userID, BadgeHonorRoll)
	}
	return nil
}

// MyStanding returns a learner's points, rank, and badges.
func (s *Service) MyStanding(ctx context.Context, tenantID, userID uuid.UUID) (Standing, error) {
	var standing Standing
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		points, rank, outOf, err := s.repo.Standing(ctx, tx, tenantID, userID)
		if err != nil {
			return err
		}
		badges, err := s.repo.Badges(ctx, tx, tenantID, userID)
		if err != nil {
			return err
		}
		standing = Standing{Points: points, Rank: rank, OutOf: outOf, Badges: badges}
		return nil
	})
	return standing, err
}

// Leaderboard returns the top learners in a workspace by points.
func (s *Service) Leaderboard(ctx context.Context, tenantID uuid.UUID, limit int) ([]LeaderboardEntry, error) {
	if limit <= 0 || limit > MaxLeaderboardSize {
		limit = DefaultLeaderboardSize
	}

	var entries []LeaderboardEntry
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		entries, err = s.repo.Leaderboard(ctx, tx, tenantID, limit)
		return err
	})
	return entries, err
}

// Catalog returns every badge this system can award, for a client that wants to
// show the unearned ones too.
func (s *Service) Catalog() []Badge {
	out := make([]Badge, len(catalog))
	copy(out, catalog)
	return out
}
