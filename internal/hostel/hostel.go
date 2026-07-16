// Package hostel models boarding management: the buildings an institution runs, the
// rooms inside them, and which student is allocated a bed. It knows nothing about
// HTTP, and references the academic spine (students) by id, never by import. A room's
// occupied count is recomputed in the transaction that allocates or vacates, so it
// can never disagree with the active allocations it summarises.
package hostel

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Sentinels. internal/httpapi maps each to a status; add a line to errors_test.go
// in the same commit as a new one.
var (
	ErrNotFound          = errors.New("hostel: not found")
	ErrRoomFull          = errors.New("hostel: the room is full")
	ErrAlreadyAllocated  = errors.New("hostel: the student already holds a room")
	ErrInvalidBuilding   = errors.New("hostel: the building is not valid")
	ErrInvalidRoom       = errors.New("hostel: the room is not valid")
	ErrInvalidAllocation = errors.New("hostel: the allocation is not valid")
	ErrInvalidPage       = errors.New("hostel: the page cursor is not valid")
)

// Allocation status.
const (
	StatusActive  = "active"
	StatusVacated = "vacated"
)

// Bounds.
const (
	MaxBuildings = 200
	MaxRooms     = 500
)

// Audit actions.
const (
	ActionBuildingCreated = "hostel_building.created"
	ActionRoomAdded       = "hostel_room.added"
	ActionAllocated       = "hostel_allocation.allocated"
	ActionVacated         = "hostel_allocation.vacated"
)

// Building is a boarding house the institution runs.
type Building struct {
	ID          uuid.UUID
	Name        string
	WardenName  *string
	WardenPhone *string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// NewBuilding is a building to create.
type NewBuilding struct {
	Name        string
	WardenName  *string
	WardenPhone *string
}

// Room is a room inside a building, with a bed capacity and a live occupancy count.
type Room struct {
	ID         uuid.UUID
	BuildingID uuid.UUID
	RoomNo     string
	Capacity   int
	Occupied   int
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// NewRoom is a room to add to a building.
type NewRoom struct {
	BuildingID uuid.UUID
	RoomNo     string
	Capacity   int
}

// Allocation is one student's bed in one room. It is active until vacated; the row
// survives vacating as boarding history.
type Allocation struct {
	ID          uuid.UUID
	RoomID      uuid.UUID
	StudentID   uuid.UUID
	AllocatedAt time.Time
	VacatedAt   *time.Time
	Status      string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// NewAllocation places a student in a room.
type NewAllocation struct {
	RoomID    uuid.UUID
	StudentID uuid.UUID
}

// AllocationFilter narrows an allocation listing.
type AllocationFilter struct {
	RoomID    *uuid.UUID
	StudentID *uuid.UUID
	Status    string
}

func trimPtr(p *string) *string {
	if p == nil {
		return nil
	}
	v := strings.TrimSpace(*p)
	if v == "" {
		return nil
	}
	return &v
}

func (n *NewBuilding) validate() error {
	n.Name = strings.TrimSpace(n.Name)
	if n.Name == "" {
		return fmt.Errorf("%w: name it", ErrInvalidBuilding)
	}
	n.WardenName = trimPtr(n.WardenName)
	n.WardenPhone = trimPtr(n.WardenPhone)
	return nil
}

func (n *NewRoom) validate() error {
	n.RoomNo = strings.TrimSpace(n.RoomNo)
	if n.RoomNo == "" {
		return fmt.Errorf("%w: give it a room number", ErrInvalidRoom)
	}
	if n.BuildingID == uuid.Nil {
		return fmt.Errorf("%w: name the building", ErrInvalidRoom)
	}
	if n.Capacity < 0 {
		return fmt.Errorf("%w: capacity cannot be negative", ErrInvalidRoom)
	}
	if n.Capacity == 0 {
		n.Capacity = 1
	}
	return nil
}

func (n *NewAllocation) validate() error {
	if n.RoomID == uuid.Nil {
		return fmt.Errorf("%w: name the room", ErrInvalidAllocation)
	}
	if n.StudentID == uuid.Nil {
		return fmt.Errorf("%w: name the student", ErrInvalidAllocation)
	}
	return nil
}
