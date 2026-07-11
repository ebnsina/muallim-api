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
