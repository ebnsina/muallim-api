package transport

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

func isForeignKeyViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23503"
}

const routeColumns = `id, name, description, fare_amount, currency, created_at, updated_at`

func (r *PostgresRepository) CreateRoute(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewRoute) (Route, error) {
	rows, err := tx.Query(ctx,
		`INSERT INTO transport_routes (tenant_id, name, description, fare_amount, currency)
		 VALUES ($1, $2, $3, $4, $5)
		 RETURNING `+routeColumns,
		tenantID, n.Name, nullIfEmpty(n.Description), n.FareAmount, n.Currency)
	if err != nil {
		return Route{}, fmt.Errorf("transport: create route: %w", err)
	}
	route, err := pgx.CollectExactlyOneRow(rows, scanRoute)
	if err != nil {
		return Route{}, fmt.Errorf("transport: create route: %w", err)
	}
	return route, nil
}

const routesSQL = `
	SELECT ` + routeColumns + ` FROM transport_routes
	WHERE tenant_id = $1
	  AND ($3::timestamptz IS NULL OR (created_at, id) < ($3, $4::uuid))
	ORDER BY created_at DESC, id DESC LIMIT $2`

func (r *PostgresRepository) Routes(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, after *cursor, limit int) ([]Route, error) {
	var afterTime, afterID any
	if after != nil {
		afterTime, afterID = after.CreatedAt, after.ID
	}
	rows, err := tx.Query(ctx, routesSQL, tenantID, limit, afterTime, afterID)
	if err != nil {
		return nil, fmt.Errorf("transport: routes: %w", err)
	}
	defer rows.Close()
	return pgx.CollectRows(rows, scanRoute)
}

