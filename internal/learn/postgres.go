package learn

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// PostgresRepository is the notes table.
type PostgresRepository struct{}

// NewPostgresRepository returns one.
func NewPostgresRepository() *PostgresRepository { return &PostgresRepository{} }

// foreignKeyViolation is Postgres 23503: a note pointed at a lesson id that is not
// in the lessons table. The tenant filter and RLS mean the same thing here —
// there is no such lesson in this workspace.
func foreignKeyViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23503"
}

const noteSQL = `
	SELECT lesson_id, body, updated_at
	FROM lesson_notes
	WHERE tenant_id = $1 AND user_id = $2 AND lesson_id = $3`

// Note reads the caller's note, if there is one. The unique index on
// (tenant_id, user_id, lesson_id) makes this a single-row index lookup.
func (r *PostgresRepository) Note(ctx context.Context, tx pgx.Tx, tenantID, userID, lessonID uuid.UUID) (Note, bool, error) {
	var note Note
	err := tx.QueryRow(ctx, noteSQL, tenantID, userID, lessonID).
		Scan(&note.LessonID, &note.Body, &note.UpdatedAt)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Note{}, false, nil
		}
		return Note{}, false, fmt.Errorf("learn: read note: %w", err)
	}
	return note, true, nil
}

// upsertNoteSQL writes the note, or replaces the body of the one that is there.
// `updated_at` is set on a replace so the row records when it last changed, not
// when it was first written.
const upsertNoteSQL = `
	INSERT INTO lesson_notes (tenant_id, user_id, lesson_id, body)
	VALUES ($1, $2, $3, $4)
	ON CONFLICT (tenant_id, user_id, lesson_id)
	DO UPDATE SET body = EXCLUDED.body, updated_at = now()
	RETURNING lesson_id, body, updated_at`

// Upsert writes the note. A lesson id with no lesson behind it trips the foreign
// key, which becomes ErrLessonNotFound rather than a leaked 500.
func (r *PostgresRepository) Upsert(ctx context.Context, tx pgx.Tx, tenantID, userID, lessonID uuid.UUID, body string) (Note, error) {
	var note Note
	err := tx.QueryRow(ctx, upsertNoteSQL, tenantID, userID, lessonID, body).
		Scan(&note.LessonID, &note.Body, &note.UpdatedAt)

	if err != nil {
		if foreignKeyViolation(err) {
			return Note{}, ErrLessonNotFound
		}
		return Note{}, fmt.Errorf("learn: save note: %w", err)
	}
	return note, nil
}

const deleteNoteSQL = `
	DELETE FROM lesson_notes
	WHERE tenant_id = $1 AND user_id = $2 AND lesson_id = $3`

// Delete removes the caller's note. Deleting one that is not there affects no
// rows and is not an error — the asked-for end state is "no note".
func (r *PostgresRepository) Delete(ctx context.Context, tx pgx.Tx, tenantID, userID, lessonID uuid.UUID) error {
	if _, err := tx.Exec(ctx, deleteNoteSQL, tenantID, userID, lessonID); err != nil {
		return fmt.Errorf("learn: delete note: %w", err)
	}
	return nil
}

const highlightsSQL = `
	SELECT id, lesson_id, quote, note, start_offset, end_offset, created_at, updated_at
	FROM lesson_highlights
	WHERE tenant_id = $1 AND user_id = $2 AND lesson_id = $3
	ORDER BY start_offset, id`

// Highlights lists the caller's marks on a lesson, walking
// lesson_highlights_learner_lesson_idx in reading order.
func (r *PostgresRepository) Highlights(ctx context.Context, tx pgx.Tx, tenantID, userID, lessonID uuid.UUID) ([]Highlight, error) {
	rows, err := tx.Query(ctx, highlightsSQL, tenantID, userID, lessonID)
	if err != nil {
		return nil, fmt.Errorf("learn: list highlights: %w", err)
	}
	defer rows.Close()

	highlights, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Highlight, error) {
		var h Highlight
		err := row.Scan(&h.ID, &h.LessonID, &h.Quote, &h.Note, &h.Start, &h.End, &h.CreatedAt, &h.UpdatedAt)
		return h, err
	})
	if err != nil {
		return nil, fmt.Errorf("learn: scan highlights: %w", err)
	}
	return highlights, nil
}

const addHighlightSQL = `
	INSERT INTO lesson_highlights (tenant_id, user_id, lesson_id, quote, note, start_offset, end_offset)
	VALUES ($1, $2, $3, $4, $5, $6, $7)
	RETURNING id, lesson_id, quote, note, start_offset, end_offset, created_at, updated_at`

