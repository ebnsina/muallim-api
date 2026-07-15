package overview

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// PostgresRepository is the Postgres-backed Repository.
type PostgresRepository struct{}

// NewPostgresRepository returns a PostgresRepository.
func NewPostgresRepository() *PostgresRepository { return &PostgresRepository{} }

// Load reads the dashboard aggregates. A dashboard is a handful of indexed,
// tenant-scoped aggregates — counts and sums, not a scan of rows — so each is one
// cheap query, not a loop.
func (r *PostgresRepository) Load(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (Snapshot, error) {
	var snap Snapshot
	snap.OutstandingFees = map[string]int64{}

	// The single-row counts, in one round trip.
	err := tx.QueryRow(ctx,
		`SELECT
		   (SELECT count(*) FROM students   WHERE tenant_id = $1 AND status = 'active'),
		   (SELECT count(*) FROM staff      WHERE tenant_id = $1 AND status = 'active'),
		   (SELECT count(*) FROM grade_levels WHERE tenant_id = $1),
		   (SELECT count(*) FROM subjects   WHERE tenant_id = $1),
		   (SELECT count(*) FROM notices    WHERE tenant_id = $1)`,
		tenantID).
		Scan(&snap.Students, &snap.Staff, &snap.Classes, &snap.Subjects, &snap.Notices)
	if err != nil {
		return Snapshot{}, fmt.Errorf("overview: counts: %w", err)
	}

	// Today's register, by status.
	rows, err := tx.Query(ctx,
		`SELECT status, count(*) FROM attendance
		 WHERE tenant_id = $1 AND on_date = CURRENT_DATE GROUP BY status`, tenantID)
	if err != nil {
		return Snapshot{}, fmt.Errorf("overview: attendance: %w", err)
	}
	if err := scanCounts(rows, func(status string, n int) {
		switch status {
		case "present":
			snap.AttendanceToday.Present = n
		case "absent":
			snap.AttendanceToday.Absent = n
		case "late":
			snap.AttendanceToday.Late = n
		case "excused":
			snap.AttendanceToday.Excused = n
		}
		snap.AttendanceToday.Total += n
	}); err != nil {
		return Snapshot{}, err
	}

	// Outstanding fees, summed by currency.
	feeRows, err := tx.Query(ctx,
		`SELECT currency, coalesce(sum(amount), 0) FROM fee_invoices
		 WHERE tenant_id = $1 AND status = 'unpaid' GROUP BY currency`, tenantID)
	if err != nil {
		return Snapshot{}, fmt.Errorf("overview: fees: %w", err)
	}
	defer feeRows.Close()
	for feeRows.Next() {
		var currency string
		var amount int64
		if err := feeRows.Scan(&currency, &amount); err != nil {
			return Snapshot{}, err
		}
		snap.OutstandingFees[currency] = amount
	}
	if err := feeRows.Err(); err != nil {
		return Snapshot{}, err
	}

	return snap, nil
}

// scanCounts walks a (label, count) result and hands each pair to fn.
func scanCounts(rows pgx.Rows, fn func(label string, n int)) error {
	defer rows.Close()
	for rows.Next() {
		var label string
		var n int
		if err := rows.Scan(&label, &n); err != nil {
			return err
		}
		fn(label, n)
	}
	return rows.Err()
}
