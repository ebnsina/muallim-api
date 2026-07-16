package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ebnsina/muallim-api/internal/auth"
	"github.com/ebnsina/muallim-api/internal/hostel"
)

// BuildingView is a boarding house the institution runs.
type BuildingView struct {
	ID          string `json:"id" format:"uuid"`
	Name        string `json:"name"`
	WardenName  string `json:"warden_name,omitempty"`
	WardenPhone string `json:"warden_phone,omitempty"`
}

// RoomView is a room inside a building, with its bed capacity and live occupancy.
type RoomView struct {
	ID         string `json:"id" format:"uuid"`
	BuildingID string `json:"building_id" format:"uuid"`
	RoomNo     string `json:"room_no"`
	Capacity   int    `json:"capacity"`
	Occupied   int    `json:"occupied"`
}

// AllocationView is one student's bed in one room.
type AllocationView struct {
	ID          string `json:"id" format:"uuid"`
	RoomID      string `json:"room_id" format:"uuid"`
	StudentID   string `json:"student_id" format:"uuid"`
	AllocatedAt string `json:"allocated_at" format:"date-time"`
	VacatedAt   string `json:"vacated_at,omitempty" format:"date-time"`
	Status      string `json:"status" enum:"active,vacated"`
}

func strDeref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func buildingView(b hostel.Building) BuildingView {
	return BuildingView{
		ID: b.ID.String(), Name: b.Name,
		WardenName: strDeref(b.WardenName), WardenPhone: strDeref(b.WardenPhone),
	}
}

func roomView(r hostel.Room) RoomView {
	return RoomView{
		ID: r.ID.String(), BuildingID: r.BuildingID.String(), RoomNo: r.RoomNo,
		Capacity: r.Capacity, Occupied: r.Occupied,
	}
}

func allocationView(a hostel.Allocation) AllocationView {
	v := AllocationView{
		ID: a.ID.String(), RoomID: a.RoomID.String(), StudentID: a.StudentID.String(),
		AllocatedAt: a.AllocatedAt.Format(time.RFC3339), Status: a.Status,
	}
	if a.VacatedAt != nil {
		v.VacatedAt = a.VacatedAt.Format(time.RFC3339)
	}
	return v
}

func optionalStringPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func registerHostel(api huma.API, svc *hostel.Service) {
	admin := []map[string][]string{{"bearer": {}}}

	huma.Register(api, huma.Operation{
		OperationID: "list-hostel-buildings",
		Method:      http.MethodGet,
		Path:        "/v1/hostel/buildings",
		Summary:     "The workspace's boarding buildings, by name",
		Description: "Keyset-paginated: pass the next_cursor from the previous page.",
		Tags:        []string{"Hostel"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		Limit  int    `query:"limit" minimum:"1" maximum:"100" default:"50"`
		Cursor string `query:"cursor"`
	}) (*struct {
		Body struct {
			Buildings  []BuildingView `json:"buildings"`
			NextCursor string         `json:"next_cursor,omitempty"`
			HasMore    bool           `json:"has_more"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		page, err := svc.ListBuildings(ctx, p.TenantID, hostel.PageParams{Limit: in.Limit, Cursor: in.Cursor})
		if err != nil {
			return nil, hostelError(err)
		}
		out := &struct {
			Body struct {
				Buildings  []BuildingView `json:"buildings"`
				NextCursor string         `json:"next_cursor,omitempty"`
				HasMore    bool           `json:"has_more"`
			}
		}{}
		out.Body.Buildings = make([]BuildingView, 0, len(page.Buildings))
		for _, b := range page.Buildings {
			out.Body.Buildings = append(out.Body.Buildings, buildingView(b))
		}
		out.Body.NextCursor = page.NextCursor
		out.Body.HasMore = page.HasMore
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "create-hostel-building",
		Method:        http.MethodPost,
		Path:          "/v1/hostel/buildings",
		Summary:       "Register a boarding building",
		DefaultStatus: http.StatusCreated,
		Tags:          []string{"Hostel"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		Body struct {
			Name        string `json:"name" minLength:"1" maxLength:"160"`
			WardenName  string `json:"warden_name,omitempty" maxLength:"200"`
			WardenPhone string `json:"warden_phone,omitempty" maxLength:"40"`
		}
	}) (*struct {
		Body struct {
			Building BuildingView `json:"building"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		b, err := svc.CreateBuilding(ctx, p.TenantID, hostel.NewBuilding{
			Name: in.Body.Name, WardenName: optionalStringPtr(in.Body.WardenName),
			WardenPhone: optionalStringPtr(in.Body.WardenPhone),
		}, hostel.Author{UserID: p.UserID})
		if err != nil {
			return nil, hostelError(err)
		}
		out := &struct {
			Body struct {
				Building BuildingView `json:"building"`
			}
		}{}
		out.Body.Building = buildingView(b)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-hostel-rooms",
		Method:      http.MethodGet,
		Path:        "/v1/hostel/buildings/{id}/rooms",
		Summary:     "A building's rooms, by room number",
		Tags:        []string{"Hostel"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*struct {
		Body struct {
			Rooms []RoomView `json:"rooms"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		buildingID, err := parseUUID(in.ID, "building")
		if err != nil {
			return nil, err
		}
		rooms, err := svc.ListRooms(ctx, p.TenantID, buildingID)
		if err != nil {
			return nil, hostelError(err)
		}
		out := &struct {
			Body struct {
				Rooms []RoomView `json:"rooms"`
			}
		}{}
		out.Body.Rooms = make([]RoomView, 0, len(rooms))
		for _, r := range rooms {
			out.Body.Rooms = append(out.Body.Rooms, roomView(r))
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "add-hostel-room",
		Method:        http.MethodPost,
		Path:          "/v1/hostel/buildings/{id}/rooms",
		Summary:       "Add a room to a building",
		DefaultStatus: http.StatusCreated,
		Tags:          []string{"Hostel"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id" format:"uuid"`
		Body struct {
			RoomNo   string `json:"room_no" minLength:"1" maxLength:"40"`
			Capacity int    `json:"capacity,omitempty" minimum:"0" maximum:"1000"`
		}
	}) (*struct {
		Body struct {
			Room RoomView `json:"room"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		buildingID, err := parseUUID(in.ID, "building")
		if err != nil {
			return nil, err
		}
		r, err := svc.AddRoom(ctx, p.TenantID, hostel.NewRoom{
			BuildingID: buildingID, RoomNo: in.Body.RoomNo, Capacity: in.Body.Capacity,
		}, hostel.Author{UserID: p.UserID})
		if err != nil {
			return nil, hostelError(err)
		}
		out := &struct {
			Body struct {
				Room RoomView `json:"room"`
			}
		}{}
		out.Body.Room = roomView(r)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-hostel-allocations",
		Method:      http.MethodGet,
		Path:        "/v1/hostel/allocations",
		Summary:     "Room allocations, newest first",
		Description: "Keyset-paginated. Filter by room_id, student_id and/or status.",
		Tags:        []string{"Hostel"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		RoomID    string `query:"room_id" format:"uuid"`
		StudentID string `query:"student_id" format:"uuid"`
		Status    string `query:"status" enum:"active,vacated"`
		Limit     int    `query:"limit" minimum:"1" maximum:"100" default:"50"`
		Cursor    string `query:"cursor"`
	}) (*struct {
		Body struct {
			Allocations []AllocationView `json:"allocations"`
			NextCursor  string           `json:"next_cursor,omitempty"`
			HasMore     bool             `json:"has_more"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		room, err := optionalUUIDPtr(in.RoomID, "room")
		if err != nil {
			return nil, err
		}
		student, err := optionalUUIDPtr(in.StudentID, "student")
		if err != nil {
			return nil, err
		}
		page, err := svc.ListAllocations(ctx, p.TenantID, hostel.AllocationFilter{
			RoomID: room, StudentID: student, Status: in.Status,
		}, hostel.PageParams{Limit: in.Limit, Cursor: in.Cursor})
		if err != nil {
			return nil, hostelError(err)
		}
		out := &struct {
			Body struct {
				Allocations []AllocationView `json:"allocations"`
				NextCursor  string           `json:"next_cursor,omitempty"`
				HasMore     bool             `json:"has_more"`
			}
		}{}
		out.Body.Allocations = make([]AllocationView, 0, len(page.Allocations))
		for _, a := range page.Allocations {
			out.Body.Allocations = append(out.Body.Allocations, allocationView(a))
		}
		out.Body.NextCursor = page.NextCursor
		out.Body.HasMore = page.HasMore
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "allocate-hostel-room",
		Method:        http.MethodPost,
		Path:          "/v1/hostel/allocations",
		Summary:       "Allocate a room to a student",
		Description:   "Refused when the room is full, or when the student already holds a bed.",
		DefaultStatus: http.StatusCreated,
		Tags:          []string{"Hostel"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		Body struct {
			RoomID    string `json:"room_id" format:"uuid"`
			StudentID string `json:"student_id" format:"uuid"`
		}
	}) (*struct {
		Body struct {
			Allocation AllocationView `json:"allocation"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		roomID, err := parseUUID(in.Body.RoomID, "room")
		if err != nil {
			return nil, err
		}
		studentID, err := parseUUID(in.Body.StudentID, "student")
		if err != nil {
			return nil, err
		}
		a, err := svc.AllocateRoom(ctx, p.TenantID, hostel.NewAllocation{
			RoomID: roomID, StudentID: studentID,
		}, hostel.Author{UserID: p.UserID})
		if err != nil {
			return nil, hostelError(err)
		}
		out := &struct {
			Body struct {
				Allocation AllocationView `json:"allocation"`
			}
		}{}
		out.Body.Allocation = allocationView(a)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "vacate-hostel-allocation",
		Method:      http.MethodDelete,
		Path:        "/v1/hostel/allocations/{id}",
		Summary:     "Vacate a room allocation",
		Description: "Frees the bed and keeps the allocation as boarding history. Idempotent.",
		Tags:        []string{"Hostel"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*struct {
		Body struct {
			Allocation AllocationView `json:"allocation"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		id, err := parseUUID(in.ID, "allocation")
		if err != nil {
			return nil, err
		}
		a, err := svc.VacateRoom(ctx, p.TenantID, id, hostel.Author{UserID: p.UserID})
		if err != nil {
			return nil, hostelError(err)
		}
		out := &struct {
			Body struct {
				Allocation AllocationView `json:"allocation"`
			}
		}{}
		out.Body.Allocation = allocationView(a)
		return out, nil
	})
}

// hostelError maps the hostel package's sentinels onto status codes.
func hostelError(err error) error {
	switch {
	case errors.Is(err, hostel.ErrNotFound):
		return huma.Error404NotFound("We couldn't find that building, room or allocation.")
	case errors.Is(err, hostel.ErrRoomFull):
		return huma.Error409Conflict("That room is full.")
	case errors.Is(err, hostel.ErrAlreadyAllocated):
		return huma.Error409Conflict("That student already holds a room.")
	case errors.Is(err, hostel.ErrInvalidPage):
		return huma.Error422UnprocessableEntity("That page link is no longer valid. Start from the first page.")
	case errors.Is(err, hostel.ErrInvalidBuilding),
		errors.Is(err, hostel.ErrInvalidRoom),
		errors.Is(err, hostel.ErrInvalidAllocation):
		return huma.Error422UnprocessableEntity("Check the building, room and allocation details and try again.")
	default:
		return err
	}
}
