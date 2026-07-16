package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ebnsina/muallim-api/internal/auth"
	"github.com/ebnsina/muallim-api/internal/transport"
)

// RouteView is a transport line the school runs. FareAmount is minor units (BDT
// poisha by default); the client formats it with the currency.
type RouteView struct {
	ID          string `json:"id" format:"uuid"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	FareAmount  int64  `json:"fare_amount"`
	Currency    string `json:"currency"`
}

// VehicleView is one bus on a route.
type VehicleView struct {
	ID             string `json:"id" format:"uuid"`
	RouteID        string `json:"route_id" format:"uuid"`
	RegistrationNo string `json:"registration_no"`
	Capacity       int    `json:"capacity"`
	DriverName     string `json:"driver_name,omitempty"`
	DriverPhone    string `json:"driver_phone,omitempty"`
}

// TransportAssignmentView puts one student on one route at a stop.
type TransportAssignmentView struct {
	ID         string `json:"id" format:"uuid"`
	RouteID    string `json:"route_id" format:"uuid"`
	StudentID  string `json:"student_id" format:"uuid"`
	StopName   string `json:"stop_name,omitempty"`
	AssignedAt string `json:"assigned_at,omitempty" format:"date-time"`
}

func routeView(r transport.Route) RouteView {
	return RouteView{
		ID: r.ID.String(), Name: r.Name, Description: r.Description,
		FareAmount: r.FareAmount, Currency: r.Currency,
	}
}

func vehicleView(v transport.Vehicle) VehicleView {
	return VehicleView{
		ID: v.ID.String(), RouteID: v.RouteID.String(), RegistrationNo: v.RegistrationNo,
		Capacity: v.Capacity, DriverName: v.DriverName, DriverPhone: v.DriverPhone,
	}
}

func transportAssignmentView(a transport.Assignment) TransportAssignmentView {
	v := TransportAssignmentView{
		ID: a.ID.String(), RouteID: a.RouteID.String(), StudentID: a.StudentID.String(),
		StopName: a.StopName,
	}
	if !a.AssignedAt.IsZero() {
		v.AssignedAt = a.AssignedAt.Format(time.RFC3339)
	}
	return v
}

func registerTransport(api huma.API, svc *transport.Service) {
	admin := []map[string][]string{{"bearer": {}}}

	huma.Register(api, huma.Operation{
		OperationID: "list-transport-routes",
		Method:      http.MethodGet,
		Path:        "/v1/transport/routes",
		Summary:     "The workspace's transport routes, newest first",
		Description: "Keyset-paginated.",
		Tags:        []string{"Transport"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		Limit  int    `query:"limit" minimum:"1" maximum:"100" default:"50"`
		Cursor string `query:"cursor"`
	}) (*struct {
		Body struct {
			Routes     []RouteView `json:"routes"`
			NextCursor string      `json:"next_cursor,omitempty"`
			HasMore    bool        `json:"has_more"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		page, err := svc.ListRoutes(ctx, p.TenantID, transport.PageParams{Limit: in.Limit, Cursor: in.Cursor})
		if err != nil {
			return nil, transportError(err)
		}
		out := &struct {
			Body struct {
				Routes     []RouteView `json:"routes"`
				NextCursor string      `json:"next_cursor,omitempty"`
				HasMore    bool        `json:"has_more"`
			}
		}{}
		out.Body.Routes = make([]RouteView, 0, len(page.Routes))
		for _, r := range page.Routes {
			out.Body.Routes = append(out.Body.Routes, routeView(r))
		}
		out.Body.NextCursor = page.NextCursor
		out.Body.HasMore = page.HasMore
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "create-transport-route",
		Method:        http.MethodPost,
		Path:          "/v1/transport/routes",
		Summary:       "Define a transport route",
		DefaultStatus: http.StatusCreated,
		Tags:          []string{"Transport"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		Body struct {
			Name        string `json:"name" minLength:"1" maxLength:"160"`
			Description string `json:"description,omitempty" maxLength:"500"`
			FareAmount  int64  `json:"fare_amount,omitempty" minimum:"0"`
			Currency    string `json:"currency,omitempty" minLength:"3" maxLength:"3"`
		}
	}) (*struct {
		Body struct {
			Route RouteView `json:"route"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		route, err := svc.CreateRoute(ctx, p.TenantID, transport.NewRoute{
			Name: in.Body.Name, Description: in.Body.Description,
			FareAmount: in.Body.FareAmount, Currency: in.Body.Currency,
		}, transport.Author{UserID: p.UserID})
		if err != nil {
			return nil, transportError(err)
		}
		out := &struct {
			Body struct {
				Route RouteView `json:"route"`
			}
		}{}
		out.Body.Route = routeView(route)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-transport-vehicles",
		Method:      http.MethodGet,
		Path:        "/v1/transport/routes/{id}/vehicles",
		Summary:     "A route's fleet, newest first",
		Tags:        []string{"Transport"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*struct {
		Body struct {
			Vehicles []VehicleView `json:"vehicles"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		routeID, err := parseUUID(in.ID, "route")
		if err != nil {
			return nil, err
		}
		list, err := svc.ListVehicles(ctx, p.TenantID, routeID)
		if err != nil {
			return nil, transportError(err)
		}
		out := &struct {
			Body struct {
				Vehicles []VehicleView `json:"vehicles"`
			}
		}{}
		out.Body.Vehicles = make([]VehicleView, 0, len(list))
		for _, v := range list {
			out.Body.Vehicles = append(out.Body.Vehicles, vehicleView(v))
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "add-transport-vehicle",
		Method:        http.MethodPost,
		Path:          "/v1/transport/routes/{id}/vehicles",
		Summary:       "Add a vehicle to a route",
		DefaultStatus: http.StatusCreated,
		Tags:          []string{"Transport"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id" format:"uuid"`
		Body struct {
			RegistrationNo string `json:"registration_no" minLength:"1" maxLength:"60"`
			Capacity       int    `json:"capacity,omitempty" minimum:"0"`
			DriverName     string `json:"driver_name,omitempty" maxLength:"200"`
			DriverPhone    string `json:"driver_phone,omitempty" maxLength:"40"`
		}
	}) (*struct {
		Body struct {
			Vehicle VehicleView `json:"vehicle"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		routeID, err := parseUUID(in.ID, "route")
		if err != nil {
			return nil, err
		}
		v, err := svc.AddVehicle(ctx, p.TenantID, transport.NewVehicle{
			RouteID: routeID, RegistrationNo: in.Body.RegistrationNo, Capacity: in.Body.Capacity,
			DriverName: in.Body.DriverName, DriverPhone: in.Body.DriverPhone,
		}, transport.Author{UserID: p.UserID})
		if err != nil {
			return nil, transportError(err)
		}
		out := &struct {
			Body struct {
				Vehicle VehicleView `json:"vehicle"`
			}
		}{}
		out.Body.Vehicle = vehicleView(v)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-transport-assignments",
		Method:      http.MethodGet,
		Path:        "/v1/transport/assignments",
		Summary:     "Student route assignments, newest first",
		Description: "Keyset-paginated. Filter by route_id and/or student_id.",
		Tags:        []string{"Transport"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		RouteID   string `query:"route_id" format:"uuid"`
		StudentID string `query:"student_id" format:"uuid"`
		Limit     int    `query:"limit" minimum:"1" maximum:"100" default:"50"`
		Cursor    string `query:"cursor"`
	}) (*struct {
		Body struct {
			Assignments []TransportAssignmentView `json:"assignments"`
			NextCursor  string                    `json:"next_cursor,omitempty"`
			HasMore     bool                      `json:"has_more"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		routeID, err := optionalUUIDPtr(in.RouteID, "route")
		if err != nil {
			return nil, err
		}
		studentID, err := optionalUUIDPtr(in.StudentID, "student")
		if err != nil {
			return nil, err
		}
		page, err := svc.ListAssignments(ctx, p.TenantID,
			transport.AssignmentFilter{RouteID: routeID, StudentID: studentID},
			transport.PageParams{Limit: in.Limit, Cursor: in.Cursor})
		if err != nil {
			return nil, transportError(err)
		}
		out := &struct {
			Body struct {
				Assignments []TransportAssignmentView `json:"assignments"`
				NextCursor  string                    `json:"next_cursor,omitempty"`
				HasMore     bool                      `json:"has_more"`
			}
		}{}
		out.Body.Assignments = make([]TransportAssignmentView, 0, len(page.Assignments))
		for _, a := range page.Assignments {
			out.Body.Assignments = append(out.Body.Assignments, transportAssignmentView(a))
		}
		out.Body.NextCursor = page.NextCursor
		out.Body.HasMore = page.HasMore
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "assign-transport",
		Method:        http.MethodPost,
		Path:          "/v1/transport/assignments",
		Summary:       "Assign a student to a route",
		Description:   "A student rides one route: a second assignment is refused with 409.",
		DefaultStatus: http.StatusCreated,
		Tags:          []string{"Transport"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		Body struct {
			RouteID   string `json:"route_id" format:"uuid"`
			StudentID string `json:"student_id" format:"uuid"`
			StopName  string `json:"stop_name,omitempty" maxLength:"160"`
		}
	}) (*struct {
		Body struct {
			Assignment TransportAssignmentView `json:"assignment"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		routeID, err := parseUUID(in.Body.RouteID, "route")
		if err != nil {
			return nil, err
		}
		studentID, err := parseUUID(in.Body.StudentID, "student")
		if err != nil {
			return nil, err
		}
		a, err := svc.AssignStudent(ctx, p.TenantID, transport.NewAssignment{
			RouteID: routeID, StudentID: studentID, StopName: in.Body.StopName,
		}, transport.Author{UserID: p.UserID})
		if err != nil {
			return nil, transportError(err)
		}
		out := &struct {
			Body struct {
				Assignment TransportAssignmentView `json:"assignment"`
			}
		}{}
		out.Body.Assignment = transportAssignmentView(a)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "unassign-transport",
		Method:        http.MethodDelete,
		Path:          "/v1/transport/assignments/{id}",
		Summary:       "Take a student off their route",
		DefaultStatus: http.StatusNoContent,
		Tags:          []string{"Transport"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*struct{}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		id, err := parseUUID(in.ID, "assignment")
		if err != nil {
			return nil, err
		}
		if err := svc.Unassign(ctx, p.TenantID, id, transport.Author{UserID: p.UserID}); err != nil {
			return nil, transportError(err)
		}
		return &struct{}{}, nil
	})
}

// transportError maps the transport package's sentinels onto status codes.
func transportError(err error) error {
	switch {
	case errors.Is(err, transport.ErrNotFound):
		return huma.Error404NotFound("We couldn't find that route, vehicle or assignment.")
	case errors.Is(err, transport.ErrAlreadyAssigned):
		return huma.Error409Conflict("That student already rides a route.")
	case errors.Is(err, transport.ErrInvalidPage):
		return huma.Error422UnprocessableEntity("That page link is no longer valid. Start from the first page.")
	case errors.Is(err, transport.ErrInvalidRoute),
		errors.Is(err, transport.ErrInvalidVehicle),
		errors.Is(err, transport.ErrInvalidAssignment):
		return huma.Error422UnprocessableEntity("Check the route, vehicle and assignment details and try again.")
	default:
		return err
	}
}
