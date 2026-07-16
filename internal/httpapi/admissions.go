package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/ebnsina/muallim-api/internal/admissions"
	"github.com/ebnsina/muallim-api/internal/auth"
)

// ApplicationView is one prospective student's intake record.
type ApplicationView struct {
	ID            string `json:"id" format:"uuid"`
	ApplicantName string `json:"applicant_name"`
	GuardianName  string `json:"guardian_name,omitempty"`
	GuardianPhone string `json:"guardian_phone,omitempty"`
	GuardianEmail string `json:"guardian_email,omitempty"`
	GradeLevelID  string `json:"grade_level_id,omitempty" format:"uuid"`
	DOB           string `json:"dob,omitempty" format:"date"`
	Status        string `json:"status" enum:"pending,accepted,rejected,admitted"`
	Note          string `json:"note,omitempty"`
	StudentID     string `json:"student_id,omitempty" format:"uuid"`
	SubmittedAt   string `json:"submitted_at" format:"date-time"`
	DecidedAt     string `json:"decided_at,omitempty" format:"date-time"`
}

func applicationView(a admissions.Application) ApplicationView {
	v := ApplicationView{
		ID: a.ID.String(), ApplicantName: a.ApplicantName,
		GuardianName: a.GuardianName, GuardianPhone: a.GuardianPhone, GuardianEmail: a.GuardianEmail,
		GradeLevelID: uuidPtrString(a.GradeLevelID), Status: a.Status, Note: a.Note,
		StudentID:   uuidPtrString(a.StudentID),
		SubmittedAt: a.SubmittedAt.Format(time.RFC3339),
	}
	if a.DOB != nil {
		v.DOB = a.DOB.Format(dateLayout)
	}
	if a.DecidedAt != nil {
		v.DecidedAt = a.DecidedAt.Format(time.RFC3339)
	}
	return v
}

