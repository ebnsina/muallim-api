package hostel_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/hostel"
	"github.com/ebnsina/muallim-api/internal/platform/database"
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
	sub := "h" + id.String()[:8]
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

func seedClass(t *testing.T, db *database.DB, tenant uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := db.WithTenant(t.Context(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO grade_levels (tenant_id, name, rank) VALUES ($1, 'Class 6', 6) RETURNING id`, tenant).Scan(&id)
	})
	if err != nil {
		t.Fatalf("seed class: %v", err)
	}
	return id
}

func seedStudent(t *testing.T, db *database.DB, tenant, class uuid.UUID, admissionNo, name string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := db.WithTenant(t.Context(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO students (tenant_id, admission_no, full_name, grade_level_id, status)
			 VALUES ($1, $2, $3, $4, 'active') RETURNING id`, tenant, admissionNo, name, class).Scan(&id)
	})
	if err != nil {
		t.Fatalf("seed student: %v", err)
	}
	return id
}

type stubAuditor struct{}

func (stubAuditor) Record(context.Context, pgx.Tx, uuid.UUID, hostel.AuditEntry) error { return nil }

func newService(db *database.DB) *hostel.Service {
	return hostel.NewService(db, hostel.NewPostgresRepository(), stubAuditor{})
}

// occupancy reads a room's live occupied count so the roll-up can be asserted against
// the allocations that produced it.
func occupancy(t *testing.T, svc *hostel.Service, tenant, building, roomID uuid.UUID) int {
	t.Helper()
	rooms, err := svc.ListRooms(t.Context(), tenant, building)
	if err != nil {
		t.Fatalf("list rooms: %v", err)
	}
	for _, r := range rooms {
		if r.ID == roomID {
			return r.Occupied
		}
	}
	t.Fatalf("room %s not found in building", roomID)
	return -1
}

func TestBoardingFlow(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)
	author := hostel.Author{UserID: uuid.New()}

	class := seedClass(t, db, tenant)
	amina := seedStudent(t, db, tenant, class, "2025-001", "Amina Rahman")
	bilal := seedStudent(t, db, tenant, class, "2025-002", "Bilal Ahmed")

	// A single-bed room in a boarding house.
	building, err := svc.CreateBuilding(t.Context(), tenant, hostel.NewBuilding{Name: "North Hall"}, author)
	if err != nil {
		t.Fatalf("create building: %v", err)
	}
	room, err := svc.AddRoom(t.Context(), tenant, hostel.NewRoom{
		BuildingID: building.ID, RoomNo: "101", Capacity: 1,
	}, author)
	if err != nil {
		t.Fatalf("add room: %v", err)
	}

	// Allocating Amina fills the room's one bed.
	alloc, err := svc.AllocateRoom(t.Context(), tenant, hostel.NewAllocation{RoomID: room.ID, StudentID: amina}, author)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if alloc.Status != hostel.StatusActive {
		t.Fatalf("status %q, want active", alloc.Status)
	}
	if got := occupancy(t, svc, tenant, building.ID, room.ID); got != 1 {
		t.Fatalf("occupied %d, want 1 after one allocation", got)
	}

	// A second student cannot take a bed that does not exist: the room is full.
	if _, err := svc.AllocateRoom(t.Context(), tenant, hostel.NewAllocation{RoomID: room.ID, StudentID: bilal}, author); !errors.Is(err, hostel.ErrRoomFull) {
		t.Fatalf("a full room accepted a second student: %v", err)
	}
	// And the failed allocation left the count untouched.
	if got := occupancy(t, svc, tenant, building.ID, room.ID); got != 1 {
		t.Fatalf("occupied %d after a refused allocation, want 1", got)
	}

	// Amina cannot hold a second bed while she already holds one, even in a room with
	// space to spare.
	roomy, err := svc.AddRoom(t.Context(), tenant, hostel.NewRoom{
		BuildingID: building.ID, RoomNo: "102", Capacity: 4,
	}, author)
	if err != nil {
		t.Fatalf("add room: %v", err)
	}
	if _, err := svc.AllocateRoom(t.Context(), tenant, hostel.NewAllocation{RoomID: roomy.ID, StudentID: amina}, author); !errors.Is(err, hostel.ErrAlreadyAllocated) {
		t.Fatalf("a student was boarded twice: %v", err)
	}

	// Vacating Amina frees the bed and records the history.
	vacated, err := svc.VacateRoom(t.Context(), tenant, alloc.ID, author)
	if err != nil {
		t.Fatalf("vacate: %v", err)
	}
	if vacated.Status != hostel.StatusVacated || vacated.VacatedAt == nil {
		t.Fatalf("vacate left %+v, want a vacated row with a timestamp", vacated)
	}
	if got := occupancy(t, svc, tenant, building.ID, room.ID); got != 0 {
		t.Fatalf("occupied %d after vacate, want 0", got)
	}

	// With the bed free and Amina's prior allocation now history, she can be boarded
	// again.
	if _, err := svc.AllocateRoom(t.Context(), tenant, hostel.NewAllocation{RoomID: room.ID, StudentID: amina}, author); err != nil {
		t.Fatalf("re-allocate after vacate: %v", err)
	}
	if got := occupancy(t, svc, tenant, building.ID, room.ID); got != 1 {
		t.Fatalf("occupied %d after re-allocation, want 1", got)
	}
}

