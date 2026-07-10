package certify

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/lms-api/internal/platform/database"
)

// Repository is where certificates live. Every method takes a transaction, never
// the pool: `app.tenant_id` is bound transaction-locally.
type Repository interface {
	// Issue writes the certificate, unless the learner already has one for the
	// course. The bool says whether this call is the one that wrote it.
	Issue(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, c Certificate) (Certificate, bool, error)

	// Subject is the learner's name and the course's title, as they stand now.
	// Read inside the issuing transaction, and copied into the row.
	Subject(ctx context.Context, tx pgx.Tx, tenantID, courseID, userID uuid.UUID) (Subject, error)

	BySerial(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, serial string) (Certificate, error)
	ForLearner(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID) ([]Certificate, error)
	Revoke(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, serial, reason string) error

	TemplateByID(ctx context.Context, tx pgx.Tx, tenantID, templateID uuid.UUID) (Template, error)
	Templates(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) ([]Template, error)
	CreateTemplate(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, t Template) (Template, error)
	DeleteTemplate(ctx context.Context, tx pgx.Tx, tenantID, templateID uuid.UUID) error
	CourseTemplateID(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, slug string) (*uuid.UUID, error)
	SetCourseTemplate(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, slug string, templateID *uuid.UUID) error
}

// Subject is who a certificate is for, and what it is for.
type Subject struct {
	LearnerName string
	CourseTitle string

	// TemplateID is the course's own, or nil for the workspace's default.
	TemplateID *uuid.UUID
}

// AuditRecorder writes an audit line in the transaction of the thing it describes.
type AuditRecorder interface {
	Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e AuditEntry) error
}

// AuditEntry is one line of the audit log.
//
// No IP and no user agent. A certificate is issued by the transaction that
// finished a course, which may be a worker draining a queue; there is no request
// behind it whose address would mean anything. Revoking has one, and the handler
// records it there.
type AuditEntry struct {
	ActorID    *uuid.UUID
	Action     string
	TargetType string
	TargetID   string
	Metadata   map[string]any
}

// The actions this package records.
const (
	ActionIssued  = "certificate.issued"
	ActionRevoked = "certificate.revoked"
)

// Service issues and verifies certificates.
type Service struct {
	db    *database.DB
	repo  Repository
	audit AuditRecorder

	now func() time.Time
}

func NewService(db *database.DB, repo Repository, recorder AuditRecorder) *Service {
	return &Service{db: db, repo: repo, audit: recorder, now: time.Now}
}

/*
IssueIfEarned writes a certificate for a learner who has just finished a course.

Called by `enroll`, in the transaction that completed the enrolment, through an
interface it declares. A certificate written afterwards by a job is a learner who
finished a course and cannot prove it for as long as the queue is behind — and for
ever if the job dies.

Idempotent. Finishing a course twice is finishing it once, and re-completing the
last lesson issues nothing new. The second call returns the certificate the first
one wrote.

Reopening a lesson does not revoke it. A certificate records that the course was
completed on a day, and that day happened. Revoking is a deliberate act, by a
person, with a reason.
*/
func (s *Service) IssueIfEarned(ctx context.Context, tx pgx.Tx, tenantID, courseID, userID uuid.UUID) error {
	subject, err := s.repo.Subject(ctx, tx, tenantID, courseID, userID)
	if err != nil {
		return err
	}

	template, err := s.templateFor(ctx, tx, tenantID, subject.TemplateID)
	if err != nil {
		return err
	}

	issued := s.now().UTC()
	serial := NewSerial()

	certificate := Certificate{
		CourseID: courseID, UserID: userID,
		Serial:      serial,
		LearnerName: subject.LearnerName,
		CourseTitle: subject.CourseTitle,
		IssuedAt:    issued,
		TemplateID:  subject.TemplateID,
		Title:       template.Title,

		// Rendered once, now, and stored. A certificate re-rendered on every read
		// would change its words the day somebody edits the template — including for
		// the people who already have it printed and framed.
		Body: Render(template.Body, Fields{
			Learner: subject.LearnerName,
			Course:  subject.CourseTitle,
			Date:    issued.Format(DateFormat),
			Serial:  serial,
		}),
		Signatory: template.Signatory,
	}

	written, fresh, err := s.repo.Issue(ctx, tx, tenantID, certificate)
	if err != nil {
		return err
	}
	if !fresh {
		return nil
	}

	return s.audit.Record(ctx, tx, tenantID, AuditEntry{
		ActorID: &userID, Action: ActionIssued,
		TargetType: "certificate", TargetID: written.Serial,
		Metadata: map[string]any{"course_id": courseID.String()},
	})
}

