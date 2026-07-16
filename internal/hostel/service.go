package hostel

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/platform/database"
)

// Repository is the persistence contract, declared by its consumer. Every method
// takes a transaction, never the pool: the tenant is bound transaction-locally.
type Repository interface {
	CreateBuilding(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewBuilding) (Building, error)
	Buildings(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, after *buildingCursor, limit int) ([]Building, error)

	AddRoom(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewRoom) (Room, error)
	Rooms(ctx context.Context, tx pgx.Tx, tenantID, buildingID uuid.UUID, limit int) ([]Room, error)

	Allocate(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewAllocation) (Allocation, error)
	Vacate(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) (Allocation, error)
	Allocations(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, f AllocationFilter, after *cursor, limit int) ([]Allocation, error)
}

// AuditRecorder writes an audit line in the transaction of the thing it describes.
type AuditRecorder interface {
	Record(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, e AuditEntry) error
}

// AuditEntry is one line of the audit log.
type AuditEntry struct {
	ActorID    *uuid.UUID
	Action     string
	TargetType string
	TargetID   string
	Metadata   map[string]any
}

// Author identifies who did something, for the audit trail.
type Author struct {
	UserID uuid.UUID
}

// Service holds the boarding rules and owns transaction boundaries.
type Service struct {
	db    *database.DB
	repo  Repository
	audit AuditRecorder
}

// NewService returns a Service.
func NewService(db *database.DB, repo Repository, recorder AuditRecorder) *Service {
	return &Service{db: db, repo: repo, audit: recorder}
}

// CreateBuilding registers a boarding house.
func (s *Service) CreateBuilding(ctx context.Context, tenantID uuid.UUID, n NewBuilding, author Author) (Building, error) {
	if err := n.validate(); err != nil {
		return Building{}, err
	}
	var b Building
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		b, err = s.repo.CreateBuilding(ctx, tx, tenantID, n)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionBuildingCreated,
			TargetType: "hostel_building", TargetID: b.ID.String(),
			Metadata: map[string]any{"name": b.Name},
		})
	})
	return b, err
}

// ListBuildings lists the workspace's buildings by name, keyset-paginated.
func (s *Service) ListBuildings(ctx context.Context, tenantID uuid.UUID, p PageParams) (BuildingPage, error) {
	after, err := p.decodeBuilding()
	if err != nil {
		return BuildingPage{}, err
	}
	limit := p.clamp()

	var page BuildingPage
	err = s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := s.repo.Buildings(ctx, tx, tenantID, after, limit+1)
		if err != nil {
			return err
		}
		if len(rows) > limit {
			page.HasMore = true
			page.NextCursor = encodeBuildingCursor(rows[limit-1])
			rows = rows[:limit]
		}
		page.Buildings = rows
		return nil
	})
	return page, err
}

// AddRoom adds a room to a building.
func (s *Service) AddRoom(ctx context.Context, tenantID uuid.UUID, n NewRoom, author Author) (Room, error) {
	if err := n.validate(); err != nil {
		return Room{}, err
	}
	var r Room
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		r, err = s.repo.AddRoom(ctx, tx, tenantID, n)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionRoomAdded,
			TargetType: "hostel_room", TargetID: r.ID.String(),
			Metadata: map[string]any{"building_id": r.BuildingID.String(), "room_no": r.RoomNo, "capacity": r.Capacity},
		})
	})
	return r, err
}

// ListRooms lists a building's rooms by room number, bounded.
func (s *Service) ListRooms(ctx context.Context, tenantID, buildingID uuid.UUID) ([]Room, error) {
	var out []Room
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = s.repo.Rooms(ctx, tx, tenantID, buildingID, MaxRooms)
		return err
	})
	return out, err
}

// AllocateRoom places a student in a room. The capacity guard and the occupancy
// increment are one statement: the row updates only WHERE occupied < capacity, so a
// full room takes nobody and ErrRoomFull is the answer. A student already boarded
// elsewhere is refused by the one-active-allocation index as ErrAlreadyAllocated.
func (s *Service) AllocateRoom(ctx context.Context, tenantID uuid.UUID, n NewAllocation, author Author) (Allocation, error) {
	if err := n.validate(); err != nil {
		return Allocation{}, err
	}
	var a Allocation
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		a, err = s.repo.Allocate(ctx, tx, tenantID, n)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionAllocated,
			TargetType: "hostel_allocation", TargetID: a.ID.String(),
			Metadata: map[string]any{"room_id": a.RoomID.String(), "student_id": a.StudentID.String()},
		})
	})
	return a, err
}

// VacateRoom frees a student's bed. The `WHERE status = 'active'` guard makes a
// double submission harmless — the second finds nothing to vacate — and the room's
// occupancy is decremented in the same transaction so the count stays true.
func (s *Service) VacateRoom(ctx context.Context, tenantID, id uuid.UUID, author Author) (Allocation, error) {
	var a Allocation
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		a, err = s.repo.Vacate(ctx, tx, tenantID, id)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionVacated,
			TargetType: "hostel_allocation", TargetID: a.ID.String(),
			Metadata: map[string]any{"room_id": a.RoomID.String(), "student_id": a.StudentID.String()},
		})
	})
	return a, err
}

// ListAllocations lists allocations newest first, keyset-paginated, filtered by
// room, student and/or status.
func (s *Service) ListAllocations(ctx context.Context, tenantID uuid.UUID, f AllocationFilter, p PageParams) (AllocationPage, error) {
	after, err := p.decode()
	if err != nil {
		return AllocationPage{}, err
	}
	limit := p.clamp()

	var page AllocationPage
	err = s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := s.repo.Allocations(ctx, tx, tenantID, f, after, limit+1)
		if err != nil {
			return err
		}
		if len(rows) > limit {
			page.HasMore = true
			page.NextCursor = encodeCursor(rows[limit-1])
			rows = rows[:limit]
		}
		page.Allocations = rows
		return nil
	})
	return page, err
}
