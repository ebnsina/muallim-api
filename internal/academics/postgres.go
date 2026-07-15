package academics

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// PostgresRepository is the Repository over Postgres. It receives a pgx.Tx bound to
// the tenant, never the pool.
type PostgresRepository struct{}

// NewPostgresRepository returns a Repository backed by Postgres.
func NewPostgresRepository() *PostgresRepository { return &PostgresRepository{} }

func (r *PostgresRepository) InstitutionType(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (string, error) {
	var kind string
	err := tx.QueryRow(ctx, `SELECT institution_type FROM tenants WHERE id = $1`, tenantID).Scan(&kind)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("academics: read institution type: %w", err)
	}
	return kind, nil
}

func (r *PostgresRepository) SetInstitutionType(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, kind string) error {
	tag, err := tx.Exec(ctx,
		`UPDATE tenants SET institution_type = $2, updated_at = now() WHERE id = $1`, tenantID, kind)
	if err != nil {
		return fmt.Errorf("academics: set institution type: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *PostgresRepository) CreateYear(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewAcademicYear) (AcademicYear, error) {
	var y AcademicYear
	err := tx.QueryRow(ctx,
		`INSERT INTO academic_years (tenant_id, name, starts_on, ends_on)
		 VALUES ($1, $2, $3, $4)
		 RETURNING id, name, starts_on, ends_on, is_current, created_at, updated_at`,
		tenantID, n.Name, n.StartsOn, n.EndsOn).
		Scan(&y.ID, &y.Name, &y.StartsOn, &y.EndsOn, &y.IsCurrent, &y.CreatedAt, &y.UpdatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return AcademicYear{}, ErrNameTaken
		}
		return AcademicYear{}, fmt.Errorf("academics: create year: %w", err)
	}
	return y, nil
}

func (r *PostgresRepository) Years(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, limit int) ([]AcademicYear, error) {
	rows, err := tx.Query(ctx,
		`SELECT id, name, starts_on, ends_on, is_current, created_at, updated_at
		 FROM academic_years WHERE tenant_id = $1
		 ORDER BY is_current DESC, starts_on DESC, id
		 LIMIT $2`, tenantID, limit)
	if err != nil {
		return nil, fmt.Errorf("academics: list years: %w", err)
	}
	defer rows.Close()

	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (AcademicYear, error) {
		var y AcademicYear
		err := row.Scan(&y.ID, &y.Name, &y.StartsOn, &y.EndsOn, &y.IsCurrent, &y.CreatedAt, &y.UpdatedAt)
		return y, err
	})
}

// SetCurrentYear clears any other current year first, then sets this one — two
// statements, because the partial unique index forbids two current years even for
// the instant a single multi-row UPDATE would take to swap them.
func (r *PostgresRepository) SetCurrentYear(ctx context.Context, tx pgx.Tx, tenantID, yearID uuid.UUID) (AcademicYear, error) {
	if _, err := tx.Exec(ctx,
		`UPDATE academic_years SET is_current = false, updated_at = now()
		 WHERE tenant_id = $1 AND is_current AND id <> $2`, tenantID, yearID); err != nil {
		return AcademicYear{}, fmt.Errorf("academics: clear current year: %w", err)
	}

	var y AcademicYear
	err := tx.QueryRow(ctx,
		`UPDATE academic_years SET is_current = true, updated_at = now()
		 WHERE tenant_id = $1 AND id = $2
		 RETURNING id, name, starts_on, ends_on, is_current, created_at, updated_at`,
		tenantID, yearID).
		Scan(&y.ID, &y.Name, &y.StartsOn, &y.EndsOn, &y.IsCurrent, &y.CreatedAt, &y.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return AcademicYear{}, ErrNotFound
		}
		return AcademicYear{}, fmt.Errorf("academics: set current year: %w", err)
	}
	return y, nil
}

func (r *PostgresRepository) DeleteYear(ctx context.Context, tx pgx.Tx, tenantID, yearID uuid.UUID) error {
	tag, err := tx.Exec(ctx, `DELETE FROM academic_years WHERE tenant_id = $1 AND id = $2`, tenantID, yearID)
	if err != nil {
		return fmt.Errorf("academics: delete year: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *PostgresRepository) CreateTerm(ctx context.Context, tx pgx.Tx, tenantID, yearID uuid.UUID, n NewTerm) (Term, error) {
	var t Term
	err := tx.QueryRow(ctx,
		`INSERT INTO terms (tenant_id, academic_year_id, name, starts_on, ends_on, position)
		 SELECT $1, $2, $3, $4, $5, coalesce(max(position) + 1, 0)
		 FROM terms WHERE tenant_id = $1 AND academic_year_id = $2
		 RETURNING id, academic_year_id, name, starts_on, ends_on, position, created_at, updated_at`,
		tenantID, yearID, n.Name, n.StartsOn, n.EndsOn).
		Scan(&t.ID, &t.AcademicYearID, &t.Name, &t.StartsOn, &t.EndsOn, &t.Position, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		if isForeignKeyViolation(err) {
			return Term{}, ErrNotFound
		}
		return Term{}, fmt.Errorf("academics: create term: %w", err)
	}
	return t, nil
}

func (r *PostgresRepository) Terms(ctx context.Context, tx pgx.Tx, tenantID, yearID uuid.UUID) ([]Term, error) {
	rows, err := tx.Query(ctx,
		`SELECT id, academic_year_id, name, starts_on, ends_on, position, created_at, updated_at
		 FROM terms WHERE tenant_id = $1 AND academic_year_id = $2
		 ORDER BY position, id`, tenantID, yearID)
	if err != nil {
		return nil, fmt.Errorf("academics: list terms: %w", err)
	}
	defer rows.Close()

	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Term, error) {
		var t Term
		err := row.Scan(&t.ID, &t.AcademicYearID, &t.Name, &t.StartsOn, &t.EndsOn, &t.Position, &t.CreatedAt, &t.UpdatedAt)
		return t, err
	})
}

