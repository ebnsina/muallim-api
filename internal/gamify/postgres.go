package gamify

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// PostgresRepository is the gamification tables.
type PostgresRepository struct{}

// NewPostgresRepository returns one.
func NewPostgresRepository() *PostgresRepository { return &PostgresRepository{} }

const insertAwardSQL = `
	INSERT INTO point_awards (tenant_id, user_id, reason, source_id, points)
	VALUES ($1, $2, $3, $4, $5)
	ON CONFLICT (tenant_id, user_id, reason, source_id) DO NOTHING`

// InsertAward writes an award, or nothing if the learner already has it. The
// RowsAffected is the whole idempotency signal: 1 is new, 0 is already there.
func (r *PostgresRepository) InsertAward(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID, reason string, sourceID uuid.UUID, points int) (bool, error) {
	tag, err := tx.Exec(ctx, insertAwardSQL, tenantID, userID, reason, sourceID, points)
	if err != nil {
		return false, fmt.Errorf("gamify: insert award: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

const addPointsSQL = `
	INSERT INTO user_points (tenant_id, user_id, points)
	VALUES ($1, $2, $3)
	ON CONFLICT (tenant_id, user_id) DO UPDATE
	SET points = user_points.points + EXCLUDED.points, updated_at = now()`

// AddPoints moves a learner's total by delta.
func (r *PostgresRepository) AddPoints(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID, delta int) error {
	if _, err := tx.Exec(ctx, addPointsSQL, tenantID, userID, delta); err != nil {
		return fmt.Errorf("gamify: add points: %w", err)
	}
	return nil
}

// CountAwards counts a learner's awards of one reason.
func (r *PostgresRepository) CountAwards(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID, reason string) (int, error) {
	var n int
	err := tx.QueryRow(ctx,
		`SELECT count(*) FROM point_awards WHERE tenant_id = $1 AND user_id = $2 AND reason = $3`,
		tenantID, userID, reason).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("gamify: count awards: %w", err)
	}
	return n, nil
}

// GrantBadge records a badge, or does nothing if the learner already holds it.
func (r *PostgresRepository) GrantBadge(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID, badge string) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO user_badges (tenant_id, user_id, badge) VALUES ($1, $2, $3)
		 ON CONFLICT (tenant_id, user_id, badge) DO NOTHING`,
		tenantID, userID, badge)
	if err != nil {
		return fmt.Errorf("gamify: grant badge: %w", err)
	}
	return nil
}

// Badges lists a learner's earned badges, newest first, described from the code
// catalogue so a client need not know it.
func (r *PostgresRepository) Badges(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID) ([]EarnedBadge, error) {
	rows, err := tx.Query(ctx,
		`SELECT badge, awarded_at FROM user_badges WHERE tenant_id = $1 AND user_id = $2 ORDER BY awarded_at DESC, badge`,
		tenantID, userID)
	if err != nil {
		return nil, fmt.Errorf("gamify: list badges: %w", err)
	}
	defer rows.Close()

	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (EarnedBadge, error) {
		var (
			code string
			at   = &EarnedBadge{}
		)
		if err := row.Scan(&code, &at.AwardedAt); err != nil {
			return EarnedBadge{}, err
		}
		at.Badge = describe(code)
		return *at, nil
	})
	if err != nil {
		return nil, fmt.Errorf("gamify: scan badges: %w", err)
	}
	return out, nil
}

const standingSQL = `
	WITH me AS (
		SELECT COALESCE(
			(SELECT points FROM user_points WHERE tenant_id = $1 AND user_id = $2), 0
		) AS p
	)
	SELECT me.p,
	       (SELECT count(*) FROM user_points WHERE tenant_id = $1 AND points > me.p) + 1,
	       (SELECT count(*) FROM user_points WHERE tenant_id = $1)
	FROM me`

// Standing returns a learner's points, 1-based rank, and the size of the board.
func (r *PostgresRepository) Standing(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID) (points, rank, outOf int, err error) {
	err = tx.QueryRow(ctx, standingSQL, tenantID, userID).Scan(&points, &rank, &outOf)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("gamify: standing: %w", err)
	}
	return points, rank, outOf, nil
}

// LeaderboardSQL is exported for a query-plan test that EXPLAINs this exact
// statement to prove the leaderboard is an ordered index scan.
//
// The tie-break is user_id ascending, to match user_points_leaderboard_idx —
// (tenant_id, points DESC, user_id) — exactly. Ordering it the other way costs an
// incremental sort for nothing: within one points value the order is arbitrary,
// so long as it is stable.
const LeaderboardSQL = `
	SELECT up.user_id, COALESCE(u.name, ''), up.points
	FROM user_points up
	JOIN users u ON u.id = up.user_id
	WHERE up.tenant_id = $1
	ORDER BY up.points DESC, up.user_id
	LIMIT $2`

// Leaderboard returns the top learners, highest first. Rank is the row's position.
func (r *PostgresRepository) Leaderboard(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, limit int) ([]LeaderboardEntry, error) {
	rows, err := tx.Query(ctx, LeaderboardSQL, tenantID, limit)
	if err != nil {
		return nil, fmt.Errorf("gamify: leaderboard: %w", err)
	}
	defer rows.Close()

	var (
		out  []LeaderboardEntry
		rank int
	)
	for rows.Next() {
		var e LeaderboardEntry
		if err := rows.Scan(&e.UserID, &e.Name, &e.Points); err != nil {
			return nil, fmt.Errorf("gamify: scan leaderboard: %w", err)
		}
		rank++
		e.Rank = rank
		out = append(out, e)
	}
	return out, rows.Err()
}
