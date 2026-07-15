package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ebnsina/muallim-api/internal/auth"
	"github.com/ebnsina/muallim-api/internal/staff"
)

// StaffView is one person on the payroll.
type StaffView struct {
	ID       string `json:"id" format:"uuid"`
	StaffNo  string `json:"staff_no,omitempty"`
	FullName string `json:"full_name"`
	Role     string `json:"role" enum:"teacher,principal,admin,accountant,librarian,support"`
	Email    string `json:"email,omitempty"`
	Phone    string `json:"phone,omitempty"`
	UserID   string `json:"user_id,omitempty" format:"uuid"`
	Status   string `json:"status" enum:"active,inactive"`
	JoinedOn string `json:"joined_on,omitempty" format:"date"`
}

func staffView(s staff.Staff) StaffView {
	v := StaffView{
		ID: s.ID.String(), StaffNo: s.StaffNo, FullName: s.FullName, Role: s.Role,
		Email: s.Email, Phone: s.Phone, UserID: uuidPtrString(s.UserID), Status: s.Status,
	}
	if s.JoinedOn != nil {
		v.JoinedOn = s.JoinedOn.Format(dateLayout)
	}
	return v
}

func registerStaff(api huma.API, svc *staff.Service) {
	admin := []map[string][]string{{"bearer": {}}}

	huma.Register(api, huma.Operation{
		OperationID: "list-staff",
		Method:      http.MethodGet,
		Path:        "/v1/staff",
		Summary:     "The staff roster, by name",
		Description: "Keyset-paginated. Filter by role.",
		Tags:        []string{"Staff"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		Role   string `query:"role" enum:"teacher,principal,admin,accountant,librarian,support"`
		Limit  int    `query:"limit" minimum:"1" maximum:"100" default:"50"`
		Cursor string `query:"cursor"`
	}) (*struct {
		Body struct {
			Staff      []StaffView `json:"staff"`
			NextCursor string      `json:"next_cursor,omitempty"`
			HasMore    bool        `json:"has_more"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		page, err := svc.Roster(ctx, p.TenantID, staff.RosterFilter{Role: in.Role},
			staff.PageParams{Limit: in.Limit, Cursor: in.Cursor})
		if err != nil {
			return nil, staffError(err)
		}
		out := &struct {
			Body struct {
				Staff      []StaffView `json:"staff"`
				NextCursor string      `json:"next_cursor,omitempty"`
				HasMore    bool        `json:"has_more"`
			}
		}{}
		out.Body.Staff = make([]StaffView, 0, len(page.Staff))
		for _, s := range page.Staff {
			out.Body.Staff = append(out.Body.Staff, staffView(s))
		}
		out.Body.NextCursor = page.NextCursor
		out.Body.HasMore = page.HasMore
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "hire-staff",
		Method:        http.MethodPost,
		Path:          "/v1/staff",
		Summary:       "Add a staff record",
		DefaultStatus: http.StatusCreated,
		Tags:          []string{"Staff"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		Body struct {
			StaffNo  string `json:"staff_no,omitempty" maxLength:"60"`
			FullName string `json:"full_name" minLength:"1" maxLength:"200"`
			Role     string `json:"role,omitempty" enum:"teacher,principal,admin,accountant,librarian,support"`
			Email    string `json:"email,omitempty" maxLength:"200"`
			Phone    string `json:"phone,omitempty" maxLength:"40"`
			JoinedOn string `json:"joined_on,omitempty" format:"date"`
		}
	}) (*struct {
		Body struct {
			Staff StaffView `json:"staff"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		joined, err := optionalDate(in.Body.JoinedOn)
		if err != nil {
			return nil, err
		}
		st, err := svc.Hire(ctx, p.TenantID, staff.NewStaff{
			StaffNo: in.Body.StaffNo, FullName: in.Body.FullName, Role: in.Body.Role,
			Email: in.Body.Email, Phone: in.Body.Phone, JoinedOn: joined,
		}, staff.Author{UserID: p.UserID})
		if err != nil {
			return nil, staffError(err)
		}
		out := &struct {
			Body struct {
				Staff StaffView `json:"staff"`
			}
		}{}
		out.Body.Staff = staffView(st)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-staff",
		Method:      http.MethodGet,
		Path:        "/v1/staff/{id}",
		Summary:     "Read one staff record",
		Tags:        []string{"Staff"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*struct {
		Body struct {
			Staff StaffView `json:"staff"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		id, err := parseUUID(in.ID, "staff")
		if err != nil {
			return nil, err
		}
		st, err := svc.Member(ctx, p.TenantID, id)
		if err != nil {
			return nil, staffError(err)
		}
		out := &struct {
			Body struct {
				Staff StaffView `json:"staff"`
			}
		}{}
		out.Body.Staff = staffView(st)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "update-staff",
		Method:      http.MethodPatch,
		Path:        "/v1/staff/{id}",
		Summary:     "Edit a staff record",
		Tags:        []string{"Staff"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id" format:"uuid"`
		Body struct {
			FullName *string `json:"full_name,omitempty" maxLength:"200"`
			Role     *string `json:"role,omitempty" enum:"teacher,principal,admin,accountant,librarian,support"`
			Email    *string `json:"email,omitempty" maxLength:"200"`
			Phone    *string `json:"phone,omitempty" maxLength:"40"`
			Status   *string `json:"status,omitempty" enum:"active,inactive"`
			JoinedOn *string `json:"joined_on,omitempty" format:"date"`
		}
	}) (*struct {
		Body struct {
			Staff StaffView `json:"staff"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		id, err := parseUUID(in.ID, "staff")
		if err != nil {
			return nil, err
		}
		patch := staff.StaffPatch{
			FullName: in.Body.FullName, Role: in.Body.Role, Email: in.Body.Email,
			Phone: in.Body.Phone, Status: in.Body.Status,
		}
		if in.Body.JoinedOn != nil {
			joined, err := optionalDate(*in.Body.JoinedOn)
			if err != nil {
				return nil, err
			}
			patch.JoinedOn = joined
		}
		st, err := svc.Edit(ctx, p.TenantID, id, patch)
		if err != nil {
			return nil, staffError(err)
		}
		out := &struct {
			Body struct {
				Staff StaffView `json:"staff"`
			}
		}{}
		out.Body.Staff = staffView(st)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "remove-staff",
		Method:        http.MethodDelete,
		Path:          "/v1/staff/{id}",
		Summary:       "Remove a staff record",
		DefaultStatus: http.StatusNoContent,
		Tags:          []string{"Staff"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*struct{}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		id, err := parseUUID(in.ID, "staff")
		if err != nil {
			return nil, err
		}
		if err := svc.Remove(ctx, p.TenantID, id); err != nil {
			return nil, staffError(err)
		}
		return &struct{}{}, nil
	})
}

// staffError maps the staff package's sentinels onto status codes.
func staffError(err error) error {
	switch {
	case errors.Is(err, staff.ErrNotFound):
		return huma.Error404NotFound("Not found.")
	case errors.Is(err, staff.ErrStaffNoTaken):
		return huma.Error409Conflict("That staff number is already used in this workspace.")
	case errors.Is(err, staff.ErrInvalidPage):
		return huma.Error422UnprocessableEntity("That page cursor is not valid.")
	case errors.Is(err, staff.ErrInvalidStaff):
		return huma.Error422UnprocessableEntity(err.Error())
	default:
		return err
	}
}