func (r *PostgresRepository) RouteByID(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) (Route, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+routeColumns+` FROM transport_routes WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	if err != nil {
		return Route{}, fmt.Errorf("transport: load route: %w", err)
	}
	route, err := pgx.CollectExactlyOneRow(rows, scanRoute)
	if errors.Is(err, pgx.ErrNoRows) {
		return Route{}, ErrNotFound
	}
	return route, err
}

const vehicleColumns = `id, route_id, registration_no, capacity, driver_name, driver_phone, created_at, updated_at`

func (r *PostgresRepository) AddVehicle(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewVehicle) (Vehicle, error) {
	rows, err := tx.Query(ctx,
		`INSERT INTO transport_vehicles (tenant_id, route_id, registration_no, capacity, driver_name, driver_phone)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING `+vehicleColumns,
		tenantID, n.RouteID, n.RegistrationNo, n.Capacity, n.DriverName, nullIfEmpty(n.DriverPhone))
	if err != nil {
		if isForeignKeyViolation(err) {
			return Vehicle{}, ErrNotFound
		}
		return Vehicle{}, fmt.Errorf("transport: add vehicle: %w", err)
	}
	v, err := pgx.CollectExactlyOneRow(rows, scanVehicle)
	if err != nil {
		return Vehicle{}, fmt.Errorf("transport: add vehicle: %w", err)
	}
	return v, nil
}

func (r *PostgresRepository) Vehicles(ctx context.Context, tx pgx.Tx, tenantID, routeID uuid.UUID, limit int) ([]Vehicle, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+vehicleColumns+` FROM transport_vehicles
		 WHERE tenant_id = $1 AND route_id = $2 ORDER BY created_at DESC, id DESC LIMIT $3`,
		tenantID, routeID, limit)
	if err != nil {
		return nil, fmt.Errorf("transport: vehicles: %w", err)
	}
	defer rows.Close()
	return pgx.CollectRows(rows, scanVehicle)
}

const assignmentColumns = `id, route_id, student_id, stop_name, assigned_at, created_at, updated_at`

// Assign places a student on a route. The unique index on (tenant, student) makes
// a second placement a no-op that returns nothing — reported as ErrAlreadyAssigned.
func (r *PostgresRepository) Assign(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewAssignment) (Assignment, error) {
	rows, err := tx.Query(ctx,
		`INSERT INTO transport_assignments (tenant_id, route_id, student_id, stop_name)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (tenant_id, student_id) DO NOTHING
		 RETURNING `+assignmentColumns,
		tenantID, n.RouteID, n.StudentID, n.StopName)
	if err != nil {
		if isForeignKeyViolation(err) {
			return Assignment{}, ErrNotFound
		}
		return Assignment{}, fmt.Errorf("transport: assign: %w", err)
	}
	a, err := pgx.CollectExactlyOneRow(rows, scanAssignment)
	// tx.Query is lazy, so a bad route/student FK surfaces here, not above.
	if errors.Is(err, pgx.ErrNoRows) {
		return Assignment{}, ErrAlreadyAssigned
	}
	if isForeignKeyViolation(err) {
		return Assignment{}, ErrNotFound
	}
	if err != nil {
		return Assignment{}, fmt.Errorf("transport: assign: %w", err)
	}
	return a, nil
}

// Two statements, not one with an `OR $x IS NULL` on route: each filter shape gets
// the index that covers it and neither leaves a Sort node on the request path. The
// optional student narrows either shape.
const assignmentsAllSQL = `
	SELECT ` + assignmentColumns + ` FROM transport_assignments
	WHERE tenant_id = $1
	  AND ($3::timestamptz IS NULL OR (created_at, id) < ($3, $4::uuid))
	  AND ($5::uuid IS NULL OR student_id = $5)
	ORDER BY created_at DESC, id DESC LIMIT $2`

const assignmentsByRouteSQL = `
	SELECT ` + assignmentColumns + ` FROM transport_assignments
	WHERE tenant_id = $1 AND route_id = $6
	  AND ($3::timestamptz IS NULL OR (created_at, id) < ($3, $4::uuid))
	  AND ($5::uuid IS NULL OR student_id = $5)
	ORDER BY created_at DESC, id DESC LIMIT $2`

func (r *PostgresRepository) Assignments(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, f AssignmentFilter, after *cursor, limit int) ([]Assignment, error) {
	var afterTime, afterID any
	if after != nil {
		afterTime, afterID = after.CreatedAt, after.ID
	}
	var studentArg any
	if f.StudentID != nil {
		studentArg = *f.StudentID
	}

	var rows pgx.Rows
	var err error
	if f.RouteID != nil {
		rows, err = tx.Query(ctx, assignmentsByRouteSQL, tenantID, limit, afterTime, afterID, studentArg, *f.RouteID)
	} else {
		rows, err = tx.Query(ctx, assignmentsAllSQL, tenantID, limit, afterTime, afterID, studentArg)
	}
	if err != nil {
		return nil, fmt.Errorf("transport: assignments: %w", err)
	}
	defer rows.Close()
	return pgx.CollectRows(rows, scanAssignment)
}

func (r *PostgresRepository) Unassign(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) error {
	tag, err := tx.Exec(ctx, `DELETE FROM transport_assignments WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	if err != nil {
		return fmt.Errorf("transport: unassign: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func scanRoute(row pgx.CollectableRow) (Route, error) {
	var r Route
	var desc *string
	err := row.Scan(&r.ID, &r.Name, &desc, &r.FareAmount, &r.Currency, &r.CreatedAt, &r.UpdatedAt)
	if desc != nil {
		r.Description = *desc
	}
	return r, err
}

func scanVehicle(row pgx.CollectableRow) (Vehicle, error) {
	var v Vehicle
	var phone *string
	err := row.Scan(&v.ID, &v.RouteID, &v.RegistrationNo, &v.Capacity, &v.DriverName, &phone, &v.CreatedAt, &v.UpdatedAt)
	if phone != nil {
		v.DriverPhone = *phone
	}
	return v, err
}

func scanAssignment(row pgx.CollectableRow) (Assignment, error) {
	var a Assignment
	err := row.Scan(&a.ID, &a.RouteID, &a.StudentID, &a.StopName, &a.AssignedAt, &a.CreatedAt, &a.UpdatedAt)
	return a, err
}
