package assess

import (
	"context"
	"errors"
	"net/netip"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/lms-api/internal/platform/database"
)

// Repository is the persistence contract, declared here by its consumer.
type Repository interface {
	CreateQuiz(ctx context.Context, tx pgx.Tx, tenantID, lessonID uuid.UUID, n NewQuiz) (Quiz, error)
	QuizByLesson(ctx context.Context, tx pgx.Tx, tenantID, lessonID uuid.UUID) (Quiz, error)
	QuizByID(ctx context.Context, tx pgx.Tx, tenantID, quizID uuid.UUID) (Quiz, error)
	UpdateQuiz(ctx context.Context, tx pgx.Tx, tenantID, quizID uuid.UUID, p QuizPatch) (Quiz, error)
	DeleteQuiz(ctx context.Context, tx pgx.Tx, tenantID, lessonID uuid.UUID) (uuid.UUID, error)

	Questions(ctx context.Context, tx pgx.Tx, tenantID, quizID uuid.UUID) ([]Question, error)
	CreateQuestion(ctx context.Context, tx pgx.Tx, tenantID, quizID uuid.UUID, n NewQuestion) (Question, error)
	DeleteQuestion(ctx context.Context, tx pgx.Tx, tenantID, questionID uuid.UUID) (uuid.UUID, error)
	ReorderQuestions(ctx context.Context, tx pgx.Tx, tenantID, quizID uuid.UUID, order []uuid.UUID) error

	StartAttempt(ctx context.Context, tx pgx.Tx, tenantID, quizID, userID uuid.UUID, expiresAt *time.Time) (Attempt, error)
	LiveAttempt(ctx context.Context, tx pgx.Tx, tenantID, quizID, userID uuid.UUID) (Attempt, error)
	AttemptByNumber(ctx context.Context, tx pgx.Tx, tenantID, quizID, userID uuid.UUID, number int) (Attempt, error)
	AttemptByID(ctx context.Context, tx pgx.Tx, tenantID, attemptID uuid.UUID) (Attempt, error)
	ListAttempts(ctx context.Context, tx pgx.Tx, tenantID, quizID, userID uuid.UUID, limit int) ([]Attempt, error)

	SaveAnswer(ctx context.Context, tx pgx.Tx, tenantID, attemptID, questionID uuid.UUID, response Response) error
	CloseAttempt(ctx context.Context, tx pgx.Tx, tenantID, attemptID uuid.UUID) (Attempt, error)
	Responses(ctx context.Context, tx pgx.Tx, tenantID, attemptID uuid.UUID) (map[uuid.UUID]Response, error)
	WriteGrades(ctx context.Context, tx pgx.Tx, tenantID, attemptID uuid.UUID, grades []AnswerGrade) error
	FinishGrading(ctx context.Context, tx pgx.Tx, tenantID, attemptID uuid.UUID, status string, points, maxPoints int, passed *bool) (Attempt, error)
	ReviewItems(ctx context.Context, tx pgx.Tx, tenantID, attemptID uuid.UUID) ([]ReviewItem, error)
}

// AuditEntry is one line of the audit trail.
type AuditEntry struct {
	ActorID    *uuid.UUID
	Action     string
	TargetType string
	TargetID   string
	IP         netip.Addr
	UserAgent  string
	Metadata   map[string]any
}

// AuditRecorder appends to the audit trail inside the caller's transaction.
type AuditRecorder interface {
	Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e AuditEntry) error
}

// Enqueuer queues the grading of a submitted attempt.
//
// The method takes the caller's transaction, so the job and the submission commit
// together. An attempt closed without its job would sit in `grading` for ever;
// a job without the closed attempt would grade something a learner is still
// editing.
type Enqueuer interface {
	GradeAttempt(ctx context.Context, tx pgx.Tx, tenantID, attemptID uuid.UUID) error
}

// MaxAttemptsListed bounds the attempt history a learner is shown.
const MaxAttemptsListed = 50

// Service holds the assessment rules and owns transaction boundaries.
type Service struct {
	db    *database.DB
	repo  Repository
	audit AuditRecorder
	jobs  Enqueuer

	now func() time.Time
}

// NewService returns a Service.
func NewService(db *database.DB, repo Repository, recorder AuditRecorder, jobs Enqueuer) *Service {
	return &Service{db: db, repo: repo, audit: recorder, jobs: jobs, now: time.Now}
}

