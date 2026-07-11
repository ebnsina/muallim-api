package assess

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// PostgresRepository implements Repository against Postgres.
//
// Every method takes the caller's transaction. None takes a pool: a query outside
// db.WithTenant runs without app.tenant_id bound, and an RLS policy that reads it
// then matches nothing — silently, by returning no rows.
type PostgresRepository struct{}

// NewPostgresRepository returns a repository.
func NewPostgresRepository() *PostgresRepository { return &PostgresRepository{} }

func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23505"
}

func isForeignKeyViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23503"
}

const quizColumns = `id, lesson_id, title, description, time_limit_seconds,
	                 max_attempts, passing_percent, created_at, updated_at`

func scanQuiz(row pgx.Row) (Quiz, error) {
	var q Quiz
	err := row.Scan(&q.ID, &q.LessonID, &q.Title, &q.Description,
		&q.TimeLimitSeconds, &q.MaxAttempts, &q.PassingPercent, &q.CreatedAt, &q.UpdatedAt)
	return q, err
}

// CreateQuiz attaches a quiz to a lesson.
func (r *PostgresRepository) CreateQuiz(ctx context.Context, tx pgx.Tx, tenantID, lessonID uuid.UUID, n NewQuiz) (Quiz, error) {
	quiz, err := scanQuiz(tx.QueryRow(ctx,
		`INSERT INTO quizzes (tenant_id, lesson_id, title, description,
		                      time_limit_seconds, max_attempts, passing_percent)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 RETURNING `+quizColumns,
		tenantID, lessonID, n.Title, n.Description,
		n.TimeLimitSeconds, n.MaxAttempts, n.PassingPercent))

	if err != nil {
		// The unique index on (tenant_id, lesson_id) is what makes "one quiz per
		// lesson" true under concurrency, rather than a check somebody raced.
		if isUniqueViolation(err) {
			return Quiz{}, ErrQuizExists
		}
		if isForeignKeyViolation(err) {
			return Quiz{}, ErrNotFound
		}
		return Quiz{}, fmt.Errorf("assess: create quiz: %w", err)
	}
	return quiz, nil
}

// QuizByLesson loads a lesson's quiz.
func (r *PostgresRepository) QuizByLesson(ctx context.Context, tx pgx.Tx, tenantID, lessonID uuid.UUID) (Quiz, error) {
	quiz, err := scanQuiz(tx.QueryRow(ctx,
		`SELECT `+quizColumns+` FROM quizzes WHERE tenant_id = $1 AND lesson_id = $2`,
		tenantID, lessonID))

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Quiz{}, ErrNotFound
		}
		return Quiz{}, fmt.Errorf("assess: load quiz: %w", err)
	}
	return quiz, nil
}

// QuizByID loads a quiz the grading job already knows the id of.
func (r *PostgresRepository) QuizByID(ctx context.Context, tx pgx.Tx, tenantID, quizID uuid.UUID) (Quiz, error) {
	quiz, err := scanQuiz(tx.QueryRow(ctx,
		`SELECT `+quizColumns+` FROM quizzes WHERE tenant_id = $1 AND id = $2`,
		tenantID, quizID))

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Quiz{}, ErrNotFound
		}
		return Quiz{}, fmt.Errorf("assess: load quiz: %w", err)
	}
	return quiz, nil
}

// CourseSlugForLesson walks a lesson to its course, for a notification's link.
func (r *PostgresRepository) CourseSlugForLesson(ctx context.Context, tx pgx.Tx, tenantID, lessonID uuid.UUID) (string, error) {
	var slug string
	err := tx.QueryRow(ctx,
		`SELECT c.slug
		 FROM lessons l
		 JOIN topics t  ON t.id = l.topic_id
		 JOIN courses c ON c.id = t.course_id
		 WHERE l.tenant_id = $1 AND l.id = $2`,
		tenantID, lessonID).Scan(&slug)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("assess: course slug for lesson: %w", err)
	}
	return slug, nil
}

