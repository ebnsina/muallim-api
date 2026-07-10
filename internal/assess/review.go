package assess

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Marking errors.
var (
	// ErrNotAwaitingReview means the attempt has nothing left for a person to do.
	// A graded attempt is not re-marked by this path: changing a settled grade is a
	// different act, with a different audit line, and it does not exist yet.
	ErrNotAwaitingReview = errors.New("assess: that attempt is not awaiting review")

	// ErrNotManual means the question graded itself. An instructor who could
	// overwrite a machine's verdict on a multiple-choice question could quietly
	// make a wrong answer right.
	ErrNotManual = errors.New("assess: that question is not marked by hand")

	// ErrInvalidGrade means the awarded points are not between zero and what the
	// question is worth.
	ErrInvalidGrade = errors.New("assess: the award is not within the question's points")
)

// ActionAnswerMarked records an instructor grading one answer.
const ActionAnswerMarked = "answer.marked"

// Submission is one learner's attempt, as the person marking it sees it.
//
// It carries the learner, because a marking queue that showed only attempt
// numbers would be a marking queue nobody could use. Name and email come from the
// users table, which this package reads and never writes — two packages, one
// table, no import between them.
type Submission struct {
	Attempt Attempt

	LearnerName  string
	LearnerEmail string

	// Unmarked counts the answers still waiting for a person.
	Unmarked int
}

// Mark is an instructor's verdict on one answer.
type Mark struct {
	Points   int
	Feedback string
}

// Submissions lists the attempts at a lesson's quiz.
//
// For the person marking them, so it names the learner. Whether the caller may
// mark is decided before this is called; this package does not know what a
// permission is.
func (s *Service) Submissions(ctx context.Context, tenantID, lessonID uuid.UUID, onlyAwaiting bool, limit int) ([]Submission, error) {
	if limit <= 0 || limit > MaxAttemptsListed {
		limit = MaxAttemptsListed
	}

	var submissions []Submission
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		quiz, err := s.repo.QuizByLesson(ctx, tx, tenantID, lessonID)
		if err != nil {
			return err
		}
		submissions, err = s.repo.ListSubmissions(ctx, tx, tenantID, quiz.ID, onlyAwaiting, limit)
		return err
	})
	if err != nil {
		return nil, err
	}
	return submissions, nil
}

// Submission returns one attempt for marking: every question, the learner's
// answer, and — because the marker needs it — the author's own answer key.
func (s *Service) Submission(ctx context.Context, tenantID, attemptID uuid.UUID) (Attempt, []Question, map[uuid.UUID]Answer, error) {
	var attempt Attempt
	var questions []Question
	answers := make(map[uuid.UUID]Answer)

	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if attempt, err = s.repo.AttemptByID(ctx, tx, tenantID, attemptID); err != nil {
			return err
		}
		if questions, err = s.repo.Questions(ctx, tx, tenantID, attempt.QuizID); err != nil {
			return err
		}

		items, err := s.repo.ReviewItems(ctx, tx, tenantID, attemptID)
		if err != nil {
			return err
		}
		for _, item := range items {
			answers[item.QuestionID] = Answer{
				AttemptID: attemptID, QuestionID: item.QuestionID,
				Response: item.Response, Graded: item.Graded,
				Correct: item.Correct, Points: item.Points, Feedback: item.Feedback,
			}
		}
		return nil
	})
	if err != nil {
		return Attempt{}, nil, nil, err
	}
	return attempt, questions, answers, nil
}

// MarkAnswer records an instructor's verdict on one essay, and restates the
// attempt in the same transaction.
//
// The roll-up is recomputed here, from the rows that were just changed — not on
// read, where a page listing a hundred attempts would sum a hundred sets of
// answers, and not in a trigger, which is action at a distance. It is one
// statement, so the attempt's score can never disagree with the answers it
// summarises.
//
// Marking the last essay is what turns `awaiting_review` into `graded`, and only
// then does the attempt acquire a pass or a failure. Until every question has a
// grade there is nothing to compare against the bar.
func (s *Service) MarkAnswer(ctx context.Context, tenantID, attemptID, questionID uuid.UUID, m Mark, marker Author) (Attempt, error) {
	var attempt Attempt

	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		current, err := s.repo.AttemptByID(ctx, tx, tenantID, attemptID)
		if err != nil {
			return err
		}
		if current.Status != StatusAwaitingReview {
			return ErrNotAwaitingReview
		}

		questions, err := s.repo.Questions(ctx, tx, tenantID, current.QuizID)
		if err != nil {
			return err
		}

		question, ok := find(questions, questionID)
		if !ok {
			return ErrNotFound
		}
		if !IsManual(question.Type) {
			return fmt.Errorf("%w: %s", ErrNotManual, question.Type)
		}
		if m.Points < 0 || m.Points > question.Points {
			return fmt.Errorf("%w: %d is not between 0 and %d", ErrInvalidGrade, m.Points, question.Points)
		}

		// `correct` on a marked answer means full marks. An essay awarded three of
		// five is neither right nor wrong, and the flag exists so that a client can
		// draw a tick — so it draws one only when the answer earned everything.
		correct := m.Points == question.Points

		if err := s.repo.MarkAnswer(ctx, tx, tenantID, attemptID, questionID, m, correct); err != nil {
			return err
		}

		attempt, err = s.repo.RecomputeAttempt(ctx, tx, tenantID, attemptID)
		if err != nil {
			return err
		}

		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &marker.UserID, Action: ActionAnswerMarked,
			TargetType: "attempt", TargetID: attemptID.String(),
			IP: marker.IP, UserAgent: marker.UserAgent,
			Metadata: map[string]any{
				"question_id": questionID.String(),
				"points":      m.Points,
				"of":          question.Points,
				"status":      attempt.Status,
			},
		})
	})
	if err != nil {
		return Attempt{}, err
	}
	return attempt, nil
}

func find(questions []Question, id uuid.UUID) (Question, bool) {
	for _, q := range questions {
		if q.ID == id {
			return q, true
		}
	}
	return Question{}, false
}
