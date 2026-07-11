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

	// NotesForCourse lists the caller's notes across every lesson of one course,
	// resolved by the course's slug.
	NotesForCourse(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID, slug string) ([]Note, error)

	// HighlightsForCourse lists the caller's marks across every lesson of one
	// course, in reading order within each lesson.
	HighlightsForCourse(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID, slug string) ([]Highlight, error)

	// Questions lists a lesson's questions with their answers, but only if the
	// reader may read the lesson — an enrolment join decides. A reader who may not
	// gets an empty list, not another course's threads.
	Questions(ctx context.Context, tx pgx.Tx, tenantID, lessonID uuid.UUID, reader Participant, limit int) ([]Question, error)

	// AskQuestion posts a question, but only if the author may read the lesson.
	// A lesson that is absent or unreadable is ErrLessonNotFound — the same 404,
	// so neither reveals the other.
	AskQuestion(ctx context.Context, tx pgx.Tx, tenantID, lessonID uuid.UUID, author Participant, body string) (Question, error)

	// Answer posts a reply, but only if the author may read the question's lesson.
	// An absent or unreadable question is ErrQuestionNotFound.
	Answer(ctx context.Context, tx pgx.Tx, tenantID, questionID uuid.UUID, author Participant, body string) (Answer, error)

	// DeleteQuestion removes a question the caller authored, or any question when
	// they may moderate. Returns whether one was theirs to remove.
	DeleteQuestion(ctx context.Context, tx pgx.Tx, tenantID, questionID uuid.UUID, actor Participant) (bool, error)

	// DeleteAnswer removes an answer the caller authored, or any when they moderate.
	DeleteAnswer(ctx context.Context, tx pgx.Tx, tenantID, answerID uuid.UUID, actor Participant) (bool, error)
}

// CourseAnnotations gathers everything a learner has kept across a course — their
// notes and their marks — for a page they review before an exam. Both reads run
// in one read-only transaction; a slug that resolves to no course simply yields
// nothing, which is what an unenrolled or mistyped page should show.
func (s *Service) CourseAnnotations(ctx context.Context, tenantID, userID uuid.UUID, slug string) ([]Note, []Highlight, error) {
	var (
		notes      []Note
		highlights []Highlight
	)

	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if notes, err = s.repo.NotesForCourse(ctx, tx, tenantID, userID, slug); err != nil {
			return err
		}
		highlights, err = s.repo.HighlightsForCourse(ctx, tx, tenantID, userID, slug)
		return err
	})

	return notes, highlights, err
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

// Questions lists a lesson's discussion. A reader who may not read the lesson
// sees an empty thread rather than another course's, and the page they are on
// has already refused them the lesson body.
func (s *Service) Questions(ctx context.Context, tenantID, lessonID uuid.UUID, reader Participant, limit int) ([]Question, error) {
	if limit <= 0 || limit > MaxQuestionsPerPage {
		limit = MaxQuestionsPerPage
	}

	var questions []Question
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		questions, err = s.repo.Questions(ctx, tx, tenantID, lessonID, reader, limit)
		return err
	})
	return questions, err
}

// Ask posts a question on a lesson. An empty body is refused, not stored.
func (s *Service) Ask(ctx context.Context, tenantID, lessonID uuid.UUID, author Participant, body string) (Question, error) {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return Question{}, ErrEmptyPost
	}
	if len(trimmed) > MaxPostLength {
		return Question{}, ErrPostTooLong
	}

	var question Question
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		question, err = s.repo.AskQuestion(ctx, tx, tenantID, lessonID, author, trimmed)
		return err
	})
	return question, err
}

// Answer posts a reply to a question.
func (s *Service) Answer(ctx context.Context, tenantID, questionID uuid.UUID, author Participant, body string) (Answer, error) {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return Answer{}, ErrEmptyPost
	}
	if len(trimmed) > MaxPostLength {
		return Answer{}, ErrPostTooLong
	}

	var answer Answer
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		answer, err = s.repo.Answer(ctx, tx, tenantID, questionID, author, trimmed)
		return err
	})
	return answer, err
}

// DeleteQuestion removes a question. Not the caller's and not theirs to moderate
// reads as ErrQuestionNotFound, the same answer as one that never existed.
func (s *Service) DeleteQuestion(ctx context.Context, tenantID, questionID uuid.UUID, actor Participant) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		removed, err := s.repo.DeleteQuestion(ctx, tx, tenantID, questionID, actor)
		if err != nil {
			return err
		}
		if !removed {
			return ErrQuestionNotFound
		}
		return nil
	})
}

// DeleteAnswer removes an answer under the same rule as a question.
func (s *Service) DeleteAnswer(ctx context.Context, tenantID, answerID uuid.UUID, actor Participant) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		removed, err := s.repo.DeleteAnswer(ctx, tx, tenantID, answerID, actor)
		if err != nil {
			return err
		}
		if !removed {
			return ErrAnswerNotFound
		}
		return nil
	})
}