// AddHighlight writes a mark. A lesson id with no lesson behind it trips the
// foreign key, which becomes ErrLessonNotFound rather than a leaked 500.
func (r *PostgresRepository) AddHighlight(ctx context.Context, tx pgx.Tx, tenantID, userID, lessonID uuid.UUID, h Highlight) (Highlight, error) {
	var out Highlight
	err := tx.QueryRow(ctx, addHighlightSQL, tenantID, userID, lessonID, h.Quote, h.Note, h.Start, h.End).
		Scan(&out.ID, &out.LessonID, &out.Quote, &out.Note, &out.Start, &out.End, &out.CreatedAt, &out.UpdatedAt)

	if err != nil {
		if foreignKeyViolation(err) {
			return Highlight{}, ErrLessonNotFound
		}
		return Highlight{}, fmt.Errorf("learn: add highlight: %w", err)
	}
	return out, nil
}

const updateHighlightNoteSQL = `
	UPDATE lesson_highlights SET note = $4, updated_at = now()
	WHERE tenant_id = $1 AND user_id = $2 AND id = $3`

// UpdateHighlightNote sets the note on the caller's own mark. The user id in the
// WHERE clause is what makes "not found" and "not yours" the same row count: zero.
func (r *PostgresRepository) UpdateHighlightNote(ctx context.Context, tx pgx.Tx, tenantID, userID, highlightID uuid.UUID, note string) (bool, error) {
	tag, err := tx.Exec(ctx, updateHighlightNoteSQL, tenantID, userID, highlightID, note)
	if err != nil {
		return false, fmt.Errorf("learn: update highlight note: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

const deleteHighlightSQL = `
	DELETE FROM lesson_highlights
	WHERE tenant_id = $1 AND user_id = $2 AND id = $3`

// DeleteHighlight removes the caller's own mark, scoped by user id so one
// learner's id cannot reach another's row.
func (r *PostgresRepository) DeleteHighlight(ctx context.Context, tx pgx.Tx, tenantID, userID, highlightID uuid.UUID) (bool, error) {
	tag, err := tx.Exec(ctx, deleteHighlightSQL, tenantID, userID, highlightID)
	if err != nil {
		return false, fmt.Errorf("learn: delete highlight: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// The course-wide reads resolve the course by slug and walk to its lessons
// through topics — reading the catalog's tables, not importing its package, the
// way certify reads users and courses to copy a name onto a certificate.
const notesForCourseSQL = `
	SELECT n.lesson_id, n.body, n.updated_at
	FROM lesson_notes n
	JOIN lessons l ON l.id = n.lesson_id
	JOIN topics t  ON t.id = l.topic_id
	JOIN courses c ON c.id = t.course_id
	WHERE n.tenant_id = $1 AND n.user_id = $2 AND lower(c.slug) = lower($3)`

// NotesForCourse lists the caller's notes across a course's lessons.
func (r *PostgresRepository) NotesForCourse(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID, slug string) ([]Note, error) {
	rows, err := tx.Query(ctx, notesForCourseSQL, tenantID, userID, slug)
	if err != nil {
		return nil, fmt.Errorf("learn: notes for course: %w", err)
	}
	defer rows.Close()

	notes, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Note, error) {
		var n Note
		err := row.Scan(&n.LessonID, &n.Body, &n.UpdatedAt)
		return n, err
	})
	if err != nil {
		return nil, fmt.Errorf("learn: scan course notes: %w", err)
	}
	return notes, nil
}

const highlightsForCourseSQL = `
	SELECT h.id, h.lesson_id, h.quote, h.note, h.start_offset, h.end_offset, h.created_at, h.updated_at
	FROM lesson_highlights h
	JOIN lessons l ON l.id = h.lesson_id
	JOIN topics t  ON t.id = l.topic_id
	JOIN courses c ON c.id = t.course_id
	WHERE h.tenant_id = $1 AND h.user_id = $2 AND lower(c.slug) = lower($3)
	ORDER BY h.lesson_id, h.start_offset, h.id`

// HighlightsForCourse lists the caller's marks across a course's lessons.
func (r *PostgresRepository) HighlightsForCourse(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID, slug string) ([]Highlight, error) {
	rows, err := tx.Query(ctx, highlightsForCourseSQL, tenantID, userID, slug)
	if err != nil {
		return nil, fmt.Errorf("learn: highlights for course: %w", err)
	}
	defer rows.Close()

	highlights, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Highlight, error) {
		var h Highlight
		err := row.Scan(&h.ID, &h.LessonID, &h.Quote, &h.Note, &h.Start, &h.End, &h.CreatedAt, &h.UpdatedAt)
		return h, err
	})
	if err != nil {
		return nil, fmt.Errorf("learn: scan course highlights: %w", err)
	}
	return highlights, nil
}

// lessonReadableGuard is true when (userID, canModerate) may read the lesson.
//
// A moderator always may. Otherwise the lesson must be a preview of a published
// course — readable by anyone — or the user must hold a live enrolment in the
// course that owns it. It reads lessons, topics, courses, and enrolments
// directly, the same shared-schema read the course-wide lists use; it does not
// gate on drip, because a released-later lesson is still one you may discuss.
//
// Parameterised as $tenant, $lesson, $user, $canModerate in that order, so a
// caller substitutes the placeholder numbers to match its own query.
const lessonReadableGuard = `
	EXISTS (
		SELECT 1 FROM lessons l
		JOIN topics t  ON t.id = l.topic_id
		JOIN courses c ON c.id = t.course_id
		WHERE l.id = %[2]s AND l.tenant_id = %[1]s
		  AND (
		      %[4]s
		      OR (l.is_preview AND c.status = 'published')
		      OR EXISTS (
		          SELECT 1 FROM enrolments e
		          WHERE e.tenant_id = %[1]s AND e.course_id = c.id AND e.user_id = %[3]s
		            AND e.status IN ('active', 'completed')
		            AND (e.expires_at IS NULL OR e.expires_at > now())
		      )
		  )
	)`

// askQuestionSQL inserts only when the author may read the lesson: the SELECT
// yields no row otherwise, and the INSERT writes nothing.
var askQuestionSQL = fmt.Sprintf(`
	INSERT INTO lesson_questions (tenant_id, lesson_id, author_id, body)
	SELECT $1, $2, $3, $5
	WHERE `+lessonReadableGuard+`
	RETURNING id, lesson_id, author_id, body, created_at`,
	"$1", "$2", "$3", "$4")

// AskQuestion posts a question, or returns ErrLessonNotFound when the lesson is
// absent or unreadable — one answer, so neither case reveals the other.
func (r *PostgresRepository) AskQuestion(ctx context.Context, tx pgx.Tx, tenantID, lessonID uuid.UUID, author Participant, body string) (Question, error) {
	var q Question
	var authorID *uuid.UUID
	err := tx.QueryRow(ctx, askQuestionSQL, tenantID, lessonID, author.UserID, author.CanModerate, body).
		Scan(&q.ID, &q.LessonID, &authorID, &q.Body, &q.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Question{}, ErrLessonNotFound
		}
		return Question{}, fmt.Errorf("learn: ask question: %w", err)
	}
	if authorID != nil {
		q.AuthorID = *authorID
	}
	return q, nil
}

// answerSQL inserts only when the author may read the question's lesson, resolved
// from the question row.
var answerSQL = fmt.Sprintf(`
	INSERT INTO lesson_answers (tenant_id, question_id, author_id, body, by_instructor)
	SELECT $1, q.id, $3, $5, $4
	FROM lesson_questions q
	WHERE q.tenant_id = $1 AND q.id = $2
	  AND `+lessonReadableGuard+`
	RETURNING id, question_id, author_id, body, by_instructor, created_at`,
	"$1", "q.lesson_id", "$3", "$4")

// Answer posts a reply, or returns ErrQuestionNotFound when the question is
// absent or its lesson unreadable.
func (r *PostgresRepository) Answer(ctx context.Context, tx pgx.Tx, tenantID, questionID uuid.UUID, author Participant, body string) (Answer, error) {
	var a Answer
	var authorID *uuid.UUID
	err := tx.QueryRow(ctx, answerSQL, tenantID, questionID, author.UserID, author.CanModerate, body).
		Scan(&a.ID, &a.QuestionID, &authorID, &a.Body, &a.ByInstructor, &a.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Answer{}, ErrQuestionNotFound
		}
		return Answer{}, fmt.Errorf("learn: answer: %w", err)
	}
	if authorID != nil {
		a.AuthorID = *authorID
	}
	return a, nil
}