// templateFor resolves the words a course prints: its own template, or the
// built-in default.
func (s *Service) templateFor(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, templateID *uuid.UUID) (Template, error) {
	if templateID == nil {
		return DefaultTemplate(), nil
	}

	template, err := s.repo.TemplateByID(ctx, tx, tenantID, *templateID)
	if errors.Is(err, ErrNotFound) {
		// The column is `ON DELETE SET NULL`, so this should not happen. If it does,
		// printing the default beats refusing somebody the certificate they earned.
		return DefaultTemplate(), nil
	}
	return template, err
}

/*
Verify reads a certificate by its serial.

No session. Whoever holds the code may read what it says: that is what a
certificate is for, and the code is the credential. A revoked one still resolves,
and says it is revoked — a code that stopped answering would be indistinguishable
from a code that was never real, which is the answer a forger wants.

The learner's email is not on it. The name is, because the name is the point.
*/
func (s *Service) Verify(ctx context.Context, tenantID uuid.UUID, serial string) (Certificate, error) {
	var certificate Certificate

	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		certificate, err = s.repo.BySerial(ctx, tx, tenantID, serial)
		return err
	})

	return certificate, err
}

// Mine is a learner's own certificates, newest first.
func (s *Service) Mine(ctx context.Context, tenantID, userID uuid.UUID) ([]Certificate, error) {
	var certificates []Certificate

	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		certificates, err = s.repo.ForLearner(ctx, tx, tenantID, userID)
		return err
	})

	return certificates, err
}

// Revoke marks a certificate as no longer standing, and says why. It is never
// deleted: somebody has the code.
func (s *Service) Revoke(ctx context.Context, tenantID uuid.UUID, serial, reason string, actor uuid.UUID) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := s.repo.Revoke(ctx, tx, tenantID, serial, reason); err != nil {
			return err
		}

		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &actor, Action: ActionRevoked,
			TargetType: "certificate", TargetID: serial,
			Metadata: map[string]any{"reason": reason},
		})
	})
}

// Templates lists the workspace's templates, the built-in default first. The
// default is not a row, so it is not in the list twice.
func (s *Service) Templates(ctx context.Context, tenantID uuid.UUID) ([]Template, error) {
	var templates []Template

	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		stored, err := s.repo.Templates(ctx, tx, tenantID)
		if err != nil {
			return err
		}
		templates = append([]Template{DefaultTemplate()}, stored...)
		return nil
	})

	return templates, err
}

// CreateTemplate adds one.
func (s *Service) CreateTemplate(ctx context.Context, tenantID uuid.UUID, t Template) (Template, error) {
	if err := t.validate(); err != nil {
		return Template{}, err
	}

	var created Template
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		created, err = s.repo.CreateTemplate(ctx, tx, tenantID, t)
		return err
	})

	return created, err
}

// DeleteTemplate removes one. Courses printing it fall back to the default, and
// the certificates already issued keep the words they were issued with.
func (s *Service) DeleteTemplate(ctx context.Context, tenantID, templateID uuid.UUID) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return s.repo.DeleteTemplate(ctx, tx, tenantID, templateID)
	})
}

// CourseTemplateID is which template a course prints — its own, or nil for the
// built-in default. For an editor that shows the current choice.
func (s *Service) CourseTemplateID(ctx context.Context, tenantID uuid.UUID, slug string) (*uuid.UUID, error) {
	var id *uuid.UUID
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		id, err = s.repo.CourseTemplateID(ctx, tx, tenantID, slug)
		return err
	})
	return id, err
}

// SetCourseTemplate points a course at a template, or back at the default.
//
// The template is read inside the transaction before it is assigned. Without that
// a course could be pointed at another workspace's template — the foreign key
// would allow it if the row existed, and RLS is the net rather than the check.
func (s *Service) SetCourseTemplate(ctx context.Context, tenantID uuid.UUID, slug string, templateID *uuid.UUID) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if templateID != nil {
			if _, err := s.repo.TemplateByID(ctx, tx, tenantID, *templateID); err != nil {
				return fmt.Errorf("assign template to %s: %w", slug, err)
			}
		}
		return s.repo.SetCourseTemplate(ctx, tx, tenantID, slug, templateID)
	})
}
