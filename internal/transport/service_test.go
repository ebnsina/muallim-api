package transport_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/platform/database"
	"github.com/ebnsina/muallim-api/internal/transport"
)

func testDB(t *testing.T) *database.DB {
	t.Helper()
	url := os.Getenv("MUALLIM_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("MUALLIM_TEST_DATABASE_URL is not set")
	}
	db, err := database.New(t.Context(), database.Options{URL: url, MaxConns: 4},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

func seedTenant(t *testing.T, db *database.DB) uuid.UUID {
	t.Helper()
	id := uuid.New()
	sub := "t" + id.String()[:8]
	err := db.WithoutTenant(t.Context(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, subdomain, name) VALUES ($1, $2, $3)`, id, sub, "Test "+sub)
		return err
	})
	if err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	t.Cleanup(func() {
		_ = db.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM tenants WHERE id = $1`, id)
			return err
		})
	})
	return id
}

func seedStudent(t *testing.T, db *database.DB, tenant uuid.UUID, admissionNo, name string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := db.WithTenant(t.Context(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO students (tenant_id, admission_no, full_name, status)
			 VALUES ($1, $2, $3, 'active') RETURNING id`, tenant, admissionNo, name).Scan(&id)
	})
	if err != nil {
		t.Fatalf("seed student: %v", err)
	}
	return id
}

type stubAuditor struct{}

func (stubAuditor) Record(context.Context, pgx.Tx, uuid.UUID, transport.AuditEntry) error { return nil }

func newService(db *database.DB) *transport.Service {
	return transport.NewService(db, transport.NewPostgresRepository(), stubAuditor{})
}

func TestTransportFlow(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)
	author := transport.Author{UserID: uuid.New()}

	amina := seedStudent(t, db, tenant, "2025-001", "Amina Rahman")
	bilal := seedStudent(t, db, tenant, "2025-002", "Bilal Ahmed")

	// A route defaults to BDT poisha.
	route, err := svc.CreateRoute(t.Context(), tenant, transport.NewRoute{
		Name: "Uttara Line", Description: "Sector 7 to campus", FareAmount: 150000,
	}, author)
	if err != nil {
		t.Fatalf("create route: %v", err)
	}
	if route.Currency != "BDT" {
		t.Fatalf("currency %q, want the BDT default", route.Currency)
	}

	// The route appears in the workspace listing.
	routes, err := svc.ListRoutes(t.Context(), tenant, transport.PageParams{})
	if err != nil {
		t.Fatalf("list routes: %v", err)
	}
	if len(routes.Routes) != 1 {
		t.Fatalf("listed %d routes, want 1", len(routes.Routes))
	}

	// A vehicle joins the route's fleet.
	v, err := svc.AddVehicle(t.Context(), tenant, transport.NewVehicle{
		RouteID: route.ID, RegistrationNo: "DHAKA-METRO-GA-11-2233", Capacity: 40,
		DriverName: "Karim", DriverPhone: "+8801700000000",
	}, author)
	if err != nil {
		t.Fatalf("add vehicle: %v", err)
	}
	if v.RouteID != route.ID {
		t.Fatalf("vehicle route %v, want %v", v.RouteID, route.ID)
	}
	vehicles, err := svc.ListVehicles(t.Context(), tenant, route.ID)
	if err != nil {
		t.Fatalf("list vehicles: %v", err)
	}
	if len(vehicles) != 1 {
		t.Fatalf("listed %d vehicles, want 1", len(vehicles))
	}

	// Assigning Amina to the route places her at a stop.
	a, err := svc.AssignStudent(t.Context(), tenant, transport.NewAssignment{
		RouteID: route.ID, StudentID: amina, StopName: "Sector 7 roundabout",
	}, author)
	if err != nil {
		t.Fatalf("assign: %v", err)
	}

	// A student rides one route: a second placement is refused.
	if _, err := svc.AssignStudent(t.Context(), tenant, transport.NewAssignment{
		RouteID: route.ID, StudentID: amina, StopName: "Elsewhere",
	}, author); !errors.Is(err, transport.ErrAlreadyAssigned) {
		t.Fatalf("a duplicate assignment was accepted: %v", err)
	}

	// Bilal rides too; the workspace listing shows both.
	if _, err := svc.AssignStudent(t.Context(), tenant, transport.NewAssignment{
		RouteID: route.ID, StudentID: bilal,
	}, author); err != nil {
		t.Fatalf("assign bilal: %v", err)
	}
	all, err := svc.ListAssignments(t.Context(), tenant, transport.AssignmentFilter{}, transport.PageParams{})
	if err != nil {
		t.Fatalf("list assignments: %v", err)
	}
	if len(all.Assignments) != 2 {
		t.Fatalf("listed %d assignments, want 2", len(all.Assignments))
	}

	// Filtering by route returns the same two; by student, exactly one.
	byRoute, err := svc.ListAssignments(t.Context(), tenant, transport.AssignmentFilter{RouteID: &route.ID}, transport.PageParams{})
	if err != nil {
		t.Fatalf("list by route: %v", err)
	}
	if len(byRoute.Assignments) != 2 {
		t.Fatalf("route listed %d, want 2", len(byRoute.Assignments))
	}
	byStudent, err := svc.ListAssignments(t.Context(), tenant, transport.AssignmentFilter{StudentID: &amina}, transport.PageParams{})
	if err != nil {
		t.Fatalf("list by student: %v", err)
	}
	if len(byStudent.Assignments) != 1 {
		t.Fatalf("student listed %d, want 1", len(byStudent.Assignments))
	}

	// Unassigning Amina frees her to be placed again.
	if err := svc.Unassign(t.Context(), tenant, a.ID, author); err != nil {
		t.Fatalf("unassign: %v", err)
	}
	if _, err := svc.AssignStudent(t.Context(), tenant, transport.NewAssignment{
		RouteID: route.ID, StudentID: amina,
	}, author); err != nil {
		t.Fatalf("re-assign after unassign: %v", err)
	}

	// Unassigning something absent is a not-found, not a silent success.
	if err := svc.Unassign(t.Context(), tenant, uuid.New(), author); !errors.Is(err, transport.ErrNotFound) {
		t.Fatalf("unassign of a missing row: %v", err)
	}
}

func TestAssignUnknownStudentIsNotFound(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)
	author := transport.Author{UserID: uuid.New()}

	route, err := svc.CreateRoute(t.Context(), tenant, transport.NewRoute{Name: "Mirpur Line"}, author)
	if err != nil {
		t.Fatalf("create route: %v", err)
	}

	// A student nobody holds cannot be assigned — the FK refuses it.
	if _, err := svc.AssignStudent(t.Context(), tenant, transport.NewAssignment{
		RouteID: route.ID, StudentID: uuid.New(),
	}, author); !errors.Is(err, transport.ErrNotFound) {
		t.Fatalf("assigning an unknown student: %v", err)
	}
}