// questionsSQL lists a lesson's questions, newest first, but only when the reader
// may read the lesson. An unreadable lesson yields no rows — an empty thread, not
// a leak.
var questionsSQL = fmt.Sprintf(`
	SELECT q.id, q.lesson_id, COALESCE(q.author_id, '00000000-0000-0000-0000-000000000000'::uuid),
	       COALESCE(u.name, ''), q.body, q.created_at
	FROM lesson_questions q
	LEFT JOIN users u ON u.id = q.author_id
	WHERE q.tenant_id = $1 AND q.lesson_id = $2
	  AND `+lessonReadableGuard+`
	ORDER BY q.created_at DESC, q.id
	LIMIT $5`,
	"$1", "$2", "$3", "$4")

// answersForSQL fetches every answer for a set of questions at once — the batch
// that keeps a thread list from being one query per question.
const answersForSQL = `
	SELECT a.id, a.question_id, COALESCE(a.author_id, '00000000-0000-0000-0000-000000000000'::uuid),
	       COALESCE(u.name, ''), a.body, a.by_instructor, a.created_at
	FROM lesson_answers a
	LEFT JOIN users u ON u.id = a.author_id
	WHERE a.tenant_id = $1 AND a.question_id = ANY($2)
	ORDER BY a.created_at, a.id`

