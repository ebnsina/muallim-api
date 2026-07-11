package learn

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/lms-api/internal/platform/database"
)

// Repository is where notes live. Every method takes a transaction, never the
// pool: `app.tenant_id` is bound transaction-locally.
type Repository interface {
	// Note reads the caller's note on a lesson. The bool is false when there is
	// none — which the service turns into an empty note, not an error.
	Note(ctx context.Context, tx pgx.Tx, tenantID, userID, lessonID uuid.UUID) (Note, bool, error)

	// Upsert writes the note, creating it or replacing the body. It returns the
	// stored note, whose UpdatedAt is the database's clock, not this process's.
	Upsert(ctx context.Context, tx pgx.Tx, tenantID, userID, lessonID uuid.UUID, body string) (Note, error)

	// Delete removes the caller's note on a lesson. Deleting one that is not there
	// is not an error: the end state — no note — is what was asked for.
	Delete(ctx context.Context, tx pgx.Tx, tenantID, userID, lessonID uuid.UUID) error
}

// Service holds the rules and owns the transaction boundaries.
type Service struct {
	db   *database.DB
	repo Repository
}

// NewService returns a Service.
func NewService(db *database.DB, repo Repository) *Service {
	return &Service{db: db, repo: repo}
}

// Note reads a learner's own note on a lesson. A lesson they have never noted
// reads as an empty note, so a caller has one shape to render rather than two.
func (s *Service) Note(ctx context.Context, tenantID, userID, lessonID uuid.UUID) (Note, error) {
	var note Note

	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		stored, found, err := s.repo.Note(ctx, tx, tenantID, userID, lessonID)
		if err != nil {
			return err
		}
		if found {
			note = stored
		} else {
			note = Note{LessonID: lessonID}
		}
		return nil
	})

	return note, err
}

// Save writes a learner's note. An empty body — nothing typed, or a note cleared
// — is a delete, so the two states a learner cannot tell apart are one row state
// too. A body past the limit is refused rather than truncated: silently dropping
// the end of what someone wrote is worse than telling them.
func (s *Service) Save(ctx context.Context, tenantID, userID, lessonID uuid.UUID, body string) (Note, error) {
	trimmed := strings.TrimSpace(body)
	if len(trimmed) > MaxNoteLength {
		return Note{}, ErrNoteTooLong
	}

	var note Note
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if trimmed == "" {
			if err := s.repo.Delete(ctx, tx, tenantID, userID, lessonID); err != nil {
				return err
			}
			note = Note{LessonID: lessonID}
			return nil
		}

		var err error
		note, err = s.repo.Upsert(ctx, tx, tenantID, userID, lessonID, trimmed)
		return err
	})

	return note, err
}
