package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ebnsina/muallim-api/internal/academics"
	"github.com/ebnsina/muallim-api/internal/auth"
)

// AttendanceMarkInput is one student's status in a submitted register.
type AttendanceMarkInput struct {
	StudentID string `json:"student_id" format:"uuid"`
	Status    string `json:"status" enum:"present,absent,late,excused"`
}

// RegisterEntryView is one student's mark on a day.
type RegisterEntryView struct {
	StudentID   string `json:"student_id" format:"uuid"`
	AdmissionNo string `json:"admission_no"`
	FullName    string `json:"full_name"`
	Status      string `json:"status" enum:"present,absent,late,excused"`
}

// AttendanceDayView is one day of a student's history.
type AttendanceDayView struct {
	OnDate    string `json:"on_date" format:"date"`
	SectionID string `json:"section_id,omitempty" format:"uuid"`
	Status    string `json:"status" enum:"present,absent,late,excused"`
}

// AttendanceSummaryView tallies a student's days by status.
type AttendanceSummaryView struct {
	Present int `json:"present"`
	Absent  int `json:"absent"`
	Late    int `json:"late"`
	Excused int `json:"excused"`
	Total   int `json:"total"`
}

func registerAttendance(api huma.API, svc *academics.Service) {
	admin := []map[string][]string{{"bearer": {}}}

	huma.Register(api, huma.Operation{
		OperationID: "mark-attendance",
		Method:      http.MethodPost,
		Path:        "/v1/attendance",
		Summary:     "Mark a day's register",
		Description: "Upserts one row per student per day — re-marking a day updates it. " +
			"Send every student's status in one call.",
		DefaultStatus: http.StatusOK,
		Tags:          []string{"Attendance"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		Body struct {
			SectionID string                `json:"section_id,omitempty" format:"uuid"`
			OnDate    string                `json:"on_date" format:"date"`
			Entries   []AttendanceMarkInput `json:"entries" minItems:"1" maxItems:"500"`
		}
	}) (*struct {
		Body struct {
			Marked int `json:"marked"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		on, err := time.Parse(dateLayout, in.Body.OnDate)
		if err != nil {
			return nil, huma.Error422UnprocessableEntity("on_date must be a calendar date, YYYY-MM-DD.")
		}
		section, err := optionalUUIDPtr(in.Body.SectionID, "section")
		if err != nil {
			return nil, err
		}

		entries := make([]academics.AttendanceEntry, 0, len(in.Body.Entries))
		for _, e := range in.Body.Entries {
			sid, err := parseUUID(e.StudentID, "student")
			if err != nil {
				return nil, err
			}
			entries = append(entries, academics.AttendanceEntry{StudentID: sid, Status: e.Status})
		}

		marked, err := svc.MarkAttendance(ctx, p.TenantID, academics.AttendanceMark{
			SectionID: section, OnDate: on, Entries: entries,
		}, academics.Author{UserID: p.UserID})
		if err != nil {
			return nil, academicsError(err)
		}
		out := &struct {
			Body struct {
				Marked int `json:"marked"`
			}
		}{}
		out.Body.Marked = marked
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "section-register",
		Method:      http.MethodGet,
		Path:        "/v1/attendance",
		Summary:     "A section's register for a day",
		Tags:        []string{"Attendance"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		SectionID string `query:"section_id" format:"uuid" required:"true"`
		Date      string `query:"date" format:"date" required:"true"`
	}) (*struct {
		Body struct {
			Entries []RegisterEntryView `json:"entries"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		sectionID, err := parseUUID(in.SectionID, "section")
		if err != nil {
			return nil, err
		}
		on, err := time.Parse(dateLayout, in.Date)
		if err != nil {
			return nil, huma.Error422UnprocessableEntity("date must be a calendar date, YYYY-MM-DD.")
		}
		rows, err := svc.Register(ctx, p.TenantID, sectionID, on)
		if err != nil {
			return nil, academicsError(err)
		}
		out := &struct {
			Body struct {
				Entries []RegisterEntryView `json:"entries"`
			}
		}{}
		out.Body.Entries = make([]RegisterEntryView, 0, len(rows))
		for _, r := range rows {
			out.Body.Entries = append(out.Body.Entries, RegisterEntryView{
				StudentID: r.StudentID.String(), AdmissionNo: r.AdmissionNo,
				FullName: r.FullName, Status: r.Status,
			})
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "student-attendance",
		Method:      http.MethodGet,
		Path:        "/v1/students/{id}/attendance",
		Summary:     "A student's attendance over a range",
		Tags:        []string{"Attendance"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id" format:"uuid"`
		From string `query:"from" format:"date" required:"true"`
		To   string `query:"to" format:"date" required:"true"`
	}) (*struct {
		Body struct {
			Days    []AttendanceDayView   `json:"days"`
			Summary AttendanceSummaryView `json:"summary"`
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
		from, to, err := parseDates(in.From, in.To)
		if err != nil {
			return nil, err
		}
		days, summary, err := svc.StudentAttendance(ctx, p.TenantID, studentID, from, to)
		if err != nil {
			return nil, academicsError(err)
		}
		out := &struct {
			Body struct {
				Days    []AttendanceDayView   `json:"days"`
				Summary AttendanceSummaryView `json:"summary"`
			}
		}{}
		out.Body.Days = make([]AttendanceDayView, 0, len(days))
		for _, d := range days {
			out.Body.Days = append(out.Body.Days, AttendanceDayView{
				OnDate: d.OnDate.Format(dateLayout), SectionID: uuidPtrString(d.SectionID), Status: d.Status,
			})
		}
		out.Body.Summary = AttendanceSummaryView{
			Present: summary.Present, Absent: summary.Absent, Late: summary.Late,
			Excused: summary.Excused, Total: summary.Total,
		}
		return out, nil
	})
}
