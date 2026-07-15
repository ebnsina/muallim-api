package httpapi

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/ebnsina/muallim-api/internal/academics"
	"github.com/ebnsina/muallim-api/internal/auth"
)

// StudentView is a student on a roll.
type StudentView struct {
	ID           string `json:"id" format:"uuid"`
	AdmissionNo  string `json:"admission_no"`
	FullName     string `json:"full_name"`
	GradeLevelID string `json:"grade_level_id,omitempty" format:"uuid"`
	SectionID    string `json:"section_id,omitempty" format:"uuid"`
	Roll         int    `json:"roll"`
	Status       string `json:"status" enum:"active,inactive,graduated,transferred"`
	UserID       string `json:"user_id,omitempty" format:"uuid"`
}

// GuardianView is a student's contact.
type GuardianView struct {
	ID        string `json:"id" format:"uuid"`
	FullName  string `json:"full_name"`
	Phone     string `json:"phone,omitempty"`
	Email     string `json:"email,omitempty"`
	Relation  string `json:"relation,omitempty"`
	IsPrimary bool   `json:"is_primary"`
}

func studentView(s academics.Student) StudentView {
	return StudentView{
		ID: s.ID.String(), AdmissionNo: s.AdmissionNo, FullName: s.FullName,
		GradeLevelID: uuidPtrString(s.GradeLevelID), SectionID: uuidPtrString(s.SectionID),
		Roll: s.Roll, Status: s.Status, UserID: uuidPtrString(s.UserID),
	}
}

func uuidPtrString(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	return id.String()
}

// optionalUUIDPtr parses an optional uuid field: empty string leaves it nil, a bad
// value is a 422, and a value becomes a pointer.
func optionalUUIDPtr(raw, label string) (*uuid.UUID, error) {
	if raw == "" {
		return nil, nil
	}
	id, err := parseUUID(raw, label)
	if err != nil {
		return nil, err
	}
	return &id, nil
}

