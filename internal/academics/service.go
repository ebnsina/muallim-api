package academics

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/platform/database"
)

// Repository is the persistence contract, declared here by its consumer. Every
// method takes a transaction, never the pool: `app.tenant_id` is bound
// transaction-locally, and a session-level SET on a pooled connection leaks.
type Repository interface {
	InstitutionType(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (string, error)
	SetInstitutionType(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, kind string) error

	CreateYear(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewAcademicYear) (AcademicYear, error)
	Years(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, limit int) ([]AcademicYear, error)
	SetCurrentYear(ctx context.Context, tx pgx.Tx, tenantID, yearID uuid.UUID) (AcademicYear, error)
	DeleteYear(ctx context.Context, tx pgx.Tx, tenantID, yearID uuid.UUID) error

	CreateTerm(ctx context.Context, tx pgx.Tx, tenantID, yearID uuid.UUID, n NewTerm) (Term, error)
	Terms(ctx context.Context, tx pgx.Tx, tenantID, yearID uuid.UUID) ([]Term, error)
	DeleteTerm(ctx context.Context, tx pgx.Tx, tenantID, termID uuid.UUID) error

	CreateClass(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewGradeLevel) (GradeLevel, error)
	Classes(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, limit int) ([]GradeLevel, error)
	DeleteClass(ctx context.Context, tx pgx.Tx, tenantID, classID uuid.UUID) error

	CreateSection(ctx context.Context, tx pgx.Tx, tenantID, classID uuid.UUID, n NewSection) (Section, error)
	Sections(ctx context.Context, tx pgx.Tx, tenantID, classID uuid.UUID) ([]Section, error)
	DeleteSection(ctx context.Context, tx pgx.Tx, tenantID, sectionID uuid.UUID) error

	CreateSubject(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewSubject) (Subject, error)
	Subjects(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, limit int) ([]Subject, error)
	DeleteSubject(ctx context.Context, tx pgx.Tx, tenantID, subjectID uuid.UUID) error

	CreateStudent(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewStudent) (Student, error)
	Roster(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, filter RosterFilter, after *cursor, limit int) ([]Student, error)
	StudentByID(ctx context.Context, tx pgx.Tx, tenantID, studentID uuid.UUID) (Student, error)
	UpdateStudent(ctx context.Context, tx pgx.Tx, tenantID, studentID uuid.UUID, p StudentPatch) (Student, error)
	DeleteStudent(ctx context.Context, tx pgx.Tx, tenantID, studentID uuid.UUID) error

	AddGuardian(ctx context.Context, tx pgx.Tx, tenantID, studentID uuid.UUID, n NewGuardian) (Guardian, error)
	GuardiansOf(ctx context.Context, tx pgx.Tx, tenantID, studentID uuid.UUID) ([]Guardian, error)
	RemoveGuardian(ctx context.Context, tx pgx.Tx, tenantID, studentID, guardianID uuid.UUID) error

	MarkAttendance(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, m AttendanceMark) (int, error)
	Register(ctx context.Context, tx pgx.Tx, tenantID, sectionID uuid.UUID, on time.Time) ([]RegisterEntry, error)
	StudentAttendance(ctx context.Context, tx pgx.Tx, tenantID, studentID uuid.UUID, from, to time.Time) ([]AttendanceDay, AttendanceSummary, error)

	CreatePeriod(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewPeriod) (Period, error)
	SectionTimetable(ctx context.Context, tx pgx.Tx, tenantID, sectionID uuid.UUID) ([]Period, error)
	DeletePeriod(ctx context.Context, tx pgx.Tx, tenantID, periodID uuid.UUID) error

	PromoteClass(ctx context.Context, tx pgx.Tx, tenantID, sourceClassID uuid.UUID, p Promotion) (int, error)
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

// Service holds the academic rules and owns transaction boundaries.
type Service struct {
	db    *database.DB
	repo  Repository
	audit AuditRecorder
}

// NewService returns a Service.
func NewService(db *database.DB, repo Repository, recorder AuditRecorder) *Service {
	return &Service{db: db, repo: repo, audit: recorder}
}

// InstitutionType reports how this workspace describes itself.
func (s *Service) InstitutionType(ctx context.Context, tenantID uuid.UUID) (string, error) {
	var kind string
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		kind, err = s.repo.InstitutionType(ctx, tx, tenantID)
		return err
	})
	return kind, err
}

// SetInstitutionType records what kind of institution this workspace is.
func (s *Service) SetInstitutionType(ctx context.Context, tenantID uuid.UUID, kind string) error {
	if !ValidInstitutionType(kind) {
		return ErrInvalidInstitutionType
	}
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return s.repo.SetInstitutionType(ctx, tx, tenantID, kind)
	})
}

// CreateYear opens an academic year.
func (s *Service) CreateYear(ctx context.Context, tenantID uuid.UUID, n NewAcademicYear, author Author) (AcademicYear, error) {
	if err := n.validate(); err != nil {
		return AcademicYear{}, err
	}

	var year AcademicYear
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		year, err = s.repo.CreateYear(ctx, tx, tenantID, n)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionYearCreated,
			TargetType: "academic_year", TargetID: year.ID.String(),
			Metadata: map[string]any{"name": year.Name},
		})
	})
	return year, err
}