// CreateQuiz attaches a quiz to a lesson.
func (s *Service) CreateQuiz(ctx context.Context, tenantID, lessonID uuid.UUID, n NewQuiz, author Author) (Quiz, error) {
	if err := n.validate(); err != nil {
		return Quiz{}, err
	}

	var created Quiz
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		created, err = s.repo.CreateQuiz(ctx, tx, tenantID, lessonID, n)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionQuizCreated,
			TargetType: "quiz", TargetID: created.ID.String(),
			IP: author.IP, UserAgent: author.UserAgent,
			Metadata: map[string]any{"lesson_id": lessonID.String()},
		})
	})
	if err != nil {
		return Quiz{}, err
	}
	return created, nil
}

// EditQuiz applies a patch to a lesson's quiz.
func (s *Service) EditQuiz(ctx context.Context, tenantID, lessonID uuid.UUID, p QuizPatch, author Author) (Quiz, error) {
	if err := p.validate(); err != nil {
		return Quiz{}, err
	}

	var updated Quiz
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		quiz, err := s.repo.QuizByLesson(ctx, tx, tenantID, lessonID)
		if err != nil {
			return err
		}

		updated, err = s.repo.UpdateQuiz(ctx, tx, tenantID, quiz.ID, p)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionQuizUpdated,
			TargetType: "quiz", TargetID: quiz.ID.String(),
			IP: author.IP, UserAgent: author.UserAgent,
		})
	})
	if err != nil {
		return Quiz{}, err
	}
	return updated, nil
}

// RemoveQuiz deletes a lesson's quiz, and every attempt at it.
func (s *Service) RemoveQuiz(ctx context.Context, tenantID, lessonID uuid.UUID, author Author) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		quizID, err := s.repo.DeleteQuiz(ctx, tx, tenantID, lessonID)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionQuizDeleted,
			TargetType: "quiz", TargetID: quizID.String(),
			IP: author.IP, UserAgent: author.UserAgent,
			Metadata: map[string]any{"lesson_id": lessonID.String()},
		})
	})
}

// AuthoredQuiz returns the quiz with its questions, answers and all.
//
// Only an author may see this. The caller establishes that; this package would
// hand it to anybody who asked, which is exactly why LearnerQuiz exists and why
// the two are different types.
func (s *Service) AuthoredQuiz(ctx context.Context, tenantID, lessonID uuid.UUID) (Quiz, []Question, error) {
	var quiz Quiz
	var questions []Question

	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if quiz, err = s.repo.QuizByLesson(ctx, tx, tenantID, lessonID); err != nil {
			return err
		}
		questions, err = s.repo.Questions(ctx, tx, tenantID, quiz.ID)
		return err
	})
	if err != nil {
		return Quiz{}, nil, err
	}
	return quiz, questions, nil
}

// AddQuestion appends a question to a lesson's quiz.
func (s *Service) AddQuestion(ctx context.Context, tenantID, lessonID uuid.UUID, n NewQuestion, author Author) (Question, error) {
	if err := n.validate(); err != nil {
		return Question{}, err
	}

	var created Question
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		quiz, err := s.repo.QuizByLesson(ctx, tx, tenantID, lessonID)
		if err != nil {
			return err
		}

		created, err = s.repo.CreateQuestion(ctx, tx, tenantID, quiz.ID, n)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionQuestionCreated,
			TargetType: "question", TargetID: created.ID.String(),
			IP: author.IP, UserAgent: author.UserAgent,
			Metadata: map[string]any{"quiz_id": quiz.ID.String(), "type": created.Type},
		})
	})
	if err != nil {
		return Question{}, err
	}
	return created, nil
}

// RemoveQuestion deletes a question and closes the gap it leaves.
func (s *Service) RemoveQuestion(ctx context.Context, tenantID, questionID uuid.UUID, author Author) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		quizID, err := s.repo.DeleteQuestion(ctx, tx, tenantID, questionID)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionQuestionDeleted,
			TargetType: "question", TargetID: questionID.String(),
			IP: author.IP, UserAgent: author.UserAgent,
			Metadata: map[string]any{"quiz_id": quizID.String()},
		})
	})
}

// ReorderQuestions sets the order of a quiz's questions.
func (s *Service) ReorderQuestions(ctx context.Context, tenantID, lessonID uuid.UUID, order []uuid.UUID, author Author) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		quiz, err := s.repo.QuizByLesson(ctx, tx, tenantID, lessonID)
		if err != nil {
			return err
		}
		if err := s.repo.ReorderQuestions(ctx, tx, tenantID, quiz.ID, order); err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionQuestionUpdated,
			TargetType: "quiz", TargetID: quiz.ID.String(),
			IP: author.IP, UserAgent: author.UserAgent,
			Metadata: map[string]any{"questions": len(order)},
		})
	})
}

