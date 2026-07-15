package academics

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// MarkAttendance upserts one day's register in a single statement. The status
// arrays are unnested alongside the student ids; a day already marked is updated,
// not duplicated, on the (tenant, student, day) unique index. One query for any
// number of students.
func (r *PostgresRepository) MarkAttendance(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, m AttendanceMark) (int, error) {
	studentIDs := make([]uuid.UUID, len(m.Entries))
	statuses := make([]string, len(m.Entries))
	for i, e := range m.Entries {
		studentIDs[i] = e.StudentID
		statuses[i] = e.Status
	}

	tag, err := tx.Exec(ctx,
		`INSERT INTO attendance (tenant_id, student_id, section_id, on_date, status, marked_by)
		 SELECT $1, s.student_id, $2::uuid, $3::date, s.status, $4
		 FROM unnest($5::uuid[], $6::text[]) AS s(student_id, status)
		 ON CONFLICT (tenant_id, student_id, on_date)
		 DO UPDATE SET status = EXCLUDED.status, section_id = EXCLUDED.section_id,
		               marked_by = EXCLUDED.marked_by, updated_at = now()`,
		tenantID, m.SectionID, m.OnDate, m.MarkedBy, studentIDs, statuses)
	if err != nil {
		if isForeignKeyViolation(err) {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("academics: mark attendance: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// Register reads a section's marks for one day, joined to student names. The
// (tenant, section, on_date) index covers the filter.
func (r *PostgresRepository) Register(ctx context.Context, tx pgx.Tx, tenantID, sectionID uuid.UUID, on time.Time) ([]RegisterEntry, error) {
	rows, err := tx.Query(ctx,
		`SELECT a.student_id, st.admission_no, st.full_name, a.status
		 FROM attendance a
		 JOIN students st ON st.id = a.student_id AND st.tenant_id = a.tenant_id
		 WHERE a.tenant_id = $1 AND a.section_id = $2 AND a.on_date = $3::date
		 ORDER BY st.full_name, st.id`,
		tenantID, sectionID, on)
	if err != nil {
		return nil, fmt.Errorf("academics: register: %w", err)
	}
	defer rows.Close()

	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (RegisterEntry, error) {
		var e RegisterEntry
		err := row.Scan(&e.StudentID, &e.AdmissionNo, &e.FullName, &e.Status)
		return e, err
	})
}

// StudentAttendance reads one student's days in a range and their tally. The
// (tenant, student, on_date) index covers both the filter and the ordering.
func (r *PostgresRepository) StudentAttendance(ctx context.Context, tx pgx.Tx, tenantID, studentID uuid.UUID, from, to time.Time) ([]AttendanceDay, AttendanceSummary, error) {
	rows, err := tx.Query(ctx,
		`SELECT on_date, section_id, status
		 FROM attendance
		 WHERE tenant_id = $1 AND student_id = $2 AND on_date BETWEEN $3::date AND $4::date
		 ORDER BY on_date, id`,
		tenantID, studentID, from, to)
	if err != nil {
		return nil, AttendanceSummary{}, fmt.Errorf("academics: student attendance: %w", err)
	}
	defer rows.Close()

	days, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (AttendanceDay, error) {
		var d AttendanceDay
		err := row.Scan(&d.OnDate, &d.SectionID, &d.Status)
		return d, err
	})
	if err != nil {
		return nil, AttendanceSummary{}, err
	}

	var sum AttendanceSummary
	for _, d := range days {
		switch d.Status {
		case Present:
			sum.Present++
		case Absent:
			sum.Absent++
		case Late:
			sum.Late++
		case Excused:
			sum.Excused++
		}
	}
	sum.Total = len(days)
	return days, sum, nil
}