// UpdateQuiz applies a patch. COALESCE leaves a nil field alone.
func (r *PostgresRepository) UpdateQuiz(ctx context.Context, tx pgx.Tx, tenantID, quizID uuid.UUID, p QuizPatch) (Quiz, error) {
	quiz, err := scanQuiz(tx.QueryRow(ctx,
		`UPDATE quizzes SET
		     title              = coalesce($3, title),
		     description        = coalesce($4, description),
		     time_limit_seconds = coalesce($5, time_limit_seconds),
		     max_attempts       = coalesce($6, max_attempts),
		     passing_percent    = coalesce($7, passing_percent),
		     updated_at         = now()
		 WHERE tenant_id = $1 AND id = $2
		 RETURNING `+quizColumns,
		tenantID, quizID, p.Title, p.Description, p.TimeLimitSeconds, p.MaxAttempts, p.PassingPercent))

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Quiz{}, ErrNotFound
		}
		return Quiz{}, fmt.Errorf("assess: update quiz: %w", err)
	}
	return quiz, nil
}

// DeleteQuiz removes a lesson's quiz, and with it every question and attempt.
func (r *PostgresRepository) DeleteQuiz(ctx context.Context, tx pgx.Tx, tenantID, lessonID uuid.UUID) (uuid.UUID, error) {
	var quizID uuid.UUID
	err := tx.QueryRow(ctx,
		`DELETE FROM quizzes WHERE tenant_id = $1 AND lesson_id = $2 RETURNING id`,
		tenantID, lessonID).Scan(&quizID)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, ErrNotFound
		}
		return uuid.Nil, fmt.Errorf("assess: delete quiz: %w", err)
	}
	return quizID, nil
}

