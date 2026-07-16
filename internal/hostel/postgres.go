package hostel

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

const buildingColumns = `id, name, warden_name, warden_phone, created_at, updated_at`

func (r *PostgresRepository) CreateBuilding(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewBuilding) (Building, error) {
	var b Building
	err := tx.QueryRow(ctx,
		`INSERT INTO hostel_buildings (tenant_id, name, warden_name, warden_phone)
		 VALUES ($1, $2, $3, $4)
		 RETURNING `+buildingColumns,
		tenantID, n.Name, n.WardenName, n.WardenPhone).
		Scan(&b.ID, &b.Name, &b.WardenName, &b.WardenPhone, &b.CreatedAt, &b.UpdatedAt)
	if err != nil {
		return Building{}, fmt.Errorf("hostel: create building: %w", err)
	}
	return b, nil
}

const buildingsSQL = `
	SELECT ` + buildingColumns + ` FROM hostel_buildings
	WHERE tenant_id = $1
	  AND ($2::text IS NULL OR (name, id) > ($2, $3::uuid))
	ORDER BY name, id LIMIT $4`

func (r *PostgresRepository) Buildings(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, after *buildingCursor, limit int) ([]Building, error) {
	var afterName, afterID any
	if after != nil {
		afterName, afterID = after.Name, after.ID
	}
	rows, err := tx.Query(ctx, buildingsSQL, tenantID, afterName, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("hostel: buildings: %w", err)
	}
	defer rows.Close()
	return pgx.CollectRows(rows, scanBuilding)
}

const roomColumns = `id, building_id, room_no, capacity, occupied, created_at, updated_at`

func (r *PostgresRepository) AddRoom(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewRoom) (Room, error) {
	var rm Room
	err := tx.QueryRow(ctx,
		`INSERT INTO hostel_rooms (tenant_id, building_id, room_no, capacity)
		 VALUES ($1, $2, $3, $4)
		 RETURNING `+roomColumns,
		tenantID, n.BuildingID, n.RoomNo, n.Capacity).
		Scan(&rm.ID, &rm.BuildingID, &rm.RoomNo, &rm.Capacity, &rm.Occupied, &rm.CreatedAt, &rm.UpdatedAt)
	if err != nil {
		if isForeignKeyViolation(err) {
			return Room{}, ErrNotFound
		}
		return Room{}, fmt.Errorf("hostel: add room: %w", err)
	}
	return rm, nil
}

func (r *PostgresRepository) Rooms(ctx context.Context, tx pgx.Tx, tenantID, buildingID uuid.UUID, limit int) ([]Room, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+roomColumns+` FROM hostel_rooms
		 WHERE tenant_id = $1 AND building_id = $2 ORDER BY room_no, id LIMIT $3`,
		tenantID, buildingID, limit)
	if err != nil {
		return nil, fmt.Errorf("hostel: rooms: %w", err)
	}
	defer rows.Close()
	return pgx.CollectRows(rows, scanRoom)
}

const allocationColumns = `id, room_id, student_id, allocated_at, vacated_at, status, created_at, updated_at`

// Allocate inserts the active allocation, then claims a bed in the room. The insert
// carries the one-active-allocation guard (unique violation → already allocated) and
// the room/student foreign keys (missing → not found). The room claim increments
// occupied only WHERE a bed is free, so a full room updates nothing and the whole
// transaction rolls back with ErrRoomFull.
func (r *PostgresRepository) Allocate(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewAllocation) (Allocation, error) {
	var a Allocation
	err := tx.QueryRow(ctx,
		`INSERT INTO hostel_allocations (tenant_id, room_id, student_id)
		 VALUES ($1, $2, $3)
		 RETURNING `+allocationColumns,
		tenantID, n.RoomID, n.StudentID).
		Scan(&a.ID, &a.RoomID, &a.StudentID, &a.AllocatedAt, &a.VacatedAt, &a.Status, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return Allocation{}, ErrAlreadyAllocated
		}
		if isForeignKeyViolation(err) {
			return Allocation{}, ErrNotFound
		}
		return Allocation{}, fmt.Errorf("hostel: allocate: %w", err)
	}

	tag, err := tx.Exec(ctx,
		`UPDATE hostel_rooms SET occupied = occupied + 1, updated_at = now()
		 WHERE tenant_id = $1 AND id = $2 AND occupied < capacity`,
		tenantID, n.RoomID)
	if err != nil {
		return Allocation{}, fmt.Errorf("hostel: claim bed: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return Allocation{}, ErrRoomFull
	}
	return a, nil
}