func registerAdmissions(api huma.API, svc *admissions.Service) {
	admin := []map[string][]string{{"bearer": {}}}

	huma.Register(api, huma.Operation{
		OperationID: "list-admissions",
		Method:      http.MethodGet,
		Path:        "/v1/admissions",
		Summary:     "Applications, newest first",
		Description: "Keyset-paginated. Filter by status.",
		Tags:        []string{"Admissions"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		Status string `query:"status" enum:"pending,accepted,rejected,admitted"`
		Limit  int    `query:"limit" minimum:"1" maximum:"100" default:"50"`
		Cursor string `query:"cursor"`
	}) (*struct {
		Body struct {
			Applications []ApplicationView `json:"applications"`
			NextCursor   string            `json:"next_cursor,omitempty"`
			HasMore      bool              `json:"has_more"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		page, err := svc.List(ctx, p.TenantID, admissions.Filter{Status: in.Status},
			admissions.PageParams{Limit: in.Limit, Cursor: in.Cursor})
		if err != nil {
			return nil, admissionsError(err)
		}
		out := &struct {
			Body struct {
				Applications []ApplicationView `json:"applications"`
				NextCursor   string            `json:"next_cursor,omitempty"`
				HasMore      bool              `json:"has_more"`
			}
		}{}
		out.Body.Applications = make([]ApplicationView, 0, len(page.Applications))
		for _, a := range page.Applications {
			out.Body.Applications = append(out.Body.Applications, applicationView(a))
		}
		out.Body.NextCursor = page.NextCursor
		out.Body.HasMore = page.HasMore
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "submit-admission",
		Method:        http.MethodPost,
		Path:          "/v1/admissions",
		Summary:       "Submit an application",
		Description:   "Intake only: an application is not an account and not a student.",
		DefaultStatus: http.StatusCreated,
		Tags:          []string{"Admissions"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		Body struct {
			ApplicantName string `json:"applicant_name" minLength:"1" maxLength:"160"`
			GuardianName  string `json:"guardian_name,omitempty" maxLength:"160"`
			GuardianPhone string `json:"guardian_phone,omitempty" maxLength:"40"`
			GuardianEmail string `json:"guardian_email,omitempty" maxLength:"160"`
			GradeLevelID  string `json:"grade_level_id,omitempty" format:"uuid"`
			DOB           string `json:"dob,omitempty" format:"date"`
			Note          string `json:"note,omitempty" maxLength:"500"`
		}
	}) (*struct {
		Body struct {
			Application ApplicationView `json:"application"`
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
		dob, err := optionalDate(in.Body.DOB)
		if err != nil {
			return nil, err
		}
		app, err := svc.Submit(ctx, p.TenantID, admissions.NewApplication{
			ApplicantName: in.Body.ApplicantName, GuardianName: in.Body.GuardianName,
			GuardianPhone: in.Body.GuardianPhone, GuardianEmail: in.Body.GuardianEmail,
			GradeLevelID: grade, DOB: dob, Note: in.Body.Note,
		}, admissions.Author{UserID: p.UserID})
		if err != nil {
			return nil, admissionsError(err)
		}
		out := &struct {
			Body struct {
				Application ApplicationView `json:"application"`
			}
		}{}
		out.Body.Application = applicationView(app)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-admission",
		Method:      http.MethodGet,
		Path:        "/v1/admissions/{id}",
		Summary:     "One application",
		Tags:        []string{"Admissions"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*struct {
		Body struct {
			Application ApplicationView `json:"application"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		id, err := parseUUID(in.ID, "application")
		if err != nil {
			return nil, err
		}
		app, err := svc.Get(ctx, p.TenantID, id)
		if err != nil {
			return nil, admissionsError(err)
		}
		out := &struct {
			Body struct {
				Application ApplicationView `json:"application"`
			}
		}{}
		out.Body.Application = applicationView(app)
		return out, nil
	})

	decision := func(op, path, summary string, act func(context.Context, uuid.UUID, uuid.UUID, admissions.Author) (admissions.Application, error)) {
		huma.Register(api, huma.Operation{
			OperationID: op,
			Method:      http.MethodPost,
			Path:        path,
			Summary:     summary,
			Tags:        []string{"Admissions"},
			Security:    admin,
		}, func(ctx context.Context, in *struct {
			ID string `path:"id" format:"uuid"`
		}) (*struct {
			Body struct {
				Application ApplicationView `json:"application"`
			}
		}, error) {
			p, err := requirePermission(ctx, auth.PermAcademicsManage)
			if err != nil {
				return nil, err
			}
			id, err := parseUUID(in.ID, "application")
			if err != nil {
				return nil, err
			}
			app, err := act(ctx, p.TenantID, id, admissions.Author{UserID: p.UserID})
			if err != nil {
				return nil, admissionsError(err)
			}
			out := &struct {
				Body struct {
					Application ApplicationView `json:"application"`
				}
			}{}
			out.Body.Application = applicationView(app)
			return out, nil
		})
	}
	decision("accept-admission", "/v1/admissions/{id}/accept", "Accept a pending application", svc.Accept)
	decision("reject-admission", "/v1/admissions/{id}/reject", "Reject a pending application", svc.Reject)
}

// admissionsError maps the admissions package's sentinels onto status codes.
func admissionsError(err error) error {
	switch {
	case errors.Is(err, admissions.ErrNotFound):
		return huma.Error404NotFound("We couldn't find that application.")
	case errors.Is(err, admissions.ErrNotPending):
		return huma.Error409Conflict("Only a pending application can be decided that way.")
	case errors.Is(err, admissions.ErrInvalidApplication):
		return huma.Error422UnprocessableEntity("Check the application details and try again.")
	case errors.Is(err, admissions.ErrInvalidPage):
		return huma.Error422UnprocessableEntity("That page link is no longer valid. Start from the first page.")
	default:
		return err
	}
}
