package httpapi

// Admitting is the one admissions step that crosses a domain boundary: it turns an
// accepted application into a real student (and their guardian). Neither domain
// knows the other, so the orchestration lives here, where the HTTP layer is
// allowed to hold both services. The office assigns the admission number as it
// admits — the application never had one.

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/ebnsina/muallim-api/internal/academics"
	"github.com/ebnsina/muallim-api/internal/admissions"
	"github.com/ebnsina/muallim-api/internal/auth"
)

func registerAdmissionsAdmit(api huma.API, adms *admissions.Service, schooling *academics.Service) {
	huma.Register(api, huma.Operation{
		OperationID:   "admit-application",
		Method:        http.MethodPost,
		Path:          "/v1/admissions/{id}/admit",
		Summary:       "Admit an accepted applicant — creates the student and guardian",
		Tags:          []string{"Admissions"},
		Security:      []map[string][]string{{"bearer": {}}},
		DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id" format:"uuid"`
		Body struct {
			AdmissionNo string `json:"admission_no" minLength:"1" maxLength:"40"`
			SectionID   string `json:"section_id,omitempty" format:"uuid"`
			Roll        int    `json:"roll,omitempty" minimum:"0"`
		}
	}) (*struct {
		Body struct {
			Student     StudentView     `json:"student"`
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

		// The application carries the applicant's name and the class they applied
		// for; refuse before creating anything if it is not accepted, so a rejected
		// or already-admitted one never mints an orphan student.
		app, err := adms.Get(ctx, p.TenantID, id)
		if err != nil {
			return nil, admissionsError(err)
		}
		if app.Status != admissions.StatusAccepted {
			return nil, huma.Error409Conflict("Only an accepted application can be admitted.")
		}

		var sectionID *uuid.UUID
		if in.Body.SectionID != "" {
			s, err := parseUUID(in.Body.SectionID, "section")
			if err != nil {
				return nil, err
			}
			sectionID = &s
		}

		author := academics.Author{UserID: p.UserID}
		student, err := schooling.AdmitStudent(ctx, p.TenantID, academics.NewStudent{
			AdmissionNo:  in.Body.AdmissionNo,
			FullName:     app.ApplicantName,
			GradeLevelID: app.GradeLevelID,
			SectionID:    sectionID,
			Roll:         in.Body.Roll,
		}, author)
		if err != nil {
			return nil, academicsError(err)
		}

		// The application's guardian details become the student's first guardian.
		if app.GuardianName != "" {
			if _, err := schooling.AddGuardian(ctx, p.TenantID, student.ID, academics.NewGuardian{
				FullName: app.GuardianName, Phone: app.GuardianPhone, Email: app.GuardianEmail,
				Relation: "guardian", IsPrimary: true,
			}); err != nil {
				return nil, academicsError(err)
			}
		}

		admitted, err := adms.MarkAdmitted(ctx, p.TenantID, id, student.ID, admissions.Author{UserID: p.UserID})
		if err != nil {
			return nil, admissionsError(err)
		}

		out := &struct {
			Body struct {
				Student     StudentView     `json:"student"`
				Application ApplicationView `json:"application"`
			}
		}{}
		out.Body.Student = studentView(student)
		out.Body.Application = applicationView(admitted)
		return out, nil
	})
}
