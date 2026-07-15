package assess

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ListSubmissions returns the attempts at a quiz for the person marking them.
//
// One query. The unmarked count comes from a lateral subquery over this attempt's
// own answers rather than from a second round trip per row, which is what turns a
// marking queue into an N+1.
//
// `submitted_at IS NOT NULL` excludes the attempts still being taken: a marker
// has no business reading a half-written essay, and the partial indexes that
// serve this query cover exactly the rows it wants.
func (r *PostgresRepository) ListSubmissions(ctx context.Context, tx pgx.Tx, tenantID, quizID uuid.UUID, onlyAwaiting bool, limit int) ([]Submission, error) {
	rows, err := tx.Query(ctx,
		`SELECT a.id, a.quiz_id, a.user_id, a.number, a.status, a.started_at,
		        a.submitted_at, a.graded_at, a.expires_at, a.points, a.max_points, a.passed,
		        u.name, u.email, s.unmarked
		 FROM quiz_attempts a
		 JOIN users u ON u.id = a.user_id
		 CROSS JOIN LATERAL (
		     SELECT count(*) AS unmarked
		     FROM attempt_answers aa
		     WHERE aa.tenant_id = a.tenant_id AND aa.attempt_id = a.id AND NOT aa.graded
		 ) s
		 WHERE a.tenant_id = $1 AND a.quiz_id = $2
		   AND a.submitted_at IS NOT NULL
		   AND ($3 = false OR a.status = 'awaiting_review')
		 ORDER BY a.submitted_at DESC, a.id DESC
		 LIMIT $4`,
		tenantID, quizID, onlyAwaiting, limit)
	if err != nil {
		return nil, fmt.Errorf("assess: list submissions: %w", err)
	}
	defer rows.Close()

	var submissions []Submission
	for rows.Next() {
		var s Submission
		var a Attempt
		if err := rows.Scan(&a.ID, &a.QuizID, &a.UserID, &a.Number, &a.Status, &a.StartedAt,
			&a.SubmittedAt, &a.GradedAt, &a.ExpiresAt, &a.Points, &a.MaxPoints, &a.Passed,
			&s.LearnerName, &s.LearnerEmail, &s.Unmarked); err != nil {
			return nil, fmt.Errorf("assess: scan submission: %w", err)
		}
		s.Attempt = a
		submissions = append(submissions, s)
	}
	return submissions, rows.Err()
}

// MarkAnswer records a verdict on one open-ended answer.
//
// The type check and the attempt's status are both in the statement. The service
// has checked them too, and says something useful about each; these are what make
// it true. A concurrent submission of another mark cannot slip a machine-graded
// question past the first check and into the write.
func (r *PostgresRepository) MarkAnswer(ctx context.Context, tx pgx.Tx, tenantID, attemptID, questionID uuid.UUID, m Mark, correct bool) error {
	tag, err := tx.Exec(ctx,
		`UPDATE attempt_answers aa
		 SET graded = true, correct = $6, points = $5, feedback = $4,
		     graded_at = now(), updated_at = now()
		 FROM questions q, quiz_attempts a
		 WHERE aa.tenant_id = $1 AND aa.attempt_id = $2 AND aa.question_id = $3
		   AND q.tenant_id = aa.tenant_id AND q.id = aa.question_id
		   AND q.type IN ('open_ended', 'draw_image') AND $5 <= q.points
		   AND a.tenant_id = aa.tenant_id AND a.id = aa.attempt_id
		   AND a.status = 'awaiting_review'`,
		tenantID, attemptID, questionID, m.Feedback, m.Points, correct)
	if err != nil {
		return fmt.Errorf("assess: mark answer: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// recomputeAttemptSQL restates an attempt from the rows that summarise it.
//
// One statement, so the score can never disagree with the answers. `max_points`
// is deliberately *not* recomputed: it was frozen when the machine graded the
// attempt, and an author who edits the quiz while an essay waits to be marked
// must not thereby restate what the learner was scored out of.
//
// A quiz worth nothing is passed by whoever attempted it — the only reading of
// "you scored all of the available points" that is not a lie — and the arithmetic
// is integers throughout, so a score exactly on the bar clears it.
const recomputeAttemptSQL = `
	UPDATE quiz_attempts a SET
	    points = s.points,
	    status = CASE WHEN s.unmarked > 0 THEN 'awaiting_review' ELSE 'graded' END,
	    passed = CASE
	        WHEN s.unmarked > 0        THEN NULL
	        WHEN a.max_points <= 0     THEN true
	        ELSE s.points * 100 >= q.passing_percent * a.max_points
	    END,
	    graded_at = CASE
	        WHEN s.unmarked > 0 THEN NULL
	        ELSE coalesce(a.graded_at, now())
	    END
	FROM quizzes q,
	     -- Not LATERAL: Postgres forbids a FROM item of an UPDATE from referencing
	     -- the row being updated. It needs no such reference — the attempt is named
	     -- by the parameters.
	     (
	         SELECT coalesce(sum(aa.points), 0)           AS points,
	                count(*) FILTER (WHERE NOT aa.graded) AS unmarked
	         FROM attempt_answers aa
	         WHERE aa.tenant_id = $1 AND aa.attempt_id = $2
	     ) s
	WHERE a.tenant_id = $1 AND a.id = $2
	  AND q.tenant_id = a.tenant_id AND q.id = a.quiz_id
	RETURNING a.id, a.quiz_id, a.user_id, a.number, a.status, a.started_at,
	          a.submitted_at, a.graded_at, a.expires_at, a.points, a.max_points, a.passed`

// RecomputeAttempt restates an attempt's score and status from its answers.
func (r *PostgresRepository) RecomputeAttempt(ctx context.Context, tx pgx.Tx, tenantID, attemptID uuid.UUID) (Attempt, error) {
	attempt, err := scanAttempt(tx.QueryRow(ctx, recomputeAttemptSQL, tenantID, attemptID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Attempt{}, ErrNotFound
		}
		return Attempt{}, fmt.Errorf("assess: recompute attempt: %w", err)
	}
	return attempt, nil
}