// LearnerView returns the quiz as the person taking it sees it, together with
// their own attempts.
//
// Whether they may read the lesson at all is decided before this is called. This
// package does not know what a course is.
func (s *Service) LearnerView(ctx context.Context, tenantID, lessonID, userID uuid.UUID) (LearnerQuiz, []Attempt, error) {
	var view LearnerQuiz
	var attempts []Attempt

	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		quiz, err := s.repo.QuizByLesson(ctx, tx, tenantID, lessonID)
		if err != nil {
			return err
		}

		questions, err := s.repo.Questions(ctx, tx, tenantID, quiz.ID)
		if err != nil {
			return err
		}
		view = forLearner(quiz, questions)

		// An anonymous reader of a preview lesson has no attempts and cannot have.
		if userID == uuid.Nil {
			return nil
		}
		attempts, err = s.repo.ListAttempts(ctx, tx, tenantID, quiz.ID, userID, MaxAttemptsListed)
		return err
	})
	if err != nil {
		return LearnerQuiz{}, nil, err
	}
	return view, attempts, nil
}

// StartAttempt opens an attempt, or hands back the one already open.
//
// Idempotent on purpose. A learner who reloads the page, or whose phone retried
// the request, has not started a second attempt and must not burn one of a
// bounded number.
func (s *Service) StartAttempt(ctx context.Context, tenantID, lessonID, userID uuid.UUID, author Author) (Attempt, error) {
	var attempt Attempt

	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		quiz, err := s.repo.QuizByLesson(ctx, tx, tenantID, lessonID)
		if err != nil {
			return err
		}

		questions, err := s.repo.Questions(ctx, tx, tenantID, quiz.ID)
		if err != nil {
			return err
		}
		if len(questions) == 0 {
			return ErrEmptyQuiz
		}

		// A live attempt is the answer to "start", not a conflict.
		live, err := s.repo.LiveAttempt(ctx, tx, tenantID, quiz.ID, userID)
		if err == nil {
			attempt = live
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}

		past, err := s.repo.ListAttempts(ctx, tx, tenantID, quiz.ID, userID, MaxAttemptsListed)
		if err != nil {
			return err
		}
		if quiz.MaxAttempts > 0 && len(past) >= quiz.MaxAttempts {
			return ErrAttemptsExhausted
		}

		// The deadline is frozen now, from the limit as it stands. Recomputing it at
		// read time would let an author shorten an attempt already under way.
		var expiresAt *time.Time
		if quiz.TimeLimitSeconds > 0 {
			deadline := s.now().Add(time.Duration(quiz.TimeLimitSeconds) * time.Second)
			expiresAt = &deadline
		}

		attempt, err = s.repo.StartAttempt(ctx, tx, tenantID, quiz.ID, userID, expiresAt)
		if err != nil {
			// Another request of this learner's won the partial unique index. Theirs
			// is the attempt; hand it back rather than reporting a conflict nobody
			// caused.
			if errors.Is(err, ErrAttemptInProgress) {
				attempt, err = s.repo.LiveAttempt(ctx, tx, tenantID, quiz.ID, userID)
			}
			if err != nil {
				return err
			}
			return nil
		}

		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionAttemptStarted,
			TargetType: "attempt", TargetID: attempt.ID.String(),
			IP: author.IP, UserAgent: author.UserAgent,
			Metadata: map[string]any{"quiz_id": quiz.ID.String(), "number": attempt.Number},
		})
	})
	if err != nil {
		return Attempt{}, err
	}
	return attempt, nil
}

// SaveAnswer records the learner's response to one question of their live
// attempt.
//
// Two things are checked here that the statement cannot: that the attempt belongs
// to this learner, and that its deadline has not passed. Whether it is still open
// is checked *in* the statement, because a read beforehand would race the submit.
func (s *Service) SaveAnswer(ctx context.Context, tenantID, lessonID, userID, questionID uuid.UUID, response Response) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		quiz, err := s.repo.QuizByLesson(ctx, tx, tenantID, lessonID)
		if err != nil {
			return err
		}

		attempt, err := s.repo.LiveAttempt(ctx, tx, tenantID, quiz.ID, userID)
		if err != nil {
			return err
		}
		if expired(attempt, s.now()) {
			return ErrAttemptExpired
		}

		return s.repo.SaveAnswer(ctx, tx, tenantID, attempt.ID, questionID, response)
	})
}