func (r *PostgresRepository) DeleteTerm(ctx context.Context, tx pgx.Tx, tenantID, termID uuid.UUID) error {
	tag, err := tx.Exec(ctx, `DELETE FROM terms WHERE tenant_id = $1 AND id = $2`, tenantID, termID)
	if err != nil {
		return fmt.Errorf("academics: delete term: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *PostgresRepository) CreateClass(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewGradeLevel) (GradeLevel, error) {
	var g GradeLevel
	err := tx.QueryRow(ctx,
		`INSERT INTO grade_levels (tenant_id, name, rank)
		 VALUES ($1, $2, $3)
		 RETURNING id, name, rank, created_at, updated_at`,
		tenantID, n.Name, n.Rank).
		Scan(&g.ID, &g.Name, &g.Rank, &g.CreatedAt, &g.UpdatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return GradeLevel{}, ErrNameTaken
		}
		return GradeLevel{}, fmt.Errorf("academics: create class: %w", err)
	}
	return g, nil
}

func (r *PostgresRepository) Classes(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, limit int) ([]GradeLevel, error) {
	rows, err := tx.Query(ctx,
		`SELECT id, name, rank, created_at, updated_at
		 FROM grade_levels WHERE tenant_id = $1
		 ORDER BY rank, id LIMIT $2`, tenantID, limit)
	if err != nil {
		return nil, fmt.Errorf("academics: list classes: %w", err)
	}
	defer rows.Close()

	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (GradeLevel, error) {
		var g GradeLevel
		err := row.Scan(&g.ID, &g.Name, &g.Rank, &g.CreatedAt, &g.UpdatedAt)
		return g, err
	})
}

func (r *PostgresRepository) DeleteClass(ctx context.Context, tx pgx.Tx, tenantID, classID uuid.UUID) error {
	tag, err := tx.Exec(ctx, `DELETE FROM grade_levels WHERE tenant_id = $1 AND id = $2`, tenantID, classID)
	if err != nil {
		return fmt.Errorf("academics: delete class: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *PostgresRepository) CreateSection(ctx context.Context, tx pgx.Tx, tenantID, classID uuid.UUID, n NewSection) (Section, error) {
	var sec Section
	err := tx.QueryRow(ctx,
		`INSERT INTO sections (tenant_id, grade_level_id, name, capacity)
		 VALUES ($1, $2, $3, $4)
		 RETURNING id, grade_level_id, name, capacity, created_at, updated_at`,
		tenantID, classID, n.Name, n.Capacity).
		Scan(&sec.ID, &sec.GradeLevelID, &sec.Name, &sec.Capacity, &sec.CreatedAt, &sec.UpdatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return Section{}, ErrNameTaken
		}
		if isForeignKeyViolation(err) {
			return Section{}, ErrNotFound
		}
		return Section{}, fmt.Errorf("academics: create section: %w", err)
	}
	return sec, nil
}

func (r *PostgresRepository) Sections(ctx context.Context, tx pgx.Tx, tenantID, classID uuid.UUID) ([]Section, error) {
	rows, err := tx.Query(ctx,
		`SELECT id, grade_level_id, name, capacity, created_at, updated_at
		 FROM sections WHERE tenant_id = $1 AND grade_level_id = $2
		 ORDER BY name, id`, tenantID, classID)
	if err != nil {
		return nil, fmt.Errorf("academics: list sections: %w", err)
	}
	defer rows.Close()

	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Section, error) {
		var sec Section
		err := row.Scan(&sec.ID, &sec.GradeLevelID, &sec.Name, &sec.Capacity, &sec.CreatedAt, &sec.UpdatedAt)
		return sec, err
	})
}

func (r *PostgresRepository) DeleteSection(ctx context.Context, tx pgx.Tx, tenantID, sectionID uuid.UUID) error {
	tag, err := tx.Exec(ctx, `DELETE FROM sections WHERE tenant_id = $1 AND id = $2`, tenantID, sectionID)
	if err != nil {
		return fmt.Errorf("academics: delete section: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *PostgresRepository) CreateSubject(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewSubject) (Subject, error) {
	var s Subject
	err := tx.QueryRow(ctx,
		`INSERT INTO subjects (tenant_id, name, code)
		 VALUES ($1, $2, $3)
		 RETURNING id, name, code, created_at, updated_at`,
		tenantID, n.Name, n.Code).
		Scan(&s.ID, &s.Name, &s.Code, &s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return Subject{}, ErrNameTaken
		}
		return Subject{}, fmt.Errorf("academics: create subject: %w", err)
	}
	return s, nil
}

func (r *PostgresRepository) Subjects(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, limit int) ([]Subject, error) {
	rows, err := tx.Query(ctx,
		`SELECT id, name, code, created_at, updated_at
		 FROM subjects WHERE tenant_id = $1
		 ORDER BY name, id LIMIT $2`, tenantID, limit)
	if err != nil {
		return nil, fmt.Errorf("academics: list subjects: %w", err)
	}
	defer rows.Close()

	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Subject, error) {
		var s Subject
		err := row.Scan(&s.ID, &s.Name, &s.Code, &s.CreatedAt, &s.UpdatedAt)
		return s, err
	})
}

func (r *PostgresRepository) DeleteSubject(ctx context.Context, tx pgx.Tx, tenantID, subjectID uuid.UUID) error {
	tag, err := tx.Exec(ctx, `DELETE FROM subjects WHERE tenant_id = $1 AND id = $2`, tenantID, subjectID)
	if err != nil {
		return fmt.Errorf("academics: delete subject: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}
