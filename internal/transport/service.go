package transport

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/platform/database"
)

// Repository is the persistence contract, declared by its consumer. Every method
// takes a transaction, never the pool: the tenant is bound transaction-locally.
type Repository interface {
	CreateRoute(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewRoute) (Route, error)
	Routes(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, after *cursor, limit int) ([]Route, error)
	RouteByID(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) (Route, error)

	AddVehicle(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewVehicle) (Vehicle, error)
	Vehicles(ctx context.Context, tx pgx.Tx, tenantID, routeID uuid.UUID, limit int) ([]Vehicle, error)

	Assign(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, n NewAssignment) (Assignment, error)
	Assignments(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, f AssignmentFilter, after *cursor, limit int) ([]Assignment, error)
	Unassign(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) error
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

// Service holds the transport rules and owns transaction boundaries.
type Service struct {
	db    *database.DB
	repo  Repository
	audit AuditRecorder
}

// NewService returns a Service.
func NewService(db *database.DB, repo Repository, recorder AuditRecorder) *Service {
	return &Service{db: db, repo: repo, audit: recorder}
}

// CreateRoute defines a transport route with a fare.
func (s *Service) CreateRoute(ctx context.Context, tenantID uuid.UUID, n NewRoute, author Author) (Route, error) {
	if err := n.validate(); err != nil {
		return Route{}, err
	}
	var route Route
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		route, err = s.repo.CreateRoute(ctx, tx, tenantID, n)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionRouteCreated,
			TargetType: "transport_route", TargetID: route.ID.String(),
			Metadata: map[string]any{"name": route.Name, "fare": route.FareAmount, "currency": route.Currency},
		})
	})
	return route, err
}

// ListRoutes lists the workspace's routes, newest first, keyset-paginated.
func (s *Service) ListRoutes(ctx context.Context, tenantID uuid.UUID, p PageParams) (RoutePage, error) {
	after, err := p.decode()
	if err != nil {
		return RoutePage{}, err
	}
	limit := p.clamp()

	var page RoutePage
	err = s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := s.repo.Routes(ctx, tx, tenantID, after, limit+1)
		if err != nil {
			return err
		}
		if len(rows) > limit {
			page.HasMore = true
			page.NextCursor = encodeRouteCursor(rows[limit-1])
			rows = rows[:limit]
		}
		page.Routes = rows
		return nil
	})
	return page, err
}

// AddVehicle adds a vehicle to a route.
func (s *Service) AddVehicle(ctx context.Context, tenantID uuid.UUID, n NewVehicle, author Author) (Vehicle, error) {
	if err := n.validate(); err != nil {
		return Vehicle{}, err
	}
	var v Vehicle
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		v, err = s.repo.AddVehicle(ctx, tx, tenantID, n)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionVehicleAdded,
			TargetType: "transport_vehicle", TargetID: v.ID.String(),
			Metadata: map[string]any{"route_id": v.RouteID.String(), "registration_no": v.RegistrationNo},
		})
	})
	return v, err
}

// ListVehicles lists a route's fleet, newest first.
func (s *Service) ListVehicles(ctx context.Context, tenantID, routeID uuid.UUID) ([]Vehicle, error) {
	var out []Vehicle
	err := s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := s.repo.RouteByID(ctx, tx, tenantID, routeID); err != nil {
			return err
		}
		var err error
		out, err = s.repo.Vehicles(ctx, tx, tenantID, routeID, MaxVehicles)
		return err
	})
	return out, err
}

// AssignStudent places a student on a route. A student rides one route: a second
// placement conflicts and is refused with ErrAlreadyAssigned.
func (s *Service) AssignStudent(ctx context.Context, tenantID uuid.UUID, n NewAssignment, author Author) (Assignment, error) {
	if err := n.validate(); err != nil {
		return Assignment{}, err
	}
	var a Assignment
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		a, err = s.repo.Assign(ctx, tx, tenantID, n)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionStudentAssigned,
			TargetType: "transport_assignment", TargetID: a.ID.String(),
			Metadata: map[string]any{"route_id": a.RouteID.String(), "student_id": a.StudentID.String(), "stop": a.StopName},
		})
	})
	return a, err
}

// ListAssignments lists assignments, newest first, keyset-paginated, optionally
// narrowed to a route and/or a student.
func (s *Service) ListAssignments(ctx context.Context, tenantID uuid.UUID, f AssignmentFilter, p PageParams) (AssignmentPage, error) {
	after, err := p.decode()
	if err != nil {
		return AssignmentPage{}, err
	}
	limit := p.clamp()

	var page AssignmentPage
	err = s.db.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := s.repo.Assignments(ctx, tx, tenantID, f, after, limit+1)
		if err != nil {
			return err
		}
		if len(rows) > limit {
			page.HasMore = true
			page.NextCursor = encodeAssignmentCursor(rows[limit-1])
			rows = rows[:limit]
		}
		page.Assignments = rows
		return nil
	})
	return page, err
}

// Unassign takes a student off their route.
func (s *Service) Unassign(ctx context.Context, tenantID, id uuid.UUID, author Author) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := s.repo.Unassign(ctx, tx, tenantID, id); err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionStudentUnassigned,
			TargetType: "transport_assignment", TargetID: id.String(),
		})
	})
}
