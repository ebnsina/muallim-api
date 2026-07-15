package exams

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/platform/database"
)

// Repository is the persistence contract, declared by its consumer. Every method
// takes a transaction, never the pool: the tenant is bound transaction-locally.
type Repository interface {
	DefaultScale(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (GradingScale, error)
	CreateScale(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewScale) (GradingScale, error)
	Scales(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, limit int) ([]GradingScale, error)
	ScaleBands(ctx context.Context, tx pgx.Tx, tenantID, scaleID uuid.UUID) ([]Band, error)

	CreateExam(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewExam) (Exam, error)
	Exams(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, after *cursor, limit int) ([]Exam, error)
	ExamByID(ctx context.Context, tx pgx.Tx, tenantID, examID uuid.UUID) (Exam, error)
	PublishExam(ctx context.Context, tx pgx.Tx, tenantID, examID uuid.UUID) (Exam, error)
	DeleteExam(ctx context.Context, tx pgx.Tx, tenantID, examID uuid.UUID) error

	UpsertMarks(ctx context.Context, tx pgx.Tx, tenantID, examID, markedBy uuid.UUID, marks []Mark) (int, error)
	StudentMarks(ctx context.Context, tx pgx.Tx, tenantID, examID, studentID uuid.UUID) ([]markRow, error)
}

// AuditRecorder writes an audit line in the transaction of the thing it describes.
type AuditRecorder interface {
	Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e AuditEntry) error
}

// AuditEntry is one line of the audit log.
type AuditEntry struct {
	ActorID    *uuid.UUID
	Action     string
	TargetType string
	TargetID   string
	Metadata   map[string]any
}

// Author identifies who did something, for the audit trail.
type Author struct {
	UserID uuid.UUID
}

// Service holds the assessment rules and owns transaction boundaries.
type Service struct {
	db    *database.DB
	repo  Repository
	audit AuditRecorder
}

// NewService returns a Service.
func NewService(db *database.DB, repo Repository, recorder AuditRecorder) *Service {
	return &Service{db: db, repo: repo, audit: recorder}
}

// DefaultScale returns the workspace's default grading scale, seeding the Bangladesh
// GPA-5 scale the first time one is asked for. Idempotent: the partial unique index
// makes a racing second seed fail, and the retry reads the winner.
func (s *Service) DefaultScale(ctx context.Context, tenantID uuid.UUID, author Author) (GradingScale, error) {
	var scale GradingScale
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		got, err := s.repo.DefaultScale(ctx, tx, tenantID)
		if err == nil {
			scale = got
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		scale, err = s.repo.CreateScale(ctx, tx, tenantID, NewScale{
			Name: DefaultScaleName, IsDefault: true, Bands: bdGPA5,
		})
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionScaleCreated,
			TargetType: "grading_scale", TargetID: scale.ID.String(),
			Metadata: map[string]any{"name": scale.Name, "seeded": true},
		})
	})
	return scale, err
}

// CreateScale adds a custom grading scale.
func (s *Service) CreateScale(ctx context.Context, tenantID uuid.UUID, n NewScale, author Author) (GradingScale, error) {
	if err := n.validate(); err != nil {
		return GradingScale{}, err
	}
	var scale GradingScale
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		scale, err = s.repo.CreateScale(ctx, tx, tenantID, n)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionScaleCreated,
			TargetType: "grading_scale", TargetID: scale.ID.String(),
			Metadata: map[string]any{"name": scale.Name},
		})
	})
	return scale, err
}

// Scales lists the workspace's grading scales with their bands.
func (s *Service) Scales(ctx context.Context, tenantID uuid.UUID) ([]GradingScale, error) {
	var scales []GradingScale
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		scales, err = s.repo.Scales(ctx, tx, tenantID, MaxScales)
		return err
	})
	return scales, err
}

// CreateExam schedules an exam.
func (s *Service) CreateExam(ctx context.Context, tenantID uuid.UUID, n NewExam, author Author) (Exam, error) {
	if err := n.validate(); err != nil {
		return Exam{}, err
	}
	var exam Exam
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		exam, err = s.repo.CreateExam(ctx, tx, tenantID, n)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionExamCreated,
			TargetType: "exam", TargetID: exam.ID.String(),
			Metadata: map[string]any{"name": exam.Name},
		})
	})
	return exam, err
}

// Exams lists a workspace's exams, newest first, keyset-paginated.
func (s *Service) Exams(ctx context.Context, tenantID uuid.UUID, p PageParams) (ExamPage, error) {
	after, err := p.decode()
	if err != nil {
		return ExamPage{}, err
	}
	limit := p.clamp()

	var page ExamPage
	err = s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := s.repo.Exams(ctx, tx, tenantID, after, limit+1)
		if err != nil {
			return err
		}
		if len(rows) > limit {
			page.HasMore = true
			page.NextCursor = encodeCursor(rows[limit-1])
			rows = rows[:limit]
		}
		page.Exams = rows
		return nil
	})
	return page, err
}

// PublishExam marks an exam published, so its report cards may be shared.
func (s *Service) PublishExam(ctx context.Context, tenantID, examID uuid.UUID, author Author) (Exam, error) {
	var exam Exam
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		exam, err = s.repo.PublishExam(ctx, tx, tenantID, examID)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionExamPublished,
			TargetType: "exam", TargetID: exam.ID.String(),
		})
	})
	return exam, err
}

// DeleteExam removes an exam and, by cascade, its marks.
func (s *Service) DeleteExam(ctx context.Context, tenantID, examID uuid.UUID) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return s.repo.DeleteExam(ctx, tx, tenantID, examID)
	})
}

// EnterMarks records a batch of subject marks for an exam. Re-entering a
// (student, subject) updates it rather than stacking a second row. Marks may only
// be entered while an exam is a draft — once published, the results are fixed.
func (s *Service) EnterMarks(ctx context.Context, tenantID, examID uuid.UUID, marks []Mark, author Author) (int, error) {
	if len(marks) == 0 {
		return 0, ErrInvalidMark
	}
	if len(marks) > MaxMarks {
		return 0, ErrInvalidMark
	}
	for _, m := range marks {
		if err := m.validate(); err != nil {
			return 0, err
		}
	}

	var entered int
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		exam, err := s.repo.ExamByID(ctx, tx, tenantID, examID)
		if err != nil {
			return err
		}
		if exam.Status != StatusDraft {
			return ErrExamPublished
		}
		entered, err = s.repo.UpsertMarks(ctx, tx, tenantID, examID, author.UserID, marks)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionMarksEntered,
			TargetType: "exam", TargetID: examID.String(),
			Metadata: map[string]any{"count": entered},
		})
	})
	return entered, err
}

// ReportCard computes one student's result for an exam against the exam's scale.
// Three reads — the exam, its scale's bands, the student's marks — then a pure
// computation, so the card can never disagree with the marks it summarises.
func (s *Service) ReportCard(ctx context.Context, tenantID, examID, studentID uuid.UUID) (ReportCard, error) {
	var rc ReportCard
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		exam, err := s.repo.ExamByID(ctx, tx, tenantID, examID)
		if err != nil {
			return err
		}
		bands, err := s.repo.ScaleBands(ctx, tx, tenantID, exam.ScaleID)
		if err != nil {
			return err
		}
		if len(bands) == 0 {
			return ErrNoScale
		}
		marks, err := s.repo.StudentMarks(ctx, tx, tenantID, examID, studentID)
		if err != nil {
			return err
		}
		if len(marks) == 0 {
			return ErrNotFound
		}
		rc, err = reportCard(studentID, marks, bands)
		return err
	})
	return rc, err
}