// Questions lists a lesson's threads: the questions in one query, then all their
// answers in a second, stitched by a map. Two queries whatever the thread count.
func (r *PostgresRepository) Questions(ctx context.Context, tx pgx.Tx, tenantID, lessonID uuid.UUID, reader Participant, limit int) ([]Question, error) {
	rows, err := tx.Query(ctx, questionsSQL, tenantID, lessonID, reader.UserID, reader.CanModerate, limit)
	if err != nil {
		return nil, fmt.Errorf("learn: list questions: %w", err)
	}
	questions, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Question, error) {
		var q Question
		err := row.Scan(&q.ID, &q.LessonID, &q.AuthorID, &q.AuthorName, &q.Body, &q.CreatedAt)
		return q, err
	})
	if err != nil {
		return nil, fmt.Errorf("learn: scan questions: %w", err)
	}
	if len(questions) == 0 {
		return questions, nil
	}

	ids := make([]uuid.UUID, len(questions))
	for i, q := range questions {
		ids[i] = q.ID
	}

	answerRows, err := tx.Query(ctx, answersForSQL, tenantID, ids)
	if err != nil {
		return nil, fmt.Errorf("learn: list answers: %w", err)
	}
	answers, err := pgx.CollectRows(answerRows, func(row pgx.CollectableRow) (Answer, error) {
		var a Answer
		err := row.Scan(&a.ID, &a.QuestionID, &a.AuthorID, &a.AuthorName, &a.Body, &a.ByInstructor, &a.CreatedAt)
		return a, err
	})
	if err != nil {
		return nil, fmt.Errorf("learn: scan answers: %w", err)
	}

	byQuestion := make(map[uuid.UUID][]Answer, len(questions))
	for _, a := range answers {
		byQuestion[a.QuestionID] = append(byQuestion[a.QuestionID], a)
	}
	for i := range questions {
		questions[i].Answers = byQuestion[questions[i].ID]
	}
	return questions, nil
}

const deleteQuestionSQL = `
	DELETE FROM lesson_questions
	WHERE tenant_id = $1 AND id = $2 AND ($3 OR author_id = $4)`

// DeleteQuestion removes a question the caller authored, or any when they
// moderate. The predicate makes "not there" and "not yours" one row count: zero.
func (r *PostgresRepository) DeleteQuestion(ctx context.Context, tx pgx.Tx, tenantID, questionID uuid.UUID, actor Participant) (bool, error) {
	tag, err := tx.Exec(ctx, deleteQuestionSQL, tenantID, questionID, actor.CanModerate, actor.UserID)
	if err != nil {
		return false, fmt.Errorf("learn: delete question: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

const deleteAnswerSQL = `
	DELETE FROM lesson_answers
	WHERE tenant_id = $1 AND id = $2 AND ($3 OR author_id = $4)`

// DeleteAnswer removes an answer under the same rule as a question.
func (r *PostgresRepository) DeleteAnswer(ctx context.Context, tx pgx.Tx, tenantID, answerID uuid.UUID, actor Participant) (bool, error) {
	tag, err := tx.Exec(ctx, deleteAnswerSQL, tenantID, answerID, actor.CanModerate, actor.UserID)
	if err != nil {
		return false, fmt.Errorf("learn: delete answer: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

const answerTargetSQL = `
	SELECT COALESCE(q.author_id, '00000000-0000-0000-0000-000000000000'::uuid),
	       l.id, c.slug
	FROM lesson_questions q
	JOIN lessons l ON l.id = q.lesson_id
	JOIN topics t  ON t.id = l.topic_id
	JOIN courses c ON c.id = t.course_id
	WHERE q.tenant_id = $1 AND q.id = $2`

// AnswerTarget returns who to notify about an answer and where it links. It runs
// after the answer is written, in the same transaction, so the question is known
// to exist. A question with no author (the account was erased) reports the nil
// uuid, and the service sends nobody a notification.
func (r *PostgresRepository) AnswerTarget(ctx context.Context, tx pgx.Tx, tenantID, questionID uuid.UUID) (AnswerTarget, error) {
	var t AnswerTarget
	err := tx.QueryRow(ctx, answerTargetSQL, tenantID, questionID).
		Scan(&t.QuestionAuthorID, &t.LessonID, &t.CourseSlug)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return AnswerTarget{}, ErrQuestionNotFound
		}
		return AnswerTarget{}, fmt.Errorf("learn: answer target: %w", err)
	}
	return t, nil
}