// SubmitAttempt closes the learner's live attempt and queues its grading.
//
// The job is inserted on this transaction. It becomes visible to the worker at
// the instant the attempt becomes `grading`, and never before or without it —
// which is the entire reason the queue lives in Postgres.
//
// Nothing is graded here. An essay quiz that graded in the handler is a request
// held open for as long as the grading takes, which is how LearnDash comes to
// spend thirty-five seconds saving one.
func (s *Service) SubmitAttempt(ctx context.Context, tenantID, lessonID, userID uuid.UUID, author Author) (Attempt, error) {
	var submitted Attempt

	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		quiz, err := s.repo.QuizByLesson(ctx, tx, tenantID, lessonID)
		if err != nil {
			return err
		}

		// Ownership: LiveAttempt is keyed by this learner, so an attempt of somebody
		// else's is simply not found.
		live, err := s.repo.LiveAttempt(ctx, tx, tenantID, quiz.ID, userID)
		if err != nil {
			return err
		}

		// An expired attempt is submitted, not refused. The learner ran out of time;
		// what they had written stands, and grading it is the honest outcome.
		submitted, err = s.repo.CloseAttempt(ctx, tx, tenantID, live.ID)
		if err != nil {
			return err
		}

		if err := s.jobs.GradeAttempt(ctx, tx, tenantID, submitted.ID); err != nil {
			return err
		}

		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionAttemptSubmitted,
			TargetType: "attempt", TargetID: submitted.ID.String(),
			IP: author.IP, UserAgent: author.UserAgent,
			Metadata: map[string]any{"quiz_id": quiz.ID.String(), "number": submitted.Number},
		})
	})
	if err != nil {
		return Attempt{}, err
	}
	return submitted, nil
}

// GradeAttempt grades a submitted attempt. Called by the worker, never by a
// handler.
//
// Idempotent, because jobs are retried. It recomputes every verdict from the
// questions and the stored responses and upserts them, so a second run writes the
// same rows. An attempt that is already graded is left alone: an instructor may
// have marked the essay by hand in between, and a retry must not undo that.
func (s *Service) GradeAttempt(ctx context.Context, tenantID, attemptID uuid.UUID) (Attempt, error) {
	var graded Attempt

	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		attempt, err := s.repo.AttemptByID(ctx, tx, tenantID, attemptID)
		if err != nil {
			return err
		}
		if attempt.Status != StatusGrading {
			graded = attempt
			return nil
		}

		quiz, err := s.repo.QuizByID(ctx, tx, tenantID, attempt.QuizID)
		if err != nil {
			return err
		}
		questions, err := s.repo.Questions(ctx, tx, tenantID, attempt.QuizID)
		if err != nil {
			return err
		}
		responses, err := s.repo.Responses(ctx, tx, tenantID, attempt.ID)
		if err != nil {
			return err
		}

		result := gradeAttempt(questions, responses)

		if err := s.repo.WriteGrades(ctx, tx, tenantID, attempt.ID, result.Answers); err != nil {
			return err
		}

		// A pass is not a thing to guess at while an essay is unmarked.
		status := StatusGraded
		var verdict *bool
		if result.AwaitingReview {
			status = StatusAwaitingReview
		} else {
			cleared := passed(result.Points, result.MaxPoints, quiz.PassingPercent)
			verdict = &cleared
		}

		graded, err = s.repo.FinishGrading(ctx, tx, tenantID, attempt.ID, status,
			result.Points, result.MaxPoints, verdict)
		if err != nil {
			return err
		}

		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			Action: ActionAttemptGraded, TargetType: "attempt", TargetID: attempt.ID.String(),
			Metadata: map[string]any{
				"points": result.Points, "max_points": result.MaxPoints, "status": status,
			},
		})
	})
	if err != nil {
		return Attempt{}, err
	}
	return graded, nil
}

// Review returns one of the learner's own attempts, with its verdicts.
//
// Addressed by attempt number, within a quiz, for this learner — so there is no
// identifier to guess into somebody else's result.
//
// The explanations are released only once the attempt has been graded. Before
// then they would be a hint; and even after, the correct answer itself is never
// sent, because a quiz that allows a retry would otherwise hand out the key with
// the first result.
func (s *Service) Review(ctx context.Context, tenantID, lessonID, userID uuid.UUID, number int) (AttemptReview, error) {
	var review AttemptReview

	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		quiz, err := s.repo.QuizByLesson(ctx, tx, tenantID, lessonID)
		if err != nil {
			return err
		}

		attempt, err := s.repo.AttemptByNumber(ctx, tx, tenantID, quiz.ID, userID, number)
		if err != nil {
			return err
		}

		items, err := s.repo.ReviewItems(ctx, tx, tenantID, attempt.ID)
		if err != nil {
			return err
		}

		if attempt.Status == StatusInProgress || attempt.Status == StatusGrading {
			for i := range items {
				items[i].Explanation = ""
			}
		}

		review = AttemptReview{Attempt: attempt, Items: items}
		return nil
	})
	if err != nil {
		return AttemptReview{}, err
	}
	return review, nil
}

// expired reports whether an attempt's deadline has passed.
func expired(a Attempt, now time.Time) bool {
	return a.ExpiresAt != nil && now.After(*a.ExpiresAt)
}