func registerStudents(api huma.API, svc *academics.Service) {
	admin := []map[string][]string{{"bearer": {}}}

	huma.Register(api, huma.Operation{
		OperationID: "list-students",
		Method:      http.MethodGet,
		Path:        "/v1/students",
		Summary:     "The student roster, by name",
		Description: "Keyset-paginated: pass the next_cursor from the previous page. Filter by class " +
			"with grade_level_id.",
		Tags:     []string{"Students"},
		Security: admin,
	}, func(ctx context.Context, in *struct {
		GradeLevelID string `query:"grade_level_id" format:"uuid"`
		Limit        int    `query:"limit" minimum:"1" maximum:"100" default:"50"`
		Cursor       string `query:"cursor"`
	}) (*struct {
		Body struct {
			Students   []StudentView `json:"students"`
			NextCursor string        `json:"next_cursor,omitempty"`
			HasMore    bool          `json:"has_more"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		grade, err := optionalUUIDPtr(in.GradeLevelID, "grade level")
		if err != nil {
			return nil, err
		}

		page, err := svc.Roster(ctx, p.TenantID, academics.RosterFilter{GradeLevelID: grade},
			academics.PageParams{Limit: in.Limit, Cursor: in.Cursor})
		if err != nil {
			return nil, academicsError(err)
		}

		out := &struct {
			Body struct {
				Students   []StudentView `json:"students"`
				NextCursor string        `json:"next_cursor,omitempty"`
				HasMore    bool          `json:"has_more"`
			}
		}{}
		out.Body.Students = make([]StudentView, 0, len(page.Students))
		for _, s := range page.Students {
			out.Body.Students = append(out.Body.Students, studentView(s))
		}
		out.Body.NextCursor = page.NextCursor
		out.Body.HasMore = page.HasMore
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "admit-student",
		Method:        http.MethodPost,
		Path:          "/v1/students",
		Summary:       "Admit a student",
		DefaultStatus: http.StatusCreated,
		Tags:          []string{"Students"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		Body struct {
			AdmissionNo  string `json:"admission_no" minLength:"1" maxLength:"60"`
			FullName     string `json:"full_name" minLength:"1" maxLength:"200"`
			GradeLevelID string `json:"grade_level_id,omitempty" format:"uuid"`
			SectionID    string `json:"section_id,omitempty" format:"uuid"`
			Roll         int    `json:"roll,omitempty" minimum:"0" maximum:"100000"`
		}
	}) (*struct {
		Body struct {
			Student StudentView `json:"student"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		grade, err := optionalUUIDPtr(in.Body.GradeLevelID, "grade level")
		if err != nil {
			return nil, err
		}
		section, err := optionalUUIDPtr(in.Body.SectionID, "section")
		if err != nil {
			return nil, err
		}

		student, err := svc.AdmitStudent(ctx, p.TenantID, academics.NewStudent{
			AdmissionNo: in.Body.AdmissionNo, FullName: in.Body.FullName,
			GradeLevelID: grade, SectionID: section, Roll: in.Body.Roll,
		}, academics.Author{UserID: p.UserID})
		if err != nil {
			return nil, academicsError(err)
		}
		out := &struct {
			Body struct {
				Student StudentView `json:"student"`
			}
		}{}
		out.Body.Student = studentView(student)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-student",
		Method:      http.MethodGet,
		Path:        "/v1/students/{id}",
		Summary:     "Read one student",
		Tags:        []string{"Students"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*struct {
		Body struct {
			Student StudentView `json:"student"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		studentID, err := parseUUID(in.ID, "student")
		if err != nil {
			return nil, err
		}
		student, err := svc.Student(ctx, p.TenantID, studentID)
		if err != nil {
			return nil, academicsError(err)
		}
		out := &struct {
			Body struct {
				Student StudentView `json:"student"`
			}
		}{}
		out.Body.Student = studentView(student)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "update-student",
		Method:      http.MethodPatch,
		Path:        "/v1/students/{id}",
		Summary:     "Edit a student's details or placement",
		Tags:        []string{"Students"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id" format:"uuid"`
		Body struct {
			FullName     *string `json:"full_name,omitempty" maxLength:"200"`
			GradeLevelID *string `json:"grade_level_id,omitempty" format:"uuid"`
			SectionID    *string `json:"section_id,omitempty" format:"uuid"`
			Roll         *int    `json:"roll,omitempty" minimum:"0" maximum:"100000"`
			Status       *string `json:"status,omitempty" enum:"active,inactive,graduated,transferred"`
		}
	}) (*struct {
		Body struct {
			Student StudentView `json:"student"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		studentID, err := parseUUID(in.ID, "student")
		if err != nil {
			return nil, err
		}

		patch := academics.StudentPatch{FullName: in.Body.FullName, Roll: in.Body.Roll, Status: in.Body.Status}
		if in.Body.GradeLevelID != nil {
			patch.GradeLevelID, err = optionalUUIDPtr(*in.Body.GradeLevelID, "grade level")
			if err != nil {
				return nil, err
			}
		}
		if in.Body.SectionID != nil {
			patch.SectionID, err = optionalUUIDPtr(*in.Body.SectionID, "section")
			if err != nil {
				return nil, err
			}
		}

		student, err := svc.EditStudent(ctx, p.TenantID, studentID, patch)
		if err != nil {
			return nil, academicsError(err)
		}
		out := &struct {
			Body struct {
				Student StudentView `json:"student"`
			}
		}{}
		out.Body.Student = studentView(student)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "remove-student",
		Method:        http.MethodDelete,
		Path:          "/v1/students/{id}",
		Summary:       "Remove a student",
		DefaultStatus: http.StatusNoContent,
		Tags:          []string{"Students"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*struct{}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		studentID, err := parseUUID(in.ID, "student")
		if err != nil {
			return nil, err
		}
		if err := svc.RemoveStudent(ctx, p.TenantID, studentID); err != nil {
			return nil, academicsError(err)
		}
		return &struct{}{}, nil
	})

	// --- Guardians -------------------------------------------------------------

	huma.Register(api, huma.Operation{
		OperationID: "list-guardians",
		Method:      http.MethodGet,
		Path:        "/v1/students/{id}/guardians",
		Summary:     "A student's guardians, primary first",
		Tags:        []string{"Students"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*struct {
		Body struct {
			Guardians []GuardianView `json:"guardians"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		studentID, err := parseUUID(in.ID, "student")
		if err != nil {
			return nil, err
		}
		guardians, err := svc.Guardians(ctx, p.TenantID, studentID)
		if err != nil {
			return nil, academicsError(err)
		}
		out := &struct {
			Body struct {
				Guardians []GuardianView `json:"guardians"`
			}
		}{}
		out.Body.Guardians = make([]GuardianView, 0, len(guardians))
		for _, g := range guardians {
			out.Body.Guardians = append(out.Body.Guardians, GuardianView{
				ID: g.ID.String(), FullName: g.FullName, Phone: g.Phone,
				Email: g.Email, Relation: g.Relation, IsPrimary: g.IsPrimary,
			})
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "add-guardian",
		Method:        http.MethodPost,
		Path:          "/v1/students/{id}/guardians",
		Summary:       "Add a guardian to a student",
		DefaultStatus: http.StatusCreated,
		Tags:          []string{"Students"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id" format:"uuid"`
		Body struct {
			FullName  string `json:"full_name" minLength:"1" maxLength:"200"`
			Phone     string `json:"phone,omitempty" maxLength:"40"`
			Email     string `json:"email,omitempty" maxLength:"200"`
			Relation  string `json:"relation,omitempty" maxLength:"60"`
			IsPrimary bool   `json:"is_primary,omitempty"`
		}
	}) (*struct {
		Body struct {
			Guardian GuardianView `json:"guardian"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		studentID, err := parseUUID(in.ID, "student")
		if err != nil {
			return nil, err
		}
		g, err := svc.AddGuardian(ctx, p.TenantID, studentID, academics.NewGuardian{
			FullName: in.Body.FullName, Phone: in.Body.Phone, Email: in.Body.Email,
			Relation: in.Body.Relation, IsPrimary: in.Body.IsPrimary,
		})
		if err != nil {
			return nil, academicsError(err)
		}
		out := &struct {
			Body struct {
				Guardian GuardianView `json:"guardian"`
			}
		}{}
		out.Body.Guardian = GuardianView{
			ID: g.ID.String(), FullName: g.FullName, Phone: g.Phone,
			Email: g.Email, Relation: g.Relation, IsPrimary: g.IsPrimary,
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "remove-guardian",
		Method:        http.MethodDelete,
		Path:          "/v1/students/{id}/guardians/{guardian_id}",
		Summary:       "Unlink a guardian from a student",
		DefaultStatus: http.StatusNoContent,
		Tags:          []string{"Students"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		ID         string `path:"id" format:"uuid"`
		GuardianID string `path:"guardian_id" format:"uuid"`
	}) (*struct{}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		studentID, err := parseUUID(in.ID, "student")
		if err != nil {
			return nil, err
		}
		guardianID, err := parseUUID(in.GuardianID, "guardian")
		if err != nil {
			return nil, err
		}
		if err := svc.RemoveGuardian(ctx, p.TenantID, studentID, guardianID); err != nil {
			return nil, academicsError(err)
		}
		return &struct{}{}, nil
	})
}
