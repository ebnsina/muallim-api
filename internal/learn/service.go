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

	// Highlights lists the caller's marks on a lesson, in the order they sit in the
	// text.
	Highlights(ctx context.Context, tx pgx.Tx, tenantID, userID, lessonID uuid.UUID) ([]Highlight, error)

	// AddHighlight writes a mark and returns it with its id and timestamps. A lesson
	// id with no lesson behind it is ErrLessonNotFound.
	AddHighlight(ctx context.Context, tx pgx.Tx, tenantID, userID, lessonID uuid.UUID, h Highlight) (Highlight, error)

	// UpdateHighlightNote changes the note on the caller's own mark, returning
	// whether such a mark existed for them.
	UpdateHighlightNote(ctx context.Context, tx pgx.Tx, tenantID, userID, highlightID uuid.UUID, note string) (bool, error)

	// DeleteHighlight removes the caller's own mark, returning whether one was there.
	DeleteHighlight(ctx context.Context, tx pgx.Tx, tenantID, userID, highlightID uuid.UUID) (bool, error)
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

// Highlights lists a learner's own marks on a lesson, in reading order.
func (s *Service) Highlights(ctx context.Context, tenantID, userID, lessonID uuid.UUID) ([]Highlight, error) {
	var highlights []Highlight

	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		highlights, err = s.repo.Highlights(ctx, tx, tenantID, userID, lessonID)
		return err
	})

	return highlights, err
}

// AddHighlight marks a passage. The note is trimmed — surrounding whitespace on a
// remark is noise — but the quote is not: it is the text that was selected, and a
// client compares it against the lesson byte for byte to know if it still fits.
func (s *Service) AddHighlight(ctx context.Context, tenantID, userID, lessonID uuid.UUID, h Highlight) (Highlight, error) {
	h.Note = strings.TrimSpace(h.Note)
	if err := h.validate(); err != nil {
		return Highlight{}, err
	}

	var created Highlight
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		created, err = s.repo.AddHighlight(ctx, tx, tenantID, userID, lessonID, h)
		return err
	})

	return created, err
}

// EditHighlightNote changes the remark on a learner's own mark. A mark that is not
// theirs — or is not there — is ErrHighlightNotFound, the same answer either way,
// so one learner cannot probe for another's marks by id.
func (s *Service) EditHighlightNote(ctx context.Context, tenantID, userID, highlightID uuid.UUID, note string) error {
	trimmed := strings.TrimSpace(note)
	if len(trimmed) > MaxNoteLength {
		return ErrNoteTooLong
	}

	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		found, err := s.repo.UpdateHighlightNote(ctx, tx, tenantID, userID, highlightID, trimmed)
		if err != nil {
			return err
		}
		if !found {
			return ErrHighlightNotFound
		}
		return nil
	})
}

// RemoveHighlight deletes a learner's own mark. Not theirs, or not there, is
// ErrHighlightNotFound.
func (s *Service) RemoveHighlight(ctx context.Context, tenantID, userID, highlightID uuid.UUID) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		found, err := s.repo.DeleteHighlight(ctx, tx, tenantID, userID, highlightID)
		if err != nil {
			return err
		}
		if !found {
			return ErrHighlightNotFound
		}
		return nil
	})
}
