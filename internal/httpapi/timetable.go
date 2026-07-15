package httpapi

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ebnsina/muallim-api/internal/academics"
	"github.com/ebnsina/muallim-api/internal/auth"
)

// PeriodView is one slot in a section's weekly timetable.
type PeriodView struct {
	ID          string `json:"id" format:"uuid"`
	SectionID   string `json:"section_id" format:"uuid"`
	SubjectID   string `json:"subject_id,omitempty" format:"uuid"`
	DayOfWeek   int    `json:"day_of_week" minimum:"0" maximum:"6"`
	StartsAt    string `json:"starts_at" example:"09:00"`
	EndsAt      string `json:"ends_at" example:"09:45"`
	TeacherName string `json:"teacher_name,omitempty"`
	Room        string `json:"room,omitempty"`
}

func periodView(p academics.Period) PeriodView {
	return PeriodView{
		ID: p.ID.String(), SectionID: p.SectionID.String(), SubjectID: uuidPtrString(p.SubjectID),
		DayOfWeek: p.DayOfWeek, StartsAt: p.StartsAt, EndsAt: p.EndsAt,
		TeacherName: p.TeacherName, Room: p.Room,
	}
}

func registerTimetable(api huma.API, svc *academics.Service) {
	admin := []map[string][]string{{"bearer": {}}}

	huma.Register(api, huma.Operation{
		OperationID: "section-timetable",
		Method:      http.MethodGet,
		Path:        "/v1/sections/{id}/timetable",
		Summary:     "A section's weekly timetable, in grid order",
		Tags:        []string{"Timetable"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*struct {
		Body struct {
			Periods []PeriodView `json:"periods"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		sectionID, err := parseUUID(in.ID, "section")
		if err != nil {
			return nil, err
		}
		periods, err := svc.SectionTimetable(ctx, p.TenantID, sectionID)
		if err != nil {
			return nil, academicsError(err)
		}
		out := &struct {
			Body struct {
				Periods []PeriodView `json:"periods"`
			}
		}{}
		out.Body.Periods = make([]PeriodView, 0, len(periods))
		for _, period := range periods {
			out.Body.Periods = append(out.Body.Periods, periodView(period))
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "add-timetable-period",
		Method:        http.MethodPost,
		Path:          "/v1/sections/{id}/timetable",
		Summary:       "Add a period to a section's timetable",
		DefaultStatus: http.StatusCreated,
		Tags:          []string{"Timetable"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id" format:"uuid"`
		Body struct {
			SubjectID   string `json:"subject_id,omitempty" format:"uuid"`
			DayOfWeek   int    `json:"day_of_week" minimum:"0" maximum:"6"`
			StartsAt    string `json:"starts_at" example:"09:00"`
			EndsAt      string `json:"ends_at" example:"09:45"`
			TeacherName string `json:"teacher_name,omitempty" maxLength:"200"`
			Room        string `json:"room,omitempty" maxLength:"60"`
		}
	}) (*struct {
		Body struct {
			Period PeriodView `json:"period"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		sectionID, err := parseUUID(in.ID, "section")
		if err != nil {
			return nil, err
		}
		subject, err := optionalUUIDPtr(in.Body.SubjectID, "subject")
		if err != nil {
			return nil, err
		}
		period, err := svc.AddPeriod(ctx, p.TenantID, academics.NewPeriod{
			SectionID: sectionID, SubjectID: subject, DayOfWeek: in.Body.DayOfWeek,
			StartsAt: in.Body.StartsAt, EndsAt: in.Body.EndsAt,
			TeacherName: in.Body.TeacherName, Room: in.Body.Room,
		})
		if err != nil {
			return nil, academicsError(err)
		}
		out := &struct {
			Body struct {
				Period PeriodView `json:"period"`
			}
		}{}
		out.Body.Period = periodView(period)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "remove-timetable-period",
		Method:        http.MethodDelete,
		Path:          "/v1/timetable/{id}",
		Summary:       "Remove a period from a timetable",
		DefaultStatus: http.StatusNoContent,
		Tags:          []string{"Timetable"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*struct{}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		periodID, err := parseUUID(in.ID, "period")
		if err != nil {
			return nil, err
		}
		if err := svc.RemovePeriod(ctx, p.TenantID, periodID); err != nil {
			return nil, academicsError(err)
		}
		return &struct{}{}, nil
	})
}
