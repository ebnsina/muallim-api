package exams

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// PostgresRepository is the Postgres-backed Repository.
type PostgresRepository struct{}

// NewPostgresRepository returns a PostgresRepository.
func NewPostgresRepository() *PostgresRepository { return &PostgresRepository{} }

func isUniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

func isForeignKeyViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23503"
}

func (r *PostgresRepository) DefaultScale(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (GradingScale, error) {
	var s GradingScale
	err := tx.QueryRow(ctx,
		`SELECT id, name, is_default, created_at FROM exam_scales
		 WHERE tenant_id = $1 AND is_default LIMIT 1`, tenantID).
		Scan(&s.ID, &s.Name, &s.IsDefault, &s.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return GradingScale{}, ErrNotFound
	}
	if err != nil {
		return GradingScale{}, fmt.Errorf("exams: default scale: %w", err)
	}
	s.Bands, err = r.ScaleBands(ctx, tx, tenantID, s.ID)
	return s, err
}

func (r *PostgresRepository) CreateScale(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewScale) (GradingScale, error) {
	var s GradingScale
	err := tx.QueryRow(ctx,
		`INSERT INTO exam_scales (tenant_id, name, is_default) VALUES ($1, $2, $3)
		 RETURNING id, name, is_default, created_at`,
		tenantID, n.Name, n.IsDefault).
		Scan(&s.ID, &s.Name, &s.IsDefault, &s.CreatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return GradingScale{}, fmt.Errorf("%w: a default scale already exists", ErrInvalidScale)
		}
		return GradingScale{}, fmt.Errorf("exams: create scale: %w", err)
	}

	letters := make([]string, len(n.Bands))
	mins := make([]float64, len(n.Bands))
	points := make([]float64, len(n.Bands))
	passes := make([]bool, len(n.Bands))
	for i, b := range n.Bands {
		letters[i], mins[i], points[i], passes[i] = b.Letter, b.MinPercent, b.GPAPoint, b.IsPass
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO exam_bands (tenant_id, scale_id, letter, min_percent, gpa_point, is_pass)
		 SELECT $1, $2, b.letter, b.min_percent, b.gpa_point, b.is_pass
		 FROM unnest($3::text[], $4::float8[], $5::float8[], $6::bool[])
		      AS b(letter, min_percent, gpa_point, is_pass)`,
		tenantID, s.ID, letters, mins, points, passes)
	if err != nil {
		return GradingScale{}, fmt.Errorf("exams: create bands: %w", err)
	}
	s.Bands = n.Bands
	return s, nil
}

func (r *PostgresRepository) Scales(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, limit int) ([]GradingScale, error) {
	rows, err := tx.Query(ctx,
		`SELECT id, name, is_default, created_at FROM exam_scales
		 WHERE tenant_id = $1 ORDER BY is_default DESC, name, id LIMIT $2`, tenantID, limit)
	if err != nil {
		return nil, fmt.Errorf("exams: scales: %w", err)
	}
	scales, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (GradingScale, error) {
		var s GradingScale
		err := row.Scan(&s.ID, &s.Name, &s.IsDefault, &s.CreatedAt)
		return s, err
	})
	if err != nil {
		return nil, err
	}
	if len(scales) == 0 {
		return scales, nil
	}

	// Batch the bands for every scale at once; stitch by scale id. Two queries for
	// any number of scales, never one per scale.
	ids := make([]uuid.UUID, len(scales))
	for i, s := range scales {
		ids[i] = s.ID
	}
	bandRows, err := tx.Query(ctx,
		`SELECT scale_id, letter, min_percent::float8, gpa_point::float8, is_pass
		 FROM exam_bands WHERE tenant_id = $1 AND scale_id = ANY($2)
		 ORDER BY min_percent DESC`, tenantID, ids)
	if err != nil {
		return nil, fmt.Errorf("exams: scale bands: %w", err)
	}
	defer bandRows.Close()

	byScale := make(map[uuid.UUID][]Band, len(scales))
	for bandRows.Next() {
		var sid uuid.UUID
		var b Band
		if err := bandRows.Scan(&sid, &b.Letter, &b.MinPercent, &b.GPAPoint, &b.IsPass); err != nil {
			return nil, err
		}
		byScale[sid] = append(byScale[sid], b)
	}
	if err := bandRows.Err(); err != nil {
		return nil, err
	}
	for i := range scales {
		scales[i].Bands = byScale[scales[i].ID]
	}
	return scales, nil
}

func (r *PostgresRepository) ScaleBands(ctx context.Context, tx pgx.Tx, tenantID, scaleID uuid.UUID) ([]Band, error) {
	rows, err := tx.Query(ctx,
		`SELECT letter, min_percent::float8, gpa_point::float8, is_pass
		 FROM exam_bands WHERE tenant_id = $1 AND scale_id = $2
		 ORDER BY min_percent DESC`, tenantID, scaleID)
	if err != nil {
		return nil, fmt.Errorf("exams: bands: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Band, error) {
		var b Band
		err := row.Scan(&b.Letter, &b.MinPercent, &b.GPAPoint, &b.IsPass)
		return b, err
	})
}

const examColumns = `id, name, term_id, grade_level_id, scale_id, held_on, status, created_at, updated_at`

func (r *PostgresRepository) CreateExam(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewExam) (Exam, error) {
	var e Exam
	err := tx.QueryRow(ctx,
		`INSERT INTO exams (tenant_id, name, term_id, grade_level_id, scale_id, held_on)
		 VALUES ($1, $2, $3, $4, $5, $6) RETURNING `+examColumns,
		tenantID, n.Name, n.TermID, n.GradeLevelID, n.ScaleID, n.HeldOn).
		Scan(&e.ID, &e.Name, &e.TermID, &e.GradeLevelID, &e.ScaleID, &e.HeldOn, &e.Status, &e.CreatedAt, &e.UpdatedAt)
	if err != nil {
		if isForeignKeyViolation(err) {
			return Exam{}, ErrNotFound
		}
		return Exam{}, fmt.Errorf("exams: create exam: %w", err)
	}
	return e, nil
}

func (r *PostgresRepository) Exams(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, after *cursor, limit int) ([]Exam, error) {
	var afterTime, afterID any
	if after != nil {
		afterTime, afterID = after.CreatedAt, after.ID
	}
	rows, err := tx.Query(ctx,
		`SELECT `+examColumns+` FROM exams
		 WHERE tenant_id = $1
		   AND ($2::timestamptz IS NULL OR (created_at, id) < ($2, $3::uuid))
		 ORDER BY created_at DESC, id DESC LIMIT $4`,
		tenantID, afterTime, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("exams: list: %w", err)
	}
	defer rows.Close()
	return pgx.CollectRows(rows, scanExam)
}

func (r *PostgresRepository) ExamByID(ctx context.Context, tx pgx.Tx, tenantID, examID uuid.UUID) (Exam, error) {
	rows, err := tx.Query(ctx, `SELECT `+examColumns+` FROM exams WHERE tenant_id = $1 AND id = $2`, tenantID, examID)
	if err != nil {
		return Exam{}, fmt.Errorf("exams: load exam: %w", err)
	}
	defer rows.Close()
	e, err := pgx.CollectExactlyOneRow(rows, scanExam)
	if errors.Is(err, pgx.ErrNoRows) {
		return Exam{}, ErrNotFound
	}
	return e, err
}

func (r *PostgresRepository) PublishExam(ctx context.Context, tx pgx.Tx, tenantID, examID uuid.UUID) (Exam, error) {
	var e Exam
	err := tx.QueryRow(ctx,
		`UPDATE exams SET status = 'published', updated_at = now()
		 WHERE tenant_id = $1 AND id = $2 RETURNING `+examColumns,
		tenantID, examID).
		Scan(&e.ID, &e.Name, &e.TermID, &e.GradeLevelID, &e.ScaleID, &e.HeldOn, &e.Status, &e.CreatedAt, &e.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Exam{}, ErrNotFound
	}
	if err != nil {
		return Exam{}, fmt.Errorf("exams: publish: %w", err)
	}
	return e, nil
}

func (r *PostgresRepository) DeleteExam(ctx context.Context, tx pgx.Tx, tenantID, examID uuid.UUID) error {
	tag, err := tx.Exec(ctx, `DELETE FROM exams WHERE tenant_id = $1 AND id = $2`, tenantID, examID)
	if err != nil {
		return fmt.Errorf("exams: delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *PostgresRepository) UpsertMarks(ctx context.Context, tx pgx.Tx, tenantID, examID, markedBy uuid.UUID, marks []Mark) (int, error) {
	students := make([]uuid.UUID, len(marks))
	subjects := make([]uuid.UUID, len(marks))
	fulls := make([]float64, len(marks))
	obtained := make([]float64, len(marks))
	for i, m := range marks {
		students[i], subjects[i], fulls[i], obtained[i] = m.StudentID, m.SubjectID, m.FullMarks, m.Obtained
	}
	tag, err := tx.Exec(ctx,
		`INSERT INTO exam_marks (tenant_id, exam_id, student_id, subject_id, full_marks, obtained, marked_by)
		 SELECT $1, $2, m.student_id, m.subject_id, m.full_marks, m.obtained, $3
		 FROM unnest($4::uuid[], $5::uuid[], $6::float8[], $7::float8[])
		      AS m(student_id, subject_id, full_marks, obtained)
		 ON CONFLICT (tenant_id, exam_id, student_id, subject_id)
		 DO UPDATE SET full_marks = EXCLUDED.full_marks, obtained = EXCLUDED.obtained,
		               marked_by = EXCLUDED.marked_by, updated_at = now()`,
		tenantID, examID, markedBy, students, subjects, fulls, obtained)
	if err != nil {
		if isForeignKeyViolation(err) {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("exams: enter marks: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

func (r *PostgresRepository) StudentMarks(ctx context.Context, tx pgx.Tx, tenantID, examID, studentID uuid.UUID) ([]markRow, error) {
	rows, err := tx.Query(ctx,
		`SELECT em.subject_id, sub.name, em.full_marks::float8, em.obtained::float8
		 FROM exam_marks em
		 JOIN subjects sub ON sub.id = em.subject_id AND sub.tenant_id = em.tenant_id
		 WHERE em.tenant_id = $1 AND em.exam_id = $2 AND em.student_id = $3
		 ORDER BY sub.name, sub.id`,
		tenantID, examID, studentID)
	if err != nil {
		return nil, fmt.Errorf("exams: student marks: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (markRow, error) {
		m := markRow{StudentID: studentID}
		err := row.Scan(&m.SubjectID, &m.SubjectName, &m.FullMarks, &m.Obtained)
		return m, err
	})
}

func scanExam(row pgx.CollectableRow) (Exam, error) {
	var e Exam
	err := row.Scan(&e.ID, &e.Name, &e.TermID, &e.GradeLevelID, &e.ScaleID, &e.HeldOn, &e.Status, &e.CreatedAt, &e.UpdatedAt)
	return e, err
}
