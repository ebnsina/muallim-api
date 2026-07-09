package catalog

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// AddPrerequisite records that course requires requiresID.
func (r *PostgresRepository) AddPrerequisite(ctx context.Context, tx pgx.Tx, tenantID, courseID, requiresID uuid.UUID) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO course_prerequisites (tenant_id, course_id, requires_course_id)
		 VALUES ($1, $2, $3)`,
		tenantID, courseID, requiresID)

	if err != nil {
		if isUniqueViolation(err) {
			return ErrPrerequisiteExists
		}
		return fmt.Errorf("catalog: add prerequisite: %w", err)
	}
	return nil
}

// RemovePrerequisite drops the edge, reporting whether one was there.
func (r *PostgresRepository) RemovePrerequisite(ctx context.Context, tx pgx.Tx, tenantID, courseID, requiresID uuid.UUID) (bool, error) {
	tag, err := tx.Exec(ctx,
		`DELETE FROM course_prerequisites
		 WHERE tenant_id = $1 AND course_id = $2 AND requires_course_id = $3`,
		tenantID, courseID, requiresID)
	if err != nil {
		return false, fmt.Errorf("catalog: remove prerequisite: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

const prerequisitesSQL = `
	SELECT c.id, c.slug, c.title, c.summary, c.difficulty, c.status, c.published_at, c.drip_mode, c.created_at, c.updated_at
	FROM course_prerequisites p
	JOIN courses c ON c.id = p.requires_course_id AND c.tenant_id = p.tenant_id
	WHERE p.tenant_id = $1 AND p.course_id = $2
	ORDER BY c.title, c.id`

// Prerequisites lists the courses a course requires, in one query.
func (r *PostgresRepository) Prerequisites(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID) ([]Course, error) {
	rows, err := tx.Query(ctx, prerequisitesSQL, tenantID, courseID)
	if err != nil {
		return nil, fmt.Errorf("catalog: list prerequisites: %w", err)
	}
	defer rows.Close()

	courses, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Course, error) {
		var c Course
		err := row.Scan(&c.ID, &c.Slug, &c.Title, &c.Summary, &c.Difficulty,
			&c.Status, &c.PublishedAt, &c.DripMode, &c.CreatedAt, &c.UpdatedAt)
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("catalog: scan prerequisites: %w", err)
	}
	return courses, nil
}

// wouldCycleSQL walks the graph outward from the proposed prerequisite and asks
// whether the course itself is reachable.
//
// If it is, adding the edge closes a loop: the course would require something
// that already requires the course. A learner facing that loop cannot start
// anything, and no error message at enrolment time would tell them why.
//
// UNION rather than UNION ALL. It deduplicates, which both bounds the walk on a
// diamond-shaped graph and terminates it on a cycle that already exists — a
// belt-and-braces case this check exists to prevent, but which a corrupted row
// could still present.
const wouldCycleSQL = `
	WITH RECURSIVE reachable(id) AS (
		SELECT $3::uuid
		UNION
		SELECT p.requires_course_id
		FROM course_prerequisites p
		JOIN reachable r ON p.course_id = r.id
		WHERE p.tenant_id = $1
	)
	SELECT EXISTS (SELECT 1 FROM reachable WHERE id = $2)`

// WouldCycle reports whether making requiresID a prerequisite of courseID would
// create a cycle.
func (r *PostgresRepository) WouldCycle(ctx context.Context, tx pgx.Tx, tenantID, courseID, requiresID uuid.UUID) (bool, error) {
	var cycle bool
	if err := tx.QueryRow(ctx, wouldCycleSQL, tenantID, courseID, requiresID).Scan(&cycle); err != nil {
		return false, fmt.Errorf("catalog: check prerequisite cycle: %w", err)
	}
	return cycle, nil
}