// Vacate frees a student's bed. The `status = 'active'` guard makes a repeat harmless;
// a row that is already vacated is returned unchanged (idempotent, no double decrement),
// and an id that names nothing is ErrNotFound.
func (r *PostgresRepository) Vacate(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) (Allocation, error) {
	rows, err := tx.Query(ctx,
		`UPDATE hostel_allocations SET status = 'vacated', vacated_at = now(), updated_at = now()
		 WHERE tenant_id = $1 AND id = $2 AND status = 'active'
		 RETURNING `+allocationColumns,
		tenantID, id)
	if err != nil {
		return Allocation{}, fmt.Errorf("hostel: vacate: %w", err)
	}
	a, err := pgx.CollectExactlyOneRow(rows, scanAllocation)
	if errors.Is(err, pgx.ErrNoRows) {
		return r.absentOrVacated(ctx, tx, tenantID, id)
	}
	if err != nil {
		return Allocation{}, fmt.Errorf("hostel: vacate: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE hostel_rooms SET occupied = GREATEST(occupied - 1, 0), updated_at = now()
		 WHERE tenant_id = $1 AND id = $2`, tenantID, a.RoomID); err != nil {
		return Allocation{}, fmt.Errorf("hostel: release bed: %w", err)
	}
	return a, nil
}

// absentOrVacated distinguishes an allocation that does not exist from one already
// vacated: the first is ErrNotFound, the second is returned as-is so a repeat vacate
// is a no-op rather than an error.
func (r *PostgresRepository) absentOrVacated(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) (Allocation, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+allocationColumns+` FROM hostel_allocations WHERE tenant_id = $1 AND id = $2`,
		tenantID, id)
	if err != nil {
		return Allocation{}, fmt.Errorf("hostel: check allocation: %w", err)
	}
	a, err := pgx.CollectExactlyOneRow(rows, scanAllocation)
	if errors.Is(err, pgx.ErrNoRows) {
		return Allocation{}, ErrNotFound
	}
	if err != nil {
		return Allocation{}, fmt.Errorf("hostel: check allocation: %w", err)
	}
	return a, nil
}

// Two statements per anchor, not one with `OR $x IS NULL`: each filter shape gets the
// index that covers its filter and its sort, so neither leaves a Sort node.
const allocationsAllSQL = `
	SELECT ` + allocationColumns + ` FROM hostel_allocations
	WHERE tenant_id = $1
	  AND ($3::timestamptz IS NULL OR (allocated_at, id) < ($3, $4::uuid))
	  AND ($5::text = '' OR status = $5)
	ORDER BY allocated_at DESC, id DESC LIMIT $2`

const allocationsByRoomSQL = `
	SELECT ` + allocationColumns + ` FROM hostel_allocations
	WHERE tenant_id = $1 AND room_id = $6
	  AND ($7::uuid IS NULL OR student_id = $7)
	  AND ($3::timestamptz IS NULL OR (allocated_at, id) < ($3, $4::uuid))
	  AND ($5::text = '' OR status = $5)
	ORDER BY allocated_at DESC, id DESC LIMIT $2`

const allocationsByStudentSQL = `
	SELECT ` + allocationColumns + ` FROM hostel_allocations
	WHERE tenant_id = $1 AND student_id = $6
	  AND ($3::timestamptz IS NULL OR (allocated_at, id) < ($3, $4::uuid))
	  AND ($5::text = '' OR status = $5)
	ORDER BY allocated_at DESC, id DESC LIMIT $2`

func (r *PostgresRepository) Allocations(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, f AllocationFilter, after *cursor, limit int) ([]Allocation, error) {
	var afterTime, afterID any
	if after != nil {
		afterTime, afterID = after.AllocatedAt, after.ID
	}

	var rows pgx.Rows
	var err error
	switch {
	case f.RoomID != nil:
		var student any
		if f.StudentID != nil {
			student = *f.StudentID
		}
		rows, err = tx.Query(ctx, allocationsByRoomSQL, tenantID, limit, afterTime, afterID, f.Status, *f.RoomID, student)
	case f.StudentID != nil:
		rows, err = tx.Query(ctx, allocationsByStudentSQL, tenantID, limit, afterTime, afterID, f.Status, *f.StudentID)
	default:
		rows, err = tx.Query(ctx, allocationsAllSQL, tenantID, limit, afterTime, afterID, f.Status)
	}
	if err != nil {
		return nil, fmt.Errorf("hostel: allocations: %w", err)
	}
	defer rows.Close()
	return pgx.CollectRows(rows, scanAllocation)
}

func scanBuilding(row pgx.CollectableRow) (Building, error) {
	var b Building
	err := row.Scan(&b.ID, &b.Name, &b.WardenName, &b.WardenPhone, &b.CreatedAt, &b.UpdatedAt)
	return b, err
}

func scanRoom(row pgx.CollectableRow) (Room, error) {
	var rm Room
	err := row.Scan(&rm.ID, &rm.BuildingID, &rm.RoomNo, &rm.Capacity, &rm.Occupied, &rm.CreatedAt, &rm.UpdatedAt)
	return rm, err
}

func scanAllocation(row pgx.CollectableRow) (Allocation, error) {
	var a Allocation
	err := row.Scan(&a.ID, &a.RoomID, &a.StudentID, &a.AllocatedAt, &a.VacatedAt, &a.Status, &a.CreatedAt, &a.UpdatedAt)
	return a, err
}
