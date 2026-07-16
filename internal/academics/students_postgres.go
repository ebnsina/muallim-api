package academics

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (r *PostgresRepository) CreateStudent(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewStudent) (Student, error) {
	var s Student
	err := tx.QueryRow(ctx,
		`INSERT INTO students (tenant_id, admission_no, full_name, grade_level_id, section_id, roll)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING id, admission_no, full_name, grade_level_id, section_id, roll, status, user_id, created_at, updated_at`,
		tenantID, n.AdmissionNo, n.FullName, n.GradeLevelID, n.SectionID, n.Roll).
		Scan(&s.ID, &s.AdmissionNo, &s.FullName, &s.GradeLevelID, &s.SectionID, &s.Roll, &s.Status, &s.UserID, &s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return Student{}, ErrAdmissionTaken
		}
		if isForeignKeyViolation(err) {
			return Student{}, ErrNotFound
		}
		return Student{}, fmt.Errorf("academics: admit student: %w", err)
	}
	return s, nil
}

const rosterColumns = `id, admission_no, full_name, grade_level_id, section_id, roll, status, user_id, created_at, updated_at`

// Two statements, not one with `grade_level_id = $x OR $x IS NULL`: that predicate
// is not sargable, so the planner would abandon the grade index even when a class
// is named. Each shape gets the index that covers its filter and its sort.
const rosterAllSQL = `
	SELECT ` + rosterColumns + `
	FROM students
	WHERE tenant_id = $1
	  AND ($2::text IS NULL OR (full_name, id) > ($2, $3::uuid))
	ORDER BY full_name, id
	LIMIT $4`

const rosterByGradeSQL = `
	SELECT ` + rosterColumns + `
	FROM students
	WHERE tenant_id = $1 AND grade_level_id = $5
	  AND ($2::text IS NULL OR (full_name, id) > ($2, $3::uuid))
	ORDER BY full_name, id
	LIMIT $4`

func (r *PostgresRepository) Roster(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, filter RosterFilter, after *cursor, limit int) ([]Student, error) {
	var afterName, afterID any
	if after != nil {
		afterName, afterID = after.Name, after.ID
	}

	var rows pgx.Rows
	var err error
	if filter.GradeLevelID != nil {
		rows, err = tx.Query(ctx, rosterByGradeSQL, tenantID, afterName, afterID, limit, *filter.GradeLevelID)
	} else {
		rows, err = tx.Query(ctx, rosterAllSQL, tenantID, afterName, afterID, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("academics: roster: %w", err)
	}
	defer rows.Close()

	return pgx.CollectRows(rows, scanStudent)
}

func (r *PostgresRepository) StudentByID(ctx context.Context, tx pgx.Tx, tenantID, studentID uuid.UUID) (Student, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+rosterColumns+` FROM students WHERE tenant_id = $1 AND id = $2`, tenantID, studentID)
	if err != nil {
		return Student{}, fmt.Errorf("academics: load student: %w", err)
	}
	defer rows.Close()

	s, err := pgx.CollectExactlyOneRow(rows, scanStudent)
	if errors.Is(err, pgx.ErrNoRows) {
		return Student{}, ErrNotFound
	}
	return s, err
}

func (r *PostgresRepository) UpdateStudent(ctx context.Context, tx pgx.Tx, tenantID, studentID uuid.UUID, p StudentPatch) (Student, error) {
	var s Student
	err := tx.QueryRow(ctx,
		`UPDATE students SET
		     full_name      = coalesce($3, full_name),
		     grade_level_id = coalesce($4::uuid, grade_level_id),
		     section_id     = coalesce($5::uuid, section_id),
		     roll           = coalesce($6, roll),
		     status         = coalesce($7, status),
		     updated_at     = now()
		 WHERE tenant_id = $1 AND id = $2
		 RETURNING `+rosterColumns,
		tenantID, studentID, p.FullName, p.GradeLevelID, p.SectionID, p.Roll, p.Status).
		Scan(&s.ID, &s.AdmissionNo, &s.FullName, &s.GradeLevelID, &s.SectionID, &s.Roll, &s.Status, &s.UserID, &s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Student{}, ErrNotFound
		}
		if isForeignKeyViolation(err) {
			return Student{}, ErrNotFound
		}
		return Student{}, fmt.Errorf("academics: update student: %w", err)
	}
	return s, nil
}