// Years lists a workspace's academic years, current first, then newest.
func (s *Service) Years(ctx context.Context, tenantID uuid.UUID) ([]AcademicYear, error) {
	var years []AcademicYear
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		years, err = s.repo.Years(ctx, tx, tenantID, MaxYears)
		return err
	})
	return years, err
}

// SetCurrentYear makes one year the current one, and no other. The database's
// partial unique index refuses a second current year, so the swap is one statement
// that clears the old and sets the new.
func (s *Service) SetCurrentYear(ctx context.Context, tenantID, yearID uuid.UUID, author Author) (AcademicYear, error) {
	var year AcademicYear
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		year, err = s.repo.SetCurrentYear(ctx, tx, tenantID, yearID)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionYearSetCurrent,
			TargetType: "academic_year", TargetID: year.ID.String(),
		})
	})
	return year, err
}

// DeleteYear removes a year and, by cascade, its terms.
func (s *Service) DeleteYear(ctx context.Context, tenantID, yearID uuid.UUID) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return s.repo.DeleteYear(ctx, tx, tenantID, yearID)
	})
}

// CreateTerm appends a term to a year, positioned after the last.
func (s *Service) CreateTerm(ctx context.Context, tenantID, yearID uuid.UUID, n NewTerm) (Term, error) {
	if err := n.validate(); err != nil {
		return Term{}, err
	}
	var term Term
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		term, err = s.repo.CreateTerm(ctx, tx, tenantID, yearID, n)
		return err
	})
	return term, err
}

// Terms lists a year's terms in order.
func (s *Service) Terms(ctx context.Context, tenantID, yearID uuid.UUID) ([]Term, error) {
	var terms []Term
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		terms, err = s.repo.Terms(ctx, tx, tenantID, yearID)
		return err
	})
	return terms, err
}

// DeleteTerm removes one term.
func (s *Service) DeleteTerm(ctx context.Context, tenantID, termID uuid.UUID) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return s.repo.DeleteTerm(ctx, tx, tenantID, termID)
	})
}

// CreateClass opens a class (grade level).
func (s *Service) CreateClass(ctx context.Context, tenantID uuid.UUID, n NewGradeLevel, author Author) (GradeLevel, error) {
	if err := n.validate(); err != nil {
		return GradeLevel{}, err
	}
	var class GradeLevel
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		class, err = s.repo.CreateClass(ctx, tx, tenantID, n)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionClassCreated,
			TargetType: "grade_level", TargetID: class.ID.String(),
			Metadata: map[string]any{"name": class.Name},
		})
	})
	return class, err
}

// Classes lists a workspace's classes, junior to senior by rank.
func (s *Service) Classes(ctx context.Context, tenantID uuid.UUID) ([]GradeLevel, error) {
	var classes []GradeLevel
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		classes, err = s.repo.Classes(ctx, tx, tenantID, MaxClasses)
		return err
	})
	return classes, err
}

// DeleteClass removes a class and, by cascade, its sections.
func (s *Service) DeleteClass(ctx context.Context, tenantID, classID uuid.UUID) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return s.repo.DeleteClass(ctx, tx, tenantID, classID)
	})
}

// CreateSection adds a section to a class.
func (s *Service) CreateSection(ctx context.Context, tenantID, classID uuid.UUID, n NewSection) (Section, error) {
	if err := n.validate(); err != nil {
		return Section{}, err
	}
	var section Section
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		section, err = s.repo.CreateSection(ctx, tx, tenantID, classID, n)
		return err
	})
	return section, err
}

// Sections lists a class's sections by name.
func (s *Service) Sections(ctx context.Context, tenantID, classID uuid.UUID) ([]Section, error) {
	var sections []Section
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		sections, err = s.repo.Sections(ctx, tx, tenantID, classID)
		return err
	})
	return sections, err
}

// DeleteSection removes one section.
func (s *Service) DeleteSection(ctx context.Context, tenantID, sectionID uuid.UUID) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return s.repo.DeleteSection(ctx, tx, tenantID, sectionID)
	})
}

// CreateSubject adds a subject to the catalog.
func (s *Service) CreateSubject(ctx context.Context, tenantID uuid.UUID, n NewSubject) (Subject, error) {
	if err := n.validate(); err != nil {
		return Subject{}, err
	}
	var subject Subject
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		subject, err = s.repo.CreateSubject(ctx, tx, tenantID, n)
		return err
	})
	return subject, err
}

// Subjects lists the workspace's subjects by name.
func (s *Service) Subjects(ctx context.Context, tenantID uuid.UUID) ([]Subject, error) {
	var subjects []Subject
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		subjects, err = s.repo.Subjects(ctx, tx, tenantID, MaxSubjects)
		return err
	})
	return subjects, err
}

// DeleteSubject removes a subject.
func (s *Service) DeleteSubject(ctx context.Context, tenantID, subjectID uuid.UUID) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return s.repo.DeleteSubject(ctx, tx, tenantID, subjectID)
	})
}