func TestAllocationListingFiltersAndPaginates(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)
	author := hostel.Author{UserID: uuid.New()}
	class := seedClass(t, db, tenant)

	building, err := svc.CreateBuilding(t.Context(), tenant, hostel.NewBuilding{Name: "West Hall"}, author)
	if err != nil {
		t.Fatalf("create building: %v", err)
	}
	room, err := svc.AddRoom(t.Context(), tenant, hostel.NewRoom{BuildingID: building.ID, RoomNo: "201", Capacity: 10}, author)
	if err != nil {
		t.Fatalf("add room: %v", err)
	}

	for i := 0; i < 3; i++ {
		s := seedStudent(t, db, tenant, class, "L-"+uuid.NewString()[:8], "Boarder")
		if _, err := svc.AllocateRoom(t.Context(), tenant, hostel.NewAllocation{RoomID: room.ID, StudentID: s}, author); err != nil {
			t.Fatalf("allocate: %v", err)
		}
	}

	all, err := svc.ListAllocations(t.Context(), tenant, hostel.AllocationFilter{}, hostel.PageParams{})
	if err != nil {
		t.Fatalf("list allocations: %v", err)
	}
	if len(all.Allocations) != 3 {
		t.Fatalf("listed %d allocations, want 3", len(all.Allocations))
	}

	byRoom, err := svc.ListAllocations(t.Context(), tenant, hostel.AllocationFilter{RoomID: &room.ID, Status: hostel.StatusActive}, hostel.PageParams{})
	if err != nil {
		t.Fatalf("list by room: %v", err)
	}
	if len(byRoom.Allocations) != 3 {
		t.Fatalf("listed %d for the room, want 3", len(byRoom.Allocations))
	}

	// A one-row page hands back a cursor that resumes cleanly.
	first, err := svc.ListAllocations(t.Context(), tenant, hostel.AllocationFilter{}, hostel.PageParams{Limit: 1})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if !first.HasMore || first.NextCursor == "" {
		t.Fatalf("a bounded first page did not signal more: %+v", first)
	}
	next, err := svc.ListAllocations(t.Context(), tenant, hostel.AllocationFilter{}, hostel.PageParams{Limit: 5, Cursor: first.NextCursor})
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if len(next.Allocations) != 2 {
		t.Fatalf("second page had %d, want the remaining 2", len(next.Allocations))
	}

	// A malformed cursor is a validation error, never a 500.
	if _, err := svc.ListAllocations(t.Context(), tenant, hostel.AllocationFilter{}, hostel.PageParams{Cursor: "!!not-base64!!"}); !errors.Is(err, hostel.ErrInvalidPage) {
		t.Fatalf("a bad cursor was accepted: %v", err)
	}
}

func TestVacateUnknownAllocationIsNotFound(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	tenant := seedTenant(t, db)
	svc := newService(db)
	author := hostel.Author{UserID: uuid.New()}

	if _, err := svc.VacateRoom(t.Context(), tenant, uuid.New(), author); !errors.Is(err, hostel.ErrNotFound) {
		t.Fatalf("vacating a phantom allocation: %v", err)
	}
}
