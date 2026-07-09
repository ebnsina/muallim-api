package enroll

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// missingPrerequisitesSQL names the prerequisite courses this learner has not
// completed.
//
// One query, not one per prerequisite. The LEFT JOIN carries the learner's
// completion for each required course, and `e.id IS NULL` keeps the ones they
// have not finished — including the ones they never enrolled on, which is the
// common case and would be a second query in any other shape.
//
// "Completed" is the enrolment's own status, set when the last lesson is marked
// done and the roll-up is recomputed in that same transaction. A learner who
// enrolled and stopped halfway has not finished it.
const missingPrerequisitesSQL = `
	SELECT c.slug, c.title
	FROM course_prerequisites p
	JOIN courses c
	  ON c.id = p.requires_course_id AND c.tenant_id = p.tenant_id
	LEFT JOIN enrolments e
	  ON e.tenant_id = p.tenant_id
	 AND e.course_id = p.requires_course_id
	 AND e.user_id = $3
	 AND e.status = 'completed'
	WHERE p.tenant_id = $1 AND p.course_id = $2 AND e.id IS NULL
	ORDER BY c.title, c.id`

// MissingPrerequisites lists the courses a learner must finish first.
//
// An anonymous reader (uuid.Nil) matches no enrolment, so every prerequisite
// comes back missing. That is the right answer and needs no second query shape.
func (r *PostgresRepository) MissingPrerequisites(ctx context.Context, tx pgx.Tx, tenantID, courseID, userID uuid.UUID) ([]MissingCourse, error) {
	rows, err := tx.Query(ctx, missingPrerequisitesSQL, tenantID, courseID, userID)
	if err != nil {
		return nil, fmt.Errorf("enroll: list missing prerequisites: %w", err)
	}
	defer rows.Close()

	missing, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (MissingCourse, error) {
		var m MissingCourse
		err := row.Scan(&m.Slug, &m.Title)
		return m, err
	})
	if err != nil {
		return nil, fmt.Errorf("enroll: scan missing prerequisites: %w", err)
	}
	return missing, nil
}
