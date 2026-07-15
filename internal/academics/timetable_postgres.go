package academics

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Times cross the boundary as HH:MM strings: written with ::time, read with
// to_char so a caller never sees the seconds Postgres keeps.
const periodColumns = `id, section_id, subject_id, day_of_week,
	to_char(starts_at, 'HH24:MI'), to_char(ends_at, 'HH24:MI'), teacher_name, room`

func (r *PostgresRepository) CreatePeriod(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewPeriod) (Period, error) {
	rows, err := tx.Query(ctx,
		`INSERT INTO timetable_periods
		     (tenant_id, section_id, subject_id, day_of_week, starts_at, ends_at, teacher_name, room)
		 VALUES ($1, $2, $3, $4, $5::time, $6::time, $7, $8)
		 RETURNING `+periodColumns,
		tenantID, n.SectionID, n.SubjectID, n.DayOfWeek, n.StartsAt, n.EndsAt, n.TeacherName, n.Room)
	if err != nil {
		return Period{}, fmt.Errorf("academics: create period: %w", err)
	}
	p, err := pgx.CollectExactlyOneRow(rows, scanPeriod)
	if isUniqueViolation(err) {
		return Period{}, fmt.Errorf("%w: that slot is already taken", ErrInvalidPeriod)
	}
	if isForeignKeyViolation(err) {
		return Period{}, ErrNotFound
	}
	if err != nil {
		return Period{}, fmt.Errorf("academics: create period: %w", err)
	}
	return p, nil
}

func (r *PostgresRepository) SectionTimetable(ctx context.Context, tx pgx.Tx, tenantID, sectionID uuid.UUID) ([]Period, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+periodColumns+` FROM timetable_periods
		 WHERE tenant_id = $1 AND section_id = $2
		 ORDER BY day_of_week, starts_at`,
		tenantID, sectionID)
	if err != nil {
		return nil, fmt.Errorf("academics: section timetable: %w", err)
	}
	defer rows.Close()
	return pgx.CollectRows(rows, scanPeriod)
}

func (r *PostgresRepository) DeletePeriod(ctx context.Context, tx pgx.Tx, tenantID, periodID uuid.UUID) error {
	tag, err := tx.Exec(ctx, `DELETE FROM timetable_periods WHERE tenant_id = $1 AND id = $2`, tenantID, periodID)
	if err != nil {
		return fmt.Errorf("academics: delete period: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func scanPeriod(row pgx.CollectableRow) (Period, error) {
	var p Period
	err := row.Scan(&p.ID, &p.SectionID, &p.SubjectID, &p.DayOfWeek, &p.StartsAt, &p.EndsAt, &p.TeacherName, &p.Room)
	return p, err
}
