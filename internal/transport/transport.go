// Package transport models school transport: routes an institution runs, the
// vehicles on them, and the students assigned to a route. It knows nothing about
// HTTP, and references the academic spine (students) by id, never by import. Money
// is bigint minor units + a currency, defaulting to BDT poisha — never a float.
package transport

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Sentinels. internal/httpapi maps each to a status; add a line to
// transport_errors_test.go in the same commit as a new one.
var (
	ErrNotFound          = errors.New("transport: not found")
	ErrInvalidRoute      = errors.New("transport: the route is not valid")
	ErrInvalidVehicle    = errors.New("transport: the vehicle is not valid")
	ErrAlreadyAssigned   = errors.New("transport: the student already rides a route")
	ErrInvalidAssignment = errors.New("transport: the assignment is not valid")
	ErrInvalidPage       = errors.New("transport: the page cursor is not valid")
)

// DefaultCurrency is BDT — the primary market. An international workspace passes
// its own; the column is char(3) either way.
const DefaultCurrency = "BDT"

// Bounds.
const MaxVehicles = 500

// Audit actions.
const (
	ActionRouteCreated      = "transport_route.created"
	ActionVehicleAdded      = "transport_vehicle.added"
	ActionStudentAssigned   = "transport_assignment.assigned"
	ActionStudentUnassigned = "transport_assignment.unassigned"
)

// Route is a line the institution runs, with a fare.
type Route struct {
	ID          uuid.UUID
	Name        string
	Description string
	FareAmount  int64
	Currency    string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// NewRoute is a route to create.
type NewRoute struct {
	Name        string
	Description string
	FareAmount  int64
	Currency    string
}

// Vehicle is a bus on a route.
type Vehicle struct {
	ID             uuid.UUID
	RouteID        uuid.UUID
	RegistrationNo string
	Capacity       int
	DriverName     string
	DriverPhone    string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// NewVehicle is a vehicle to add to a route.
type NewVehicle struct {
	RouteID        uuid.UUID
	RegistrationNo string
	Capacity       int
	DriverName     string
	DriverPhone    string
}

// Assignment puts one student on one route at a stop.
type Assignment struct {
	ID         uuid.UUID
	RouteID    uuid.UUID
	StudentID  uuid.UUID
	StopName   string
	AssignedAt time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// NewAssignment places a student on a route.
type NewAssignment struct {
	RouteID   uuid.UUID
	StudentID uuid.UUID
	StopName  string
}

// AssignmentFilter narrows a listing to a route and/or a student.
type AssignmentFilter struct {
	RouteID   *uuid.UUID
	StudentID *uuid.UUID
}

func normaliseCurrency(c string) string {
	c = strings.ToUpper(strings.TrimSpace(c))
	if c == "" {
		return DefaultCurrency
	}
	return c
}

func (n *NewRoute) validate() error {
	if strings.TrimSpace(n.Name) == "" {
		return fmt.Errorf("%w: name it", ErrInvalidRoute)
	}
	if n.FareAmount < 0 {
		return fmt.Errorf("%w: the fare cannot be negative", ErrInvalidRoute)
	}
	n.Currency = normaliseCurrency(n.Currency)
	if len(n.Currency) != 3 {
		return fmt.Errorf("%w: a currency is three letters", ErrInvalidRoute)
	}
	return nil
}

func (n *NewVehicle) validate() error {
	if n.RouteID == uuid.Nil {
		return fmt.Errorf("%w: name its route", ErrInvalidVehicle)
	}
	if strings.TrimSpace(n.RegistrationNo) == "" {
		return fmt.Errorf("%w: give it a registration number", ErrInvalidVehicle)
	}
	if n.Capacity < 0 {
		return fmt.Errorf("%w: the capacity cannot be negative", ErrInvalidVehicle)
	}
	return nil
}

func (n *NewAssignment) validate() error {
	if n.RouteID == uuid.Nil {
		return fmt.Errorf("%w: name the route", ErrInvalidAssignment)
	}
	if n.StudentID == uuid.Nil {
		return fmt.Errorf("%w: name the student", ErrInvalidAssignment)
	}
	return nil
}