func (r *PostgresRepository) DeleteStudent(ctx context.Context, tx pgx.Tx, tenantID, studentID uuid.UUID) error {
	tag, err := tx.Exec(ctx, `DELETE FROM students WHERE tenant_id = $1 AND id = $2`, tenantID, studentID)
	if err != nil {
		return fmt.Errorf("academics: delete student: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *PostgresRepository) AddGuardian(ctx context.Context, tx pgx.Tx, tenantID, studentID uuid.UUID, n NewGuardian) (Guardian, error) {
	// A student may have one primary contact; naming a new one demotes the old.
	if n.IsPrimary {
		if _, err := tx.Exec(ctx,
			`UPDATE student_guardians SET is_primary = false WHERE tenant_id = $1 AND student_id = $2`,
			tenantID, studentID); err != nil {
			return Guardian{}, fmt.Errorf("academics: clear primary guardian: %w", err)
		}
	}

	var g Guardian
	err := tx.QueryRow(ctx,
		`INSERT INTO guardians (tenant_id, full_name, phone, email, relation)
		 VALUES ($1, $2, $3, $4, $5)
		 RETURNING id, full_name, phone, email, relation, created_at`,
		tenantID, n.FullName, n.Phone, n.Email, n.Relation).
		Scan(&g.ID, &g.FullName, &g.Phone, &g.Email, &g.Relation, &g.CreatedAt)
	if err != nil {
		return Guardian{}, fmt.Errorf("academics: create guardian: %w", err)
	}

	_, err = tx.Exec(ctx,
		`INSERT INTO student_guardians (tenant_id, student_id, guardian_id, is_primary)
		 VALUES ($1, $2, $3, $4)`,
		tenantID, studentID, g.ID, n.IsPrimary)
	if err != nil {
		if isForeignKeyViolation(err) {
			return Guardian{}, ErrNotFound
		}
		return Guardian{}, fmt.Errorf("academics: link guardian: %w", err)
	}
	g.IsPrimary = n.IsPrimary
	return g, nil
}

func (r *PostgresRepository) GuardiansOf(ctx context.Context, tx pgx.Tx, tenantID, studentID uuid.UUID) ([]Guardian, error) {
	rows, err := tx.Query(ctx,
		`SELECT g.id, g.full_name, g.phone, g.email, g.relation, sg.is_primary, g.created_at
		 FROM student_guardians sg
		 JOIN guardians g ON g.id = sg.guardian_id AND g.tenant_id = sg.tenant_id
		 WHERE sg.tenant_id = $1 AND sg.student_id = $2
		 ORDER BY sg.is_primary DESC, g.full_name, g.id`,
		tenantID, studentID)
	if err != nil {
		return nil, fmt.Errorf("academics: list guardians: %w", err)
	}
	defer rows.Close()

	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Guardian, error) {
		var g Guardian
		err := row.Scan(&g.ID, &g.FullName, &g.Phone, &g.Email, &g.Relation, &g.IsPrimary, &g.CreatedAt)
		return g, err
	})
}

func (r *PostgresRepository) RemoveGuardian(ctx context.Context, tx pgx.Tx, tenantID, studentID, guardianID uuid.UUID) error {
	tag, err := tx.Exec(ctx,
		`DELETE FROM student_guardians WHERE tenant_id = $1 AND student_id = $2 AND guardian_id = $3`,
		tenantID, studentID, guardianID)
	if err != nil {
		return fmt.Errorf("academics: remove guardian: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// LinkGuardianUser binds a guardian contact to the login that will read their
// child's day. The portal's authorisation hangs off this column.
func (r *PostgresRepository) LinkGuardianUser(ctx context.Context, tx pgx.Tx, tenantID, guardianID, userID uuid.UUID) error {
	tag, err := tx.Exec(ctx,
		`UPDATE guardians SET user_id = $3, updated_at = now() WHERE tenant_id = $1 AND id = $2`,
		tenantID, guardianID, userID)
	if err != nil {
		return fmt.Errorf("academics: link guardian user: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ChildrenFor is the students a signed-in user may see: their own record (a pupil
// with an account) and every child they are the guardian of. One query — the OR
// never doubles a row, since a student appears once in `students`.
func (r *PostgresRepository) ChildrenFor(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID) ([]Student, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+rosterColumns+` FROM students
		 WHERE tenant_id = $1 AND (
		   user_id = $2
		   OR id IN (
		     SELECT sg.student_id FROM student_guardians sg
		     JOIN guardians g ON g.id = sg.guardian_id AND g.tenant_id = sg.tenant_id
		     WHERE sg.tenant_id = $1 AND g.user_id = $2
		   )
		 )
		 ORDER BY full_name, id`,
		tenantID, userID)
	if err != nil {
		return nil, fmt.Errorf("academics: children for: %w", err)
	}
	defer rows.Close()
	return pgx.CollectRows(rows, scanStudent)
}

// ChildStudent loads one student only if the user may see them — their own record
// or a child they guard. A student the user is not tied to comes back as
// ErrNotFound, never their data: the portal admits existence to no one.
func (r *PostgresRepository) ChildStudent(ctx context.Context, tx pgx.Tx, tenantID, userID, studentID uuid.UUID) (Student, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+rosterColumns+` FROM students
		 WHERE tenant_id = $1 AND id = $3 AND (
		   user_id = $2
		   OR id IN (
		     SELECT sg.student_id FROM student_guardians sg
		     JOIN guardians g ON g.id = sg.guardian_id AND g.tenant_id = sg.tenant_id
		     WHERE sg.tenant_id = $1 AND g.user_id = $2
		   )
		 )`,
		tenantID, userID, studentID)
	if err != nil {
		return Student{}, fmt.Errorf("academics: child student: %w", err)
	}
	s, err := pgx.CollectExactlyOneRow(rows, scanStudent)
	if errors.Is(err, pgx.ErrNoRows) {
		return Student{}, ErrNotFound
	}
	if err != nil {
		return Student{}, fmt.Errorf("academics: child student: %w", err)
	}
	return s, nil
}

func scanStudent(row pgx.CollectableRow) (Student, error) {
	var s Student
	err := row.Scan(&s.ID, &s.AdmissionNo, &s.FullName, &s.GradeLevelID, &s.SectionID, &s.Roll, &s.Status, &s.UserID, &s.CreatedAt, &s.UpdatedAt)
	return s, err
}
