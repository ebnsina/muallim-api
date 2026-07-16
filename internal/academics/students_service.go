package academics

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Audit actions for the people layer.
const (
	ActionStudentAdmitted = "student.admitted"
)

// AdmitStudent puts a student on a roll.
func (s *Service) AdmitStudent(ctx context.Context, tenantID uuid.UUID, n NewStudent, author Author) (Student, error) {
	if err := n.validate(); err != nil {
		return Student{}, err
	}
	var student Student
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		student, err = s.repo.CreateStudent(ctx, tx, tenantID, n)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionStudentAdmitted,
			TargetType: "student", TargetID: student.ID.String(),
			Metadata: map[string]any{"admission_no": student.AdmissionNo},
		})
	})
	return student, err
}

// Roster returns one keyset page of students, filtered and sorted by name.
func (s *Service) Roster(ctx context.Context, tenantID uuid.UUID, filter RosterFilter, page PageParams) (RosterPage, error) {
	limit, after, err := page.clamp()
	if err != nil {
		return RosterPage{}, err
	}

	var rows []Student
	err = s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rows, err = s.repo.Roster(ctx, tx, tenantID, filter, after, limit+1)
		return err
	})
	if err != nil {
		return RosterPage{}, err
	}
	return paginate(rows, limit), nil
}

// Student loads one student.
func (s *Service) Student(ctx context.Context, tenantID, studentID uuid.UUID) (Student, error) {
	var student Student
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		student, err = s.repo.StudentByID(ctx, tx, tenantID, studentID)
		return err
	})
	return student, err
}

// EditStudent updates a student's details or placement.
func (s *Service) EditStudent(ctx context.Context, tenantID, studentID uuid.UUID, p StudentPatch) (Student, error) {
	if err := p.validate(); err != nil {
		return Student{}, err
	}
	var student Student
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		student, err = s.repo.UpdateStudent(ctx, tx, tenantID, studentID, p)
		return err
	})
	return student, err
}

// RemoveStudent deletes a student and, by cascade, their guardian links.
func (s *Service) RemoveStudent(ctx context.Context, tenantID, studentID uuid.UUID) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return s.repo.DeleteStudent(ctx, tx, tenantID, studentID)
	})
}

// AddGuardian records a guardian and links them to a student.
// ChildrenFor is the students a signed-in guardian or pupil may see through the
// portal. Read-only.
func (s *Service) ChildrenFor(ctx context.Context, tenantID, userID uuid.UUID) ([]Student, error) {
	var children []Student
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		children, err = s.repo.ChildrenFor(ctx, tx, tenantID, userID)
		return err
	})
	return children, err
}

// ChildStudent loads one of the user's own students, or ErrNotFound — the
// ownership gate every per-child portal read passes through first.
func (s *Service) ChildStudent(ctx context.Context, tenantID, userID, studentID uuid.UUID) (Student, error) {
	var st Student
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		st, err = s.repo.ChildStudent(ctx, tx, tenantID, userID, studentID)
		return err
	})
	return st, err
}

// LinkGuardianUser gives a guardian contact the account they sign in with. An
// administrative act — the portal read follows from it.
func (s *Service) LinkGuardianUser(ctx context.Context, tenantID, guardianID, userID uuid.UUID) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return s.repo.LinkGuardianUser(ctx, tx, tenantID, guardianID, userID)
	})
}

func (s *Service) AddGuardian(ctx context.Context, tenantID, studentID uuid.UUID, n NewGuardian) (Guardian, error) {
	if err := n.validate(); err != nil {
		return Guardian{}, err
	}
	var g Guardian
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		g, err = s.repo.AddGuardian(ctx, tx, tenantID, studentID, n)
		return err
	})
	return g, err
}

// Guardians lists a student's guardians, the primary contact first.
func (s *Service) Guardians(ctx context.Context, tenantID, studentID uuid.UUID) ([]Guardian, error) {
	var guardians []Guardian
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		guardians, err = s.repo.GuardiansOf(ctx, tx, tenantID, studentID)
		return err
	})
	return guardians, err
}

// RemoveGuardian unlinks a guardian from a student.
func (s *Service) RemoveGuardian(ctx context.Context, tenantID, studentID, guardianID uuid.UUID) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return s.repo.RemoveGuardian(ctx, tx, tenantID, studentID, guardianID)
	})
}