// Questions loads a quiz's questions with their options, in two queries.
//
// Never one query per question. `= ANY($2)` fetches every option of every
// question at once and a map stitches them back together, so a quiz of four
// questions costs what a quiz of four hundred does.
func (r *PostgresRepository) Questions(ctx context.Context, tx pgx.Tx, tenantID, quizID uuid.UUID) ([]Question, error) {
	rows, err := tx.Query(ctx,
		`SELECT id, quiz_id, type, prompt, points, position, explanation, case_sensitive, accepted
		 FROM questions
		 WHERE tenant_id = $1 AND quiz_id = $2
		 ORDER BY position, id`,
		tenantID, quizID)
	if err != nil {
		return nil, fmt.Errorf("assess: load questions: %w", err)
	}

	var questions []Question
	var ids []uuid.UUID

	for rows.Next() {
		var q Question
		var accepted []byte
		if err := rows.Scan(&q.ID, &q.QuizID, &q.Type, &q.Prompt, &q.Points, &q.Position,
			&q.Explanation, &q.CaseSensitive, &accepted); err != nil {
			rows.Close()
			return nil, fmt.Errorf("assess: scan question: %w", err)
		}
		if err := json.Unmarshal(accepted, &q.Accepted); err != nil {
			rows.Close()
			return nil, fmt.Errorf("assess: decode accepted answers of question %s: %w", q.ID, err)
		}
		questions = append(questions, q)
		ids = append(ids, q.ID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("assess: load questions: %w", err)
	}

	if len(questions) == 0 {
		return nil, nil
	}

	options, err := r.optionsFor(ctx, tx, tenantID, ids)
	if err != nil {
		return nil, err
	}
	for i := range questions {
		questions[i].Options = options[questions[i].ID]
	}
	return questions, nil
}

func (r *PostgresRepository) optionsFor(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, questionIDs []uuid.UUID) (map[uuid.UUID][]Option, error) {
	rows, err := tx.Query(ctx,
		`SELECT id, question_id, content, position, is_correct, match_id, match_content
		 FROM question_options
		 WHERE tenant_id = $1 AND question_id = ANY($2)
		 ORDER BY question_id, position, id`,
		tenantID, questionIDs)
	if err != nil {
		return nil, fmt.Errorf("assess: load options: %w", err)
	}
	defer rows.Close()

	options := make(map[uuid.UUID][]Option, len(questionIDs))
	for rows.Next() {
		var o Option
		if err := rows.Scan(&o.ID, &o.QuestionID, &o.Content, &o.Position,
			&o.IsCorrect, &o.MatchID, &o.MatchContent); err != nil {
			return nil, fmt.Errorf("assess: scan option: %w", err)
		}
		options[o.QuestionID] = append(options[o.QuestionID], o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("assess: load options: %w", err)
	}
	return options, nil
}

// CreateQuestion appends a question and its options.
//
// The position is computed inside the insert, so two concurrent appends cannot
// read the same max. The options go in as one statement built from arrays, not as
// one INSERT per option.
func (r *PostgresRepository) CreateQuestion(ctx context.Context, tx pgx.Tx, tenantID, quizID uuid.UUID, n NewQuestion) (Question, error) {
	accepted, err := json.Marshal(acceptedOrEmpty(n.Accepted))
	if err != nil {
		return Question{}, fmt.Errorf("assess: encode accepted answers: %w", err)
	}

	var q Question
	err = tx.QueryRow(ctx,
		`INSERT INTO questions (tenant_id, quiz_id, type, prompt, points, explanation,
		                        case_sensitive, accepted, position)
		 SELECT $1, $2, $3, $4, $5, $6, $7, $8, coalesce(max(position) + 1, 0)
		 FROM questions WHERE tenant_id = $1 AND quiz_id = $2
		 RETURNING id, quiz_id, type, prompt, points, position, explanation, case_sensitive`,
		tenantID, quizID, n.Type, n.Prompt, n.Points, n.Explanation, n.CaseSensitive, accepted).
		Scan(&q.ID, &q.QuizID, &q.Type, &q.Prompt, &q.Points, &q.Position, &q.Explanation, &q.CaseSensitive)

	if err != nil {
		if isForeignKeyViolation(err) {
			return Question{}, ErrNotFound
		}
		return Question{}, fmt.Errorf("assess: create question: %w", err)
	}
	q.Accepted = n.Accepted

	if len(n.Options) == 0 {
		return q, nil
	}

	contents := make([]string, len(n.Options))
	correct := make([]bool, len(n.Options))
	matches := make([]string, len(n.Options))
	for i, o := range n.Options {
		contents[i], correct[i], matches[i] = o.Content, o.IsCorrect, o.MatchContent
	}

	rows, err := tx.Query(ctx,
		`INSERT INTO question_options (tenant_id, question_id, content, is_correct, match_content, position)
		 SELECT $1, $2, v.content, v.is_correct, v.match_content, v.rank - 1
		 FROM unnest($3::text[], $4::boolean[], $5::text[]) WITH ORDINALITY AS v(content, is_correct, match_content, rank)
		 RETURNING id, question_id, content, position, is_correct, match_id, match_content`,
		tenantID, q.ID, contents, correct, matches)
	if err != nil {
		return Question{}, fmt.Errorf("assess: create options: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var o Option
		if err := rows.Scan(&o.ID, &o.QuestionID, &o.Content, &o.Position,
			&o.IsCorrect, &o.MatchID, &o.MatchContent); err != nil {
			return Question{}, fmt.Errorf("assess: scan option: %w", err)
		}
		q.Options = append(q.Options, o)
	}
	if err := rows.Err(); err != nil {
		return Question{}, fmt.Errorf("assess: create options: %w", err)
	}
	return q, nil
}

// acceptedOrEmpty keeps a nil slice out of the column: `null` would fail the
// jsonb_typeof(accepted) = 'array' check, and `[]` is what "no blanks" means.
func acceptedOrEmpty(a [][]string) [][]string {
	if a == nil {
		return [][]string{}
	}
	return a
}

// DeleteQuestion removes a question and closes the gap it leaves, in one
// statement each, and returns the quiz it belonged to.
func (r *PostgresRepository) DeleteQuestion(ctx context.Context, tx pgx.Tx, tenantID, questionID uuid.UUID) (uuid.UUID, error) {
	var quizID uuid.UUID
	var position int

	err := tx.QueryRow(ctx,
		`DELETE FROM questions WHERE tenant_id = $1 AND id = $2 RETURNING quiz_id, position`,
		tenantID, questionID).Scan(&quizID, &position)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, ErrNotFound
		}
		return uuid.Nil, fmt.Errorf("assess: delete question: %w", err)
	}

	// Positions stay dense. The gap closes in the same transaction, so no reader
	// ever sees a quiz numbered 0, 1, 3.
	if _, err := tx.Exec(ctx,
		`UPDATE questions SET position = position - 1
		 WHERE tenant_id = $1 AND quiz_id = $2 AND position > $3`,
		tenantID, quizID, position); err != nil {
		return uuid.Nil, fmt.Errorf("assess: close position gap: %w", err)
	}
	return quizID, nil
}

// ReorderQuestions sets the order of a quiz's questions.
//
// The submitted list must name every question exactly once, or the order is
// refused rather than half-applied. The unique constraint on
// (tenant_id, quiz_id, position) is DEFERRABLE because the statement passes
// through states where two rows share a position.
func (r *PostgresRepository) ReorderQuestions(ctx context.Context, tx pgx.Tx, tenantID, quizID uuid.UUID, order []uuid.UUID) error {
	var siblings int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM questions WHERE tenant_id = $1 AND quiz_id = $2`,
		tenantID, quizID).Scan(&siblings); err != nil {
		return fmt.Errorf("assess: count questions: %w", err)
	}

	seen := make(map[uuid.UUID]struct{}, len(order))
	for _, id := range order {
		seen[id] = struct{}{}
	}
	if len(order) != siblings || len(seen) != siblings {
		return fmt.Errorf("%w: %d ids for %d questions", ErrIncompleteOrder, len(seen), siblings)
	}

	tag, err := tx.Exec(ctx,
		`UPDATE questions q SET position = v.rank - 1, updated_at = now()
		 FROM unnest($3::uuid[]) WITH ORDINALITY AS v(id, rank)
		 WHERE q.tenant_id = $1 AND q.quiz_id = $2 AND q.id = v.id`,
		tenantID, quizID, order)
	if err != nil {
		return fmt.Errorf("assess: reorder questions: %w", err)
	}
	if int(tag.RowsAffected()) != len(order) {
		return fmt.Errorf("%w: %d of %d questions matched", ErrIncompleteOrder, tag.RowsAffected(), len(order))
	}
	return nil
}

const attemptColumns = `id, quiz_id, user_id, number, status, started_at,
	                    submitted_at, graded_at, expires_at, points, max_points, passed`

func scanAttempt(row pgx.Row) (Attempt, error) {
	var a Attempt
	err := row.Scan(&a.ID, &a.QuizID, &a.UserID, &a.Number, &a.Status, &a.StartedAt,
		&a.SubmittedAt, &a.GradedAt, &a.ExpiresAt, &a.Points, &a.MaxPoints, &a.Passed)
	return a, err
}

// StartAttempt opens the learner's next attempt.
//
// The number is `max + 1` computed inside the insert, and the partial unique
// index on (tenant_id, quiz_id, user_id) WHERE status = 'in_progress' is what
// makes "one live attempt" true. Two tabs pressing Start together do not both
// win a read-then-write; one of them gets a unique violation, and the caller
// returns the attempt that exists.
func (r *PostgresRepository) StartAttempt(ctx context.Context, tx pgx.Tx, tenantID, quizID, userID uuid.UUID, expiresAt *time.Time) (Attempt, error) {
	attempt, err := scanAttempt(tx.QueryRow(ctx,
		`INSERT INTO quiz_attempts (tenant_id, quiz_id, user_id, expires_at, number)
		 SELECT $1, $2, $3, $4, coalesce(max(number) + 1, 1)
		 FROM quiz_attempts WHERE tenant_id = $1 AND quiz_id = $2 AND user_id = $3
		 RETURNING `+attemptColumns,
		tenantID, quizID, userID, expiresAt))

	if err != nil {
		if isUniqueViolation(err) {
			return Attempt{}, ErrAttemptInProgress
		}
		if isForeignKeyViolation(err) {
			return Attempt{}, ErrNotFound
		}
		return Attempt{}, fmt.Errorf("assess: start attempt: %w", err)
	}
	return attempt, nil
}

// LiveAttempt returns the learner's in-progress attempt at a quiz.
func (r *PostgresRepository) LiveAttempt(ctx context.Context, tx pgx.Tx, tenantID, quizID, userID uuid.UUID) (Attempt, error) {
	attempt, err := scanAttempt(tx.QueryRow(ctx,
		`SELECT `+attemptColumns+`
		 FROM quiz_attempts
		 WHERE tenant_id = $1 AND quiz_id = $2 AND user_id = $3 AND status = 'in_progress'`,
		tenantID, quizID, userID))

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Attempt{}, ErrNotFound
		}
		return Attempt{}, fmt.Errorf("assess: load live attempt: %w", err)
	}
	return attempt, nil
}

// AttemptByNumber loads one of a learner's attempts.
//
// Addressed by (quiz, learner, number) rather than by an id, so there is no
// identifier a client could guess into somebody else's result.
func (r *PostgresRepository) AttemptByNumber(ctx context.Context, tx pgx.Tx, tenantID, quizID, userID uuid.UUID, number int) (Attempt, error) {
	attempt, err := scanAttempt(tx.QueryRow(ctx,
		`SELECT `+attemptColumns+`
		 FROM quiz_attempts
		 WHERE tenant_id = $1 AND quiz_id = $2 AND user_id = $3 AND number = $4`,
		tenantID, quizID, userID, number))

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Attempt{}, ErrNotFound
		}
		return Attempt{}, fmt.Errorf("assess: load attempt: %w", err)
	}
	return attempt, nil
}

// AttemptByID loads an attempt for the grading job, which has only an id.
func (r *PostgresRepository) AttemptByID(ctx context.Context, tx pgx.Tx, tenantID, attemptID uuid.UUID) (Attempt, error) {
	attempt, err := scanAttempt(tx.QueryRow(ctx,
		`SELECT `+attemptColumns+` FROM quiz_attempts WHERE tenant_id = $1 AND id = $2`,
		tenantID, attemptID))

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Attempt{}, ErrNotFound
		}
		return Attempt{}, fmt.Errorf("assess: load attempt: %w", err)
	}
	return attempt, nil
}

// ListAttempts returns a learner's attempts at a quiz, newest first.
func (r *PostgresRepository) ListAttempts(ctx context.Context, tx pgx.Tx, tenantID, quizID, userID uuid.UUID, limit int) ([]Attempt, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+attemptColumns+`
		 FROM quiz_attempts
		 WHERE tenant_id = $1 AND quiz_id = $2 AND user_id = $3
		 ORDER BY number DESC
		 LIMIT $4`,
		tenantID, quizID, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("assess: list attempts: %w", err)
	}
	defer rows.Close()

	var attempts []Attempt
	for rows.Next() {
		attempt, err := scanAttempt(rows)
		if err != nil {
			return nil, fmt.Errorf("assess: scan attempt: %w", err)
		}
		attempts = append(attempts, attempt)
	}
	return attempts, rows.Err()
}

// SaveAnswer records the learner's response to one question.
//
// The INSERT selects from `questions`, so a question belonging to another quiz
// produces no row and no answer. It is an upsert on (attempt, question): a
// learner may change their mind until they submit, and doing so replaces the
// answer rather than adding a second one.
//
// The `status = 'in_progress'` predicate is inside the statement. A check read
// beforehand would be a race with the submit that closes the attempt.
func (r *PostgresRepository) SaveAnswer(ctx context.Context, tx pgx.Tx, tenantID, attemptID, questionID uuid.UUID, response Response) error {
	encoded, err := json.Marshal(response)
	if err != nil {
		return fmt.Errorf("assess: encode response: %w", err)
	}

	tag, err := tx.Exec(ctx,
		`INSERT INTO attempt_answers (tenant_id, attempt_id, question_id, response)
		 SELECT $1, a.id, q.id, $4
		 FROM quiz_attempts a
		 JOIN quizzes qz ON qz.id = a.quiz_id AND qz.tenant_id = a.tenant_id
		 JOIN questions q ON q.quiz_id = qz.id AND q.tenant_id = a.tenant_id
		 WHERE a.tenant_id = $1 AND a.id = $2 AND q.id = $3 AND a.status = 'in_progress'
		 ON CONFLICT (tenant_id, attempt_id, question_id)
		 DO UPDATE SET response = excluded.response, updated_at = now()`,
		tenantID, attemptID, questionID, encoded)
	if err != nil {
		return fmt.Errorf("assess: save answer: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Either the question is not on this quiz, or the attempt is closed. The
		// caller has already established which by loading the attempt.
		return ErrNotFound
	}
	return nil
}

// CloseAttempt moves an attempt from in_progress to grading.
//
// The transition is the WHERE clause. A second submit — a double-clicked button,
// a retried request — matches no row and is told the attempt is closed, so only
// one grading job is ever enqueued alongside it.
func (r *PostgresRepository) CloseAttempt(ctx context.Context, tx pgx.Tx, tenantID, attemptID uuid.UUID) (Attempt, error) {
	attempt, err := scanAttempt(tx.QueryRow(ctx,
		`UPDATE quiz_attempts
		 SET status = 'grading', submitted_at = now()
		 WHERE tenant_id = $1 AND id = $2 AND status = 'in_progress'
		 RETURNING `+attemptColumns,
		tenantID, attemptID))

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Attempt{}, ErrAttemptClosed
		}
		return Attempt{}, fmt.Errorf("assess: close attempt: %w", err)
	}
	return attempt, nil
}

// Responses loads what the learner answered, keyed by question.
func (r *PostgresRepository) Responses(ctx context.Context, tx pgx.Tx, tenantID, attemptID uuid.UUID) (map[uuid.UUID]Response, error) {
	rows, err := tx.Query(ctx,
		`SELECT question_id, response FROM attempt_answers WHERE tenant_id = $1 AND attempt_id = $2`,
		tenantID, attemptID)
	if err != nil {
		return nil, fmt.Errorf("assess: load responses: %w", err)
	}
	defer rows.Close()

	responses := make(map[uuid.UUID]Response)
	for rows.Next() {
		var questionID uuid.UUID
		var encoded []byte
		if err := rows.Scan(&questionID, &encoded); err != nil {
			return nil, fmt.Errorf("assess: scan response: %w", err)
		}

		var response Response
		if err := json.Unmarshal(encoded, &response); err != nil {
			return nil, fmt.Errorf("assess: decode response to question %s: %w", questionID, err)
		}
		responses[questionID] = response
	}
	return responses, rows.Err()
}

// WriteGrades records one verdict per question, in a single statement.
//
// An upsert, and idempotent: a retried grading job recomputes the same verdicts
// from the same rows and writes them again. A question the learner never answered
// gets a row here too, so the review can say "you left this blank" rather than
// leaving a hole a client has to interpret.
//
// The manual ones are written ungraded, with no points, waiting for a person.
func (r *PostgresRepository) WriteGrades(ctx context.Context, tx pgx.Tx, tenantID, attemptID uuid.UUID, grades []AnswerGrade) error {
	if len(grades) == 0 {
		return nil
	}

	questionIDs := make([]uuid.UUID, len(grades))
	graded := make([]bool, len(grades))
	correct := make([]bool, len(grades))
	points := make([]int, len(grades))

	for i, g := range grades {
		questionIDs[i] = g.QuestionID
		graded[i] = !g.Verdict.Manual
		correct[i] = g.Verdict.Correct
		points[i] = g.Verdict.Points
	}

	_, err := tx.Exec(ctx,
		`INSERT INTO attempt_answers (tenant_id, attempt_id, question_id, graded, correct, points, graded_at)
		 SELECT $1, $2, v.question_id, v.graded, v.correct, v.points,
		        CASE WHEN v.graded THEN now() END
		 FROM unnest($3::uuid[], $4::boolean[], $5::boolean[], $6::integer[])
		      AS v(question_id, graded, correct, points)
		 ON CONFLICT (tenant_id, attempt_id, question_id) DO UPDATE SET
		     graded    = excluded.graded,
		     correct   = excluded.correct,
		     points    = excluded.points,
		     graded_at = excluded.graded_at,
		     updated_at = now()`,
		tenantID, attemptID, questionIDs, graded, correct, points)
	if err != nil {
		return fmt.Errorf("assess: write grades: %w", err)
	}
	return nil
}

// FinishGrading records the attempt's score and its final status.
func (r *PostgresRepository) FinishGrading(ctx context.Context, tx pgx.Tx, tenantID, attemptID uuid.UUID, status string, points, maxPoints int, passed *bool) (Attempt, error) {
	attempt, err := scanAttempt(tx.QueryRow(ctx,
		`UPDATE quiz_attempts
		 SET status = $3, points = $4, max_points = $5, passed = $6,
		     graded_at = CASE WHEN $3 = 'graded' THEN now() ELSE graded_at END
		 WHERE tenant_id = $1 AND id = $2
		 RETURNING `+attemptColumns,
		tenantID, attemptID, status, points, maxPoints, passed))

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Attempt{}, ErrNotFound
		}
		return Attempt{}, fmt.Errorf("assess: finish grading: %w", err)
	}
	return attempt, nil
}

// ReviewItems returns every question of an attempt's quiz beside the learner's
// answer and its verdict, in one query.
//
// A LEFT JOIN, because a question the learner never reached has no answer row
// until grading writes one, and a review that silently dropped it would be a
// review with holes in it.
//
// The explanation is selected here and released by the service only once the
// attempt has been graded.
func (r *PostgresRepository) ReviewItems(ctx context.Context, tx pgx.Tx, tenantID, attemptID uuid.UUID) ([]ReviewItem, error) {
	rows, err := tx.Query(ctx,
		`SELECT q.id, q.prompt, q.type, q.position, q.points, q.explanation,
		        coalesce(aa.response, '{}'::jsonb),
		        coalesce(aa.graded, false), coalesce(aa.correct, false),
		        coalesce(aa.points, 0), coalesce(aa.feedback, '')
		 FROM quiz_attempts a
		 JOIN questions q ON q.quiz_id = a.quiz_id AND q.tenant_id = a.tenant_id
		 LEFT JOIN attempt_answers aa
		        ON aa.tenant_id = a.tenant_id AND aa.attempt_id = a.id AND aa.question_id = q.id
		 WHERE a.tenant_id = $1 AND a.id = $2
		 ORDER BY q.position, q.id`,
		tenantID, attemptID)
	if err != nil {
		return nil, fmt.Errorf("assess: load review: %w", err)
	}
	defer rows.Close()

	var items []ReviewItem
	for rows.Next() {
		var item ReviewItem
		var encoded []byte
		if err := rows.Scan(&item.QuestionID, &item.Prompt, &item.Type, &item.Position,
			&item.MaxPoints, &item.Explanation, &encoded,
			&item.Graded, &item.Correct, &item.Points, &item.Feedback); err != nil {
			return nil, fmt.Errorf("assess: scan review item: %w", err)
		}
		if err := json.Unmarshal(encoded, &item.Response); err != nil {
			return nil, fmt.Errorf("assess: decode response to question %s: %w", item.QuestionID, err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}
