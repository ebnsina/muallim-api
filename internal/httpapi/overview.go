package httpapi

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ebnsina/muallim-api/internal/auth"
	"github.com/ebnsina/muallim-api/internal/overview"
)

// AttendanceTodayView tallies today's register across the workspace.
type AttendanceTodayView struct {
	Present int `json:"present"`
	Absent  int `json:"absent"`
	Late    int `json:"late"`
	Excused int `json:"excused"`
	Total   int `json:"total"`
}

// OverviewView is the institution dashboard at a glance.
type OverviewView struct {
	Students        int                 `json:"students"`
	Staff           int                 `json:"staff"`
	Classes         int                 `json:"classes"`
	Subjects        int                 `json:"subjects"`
	AttendanceToday AttendanceTodayView `json:"attendance_today"`
	OutstandingFees map[string]int64    `json:"outstanding_fees"`
	Notices         int                 `json:"notices"`
}

func registerOverview(api huma.API, svc *overview.Service) {
	admin := []map[string][]string{{"bearer": {}}}

	huma.Register(api, huma.Operation{
		OperationID: "institution-overview",
		Method:      http.MethodGet,
		Path:        "/v1/overview",
		Summary:     "The institution dashboard at a glance",
		Description: "Aggregate counts for the admin home: students, staff, classes, " +
			"today's attendance, outstanding fees by currency, and notices.",
		Tags:     []string{"Overview"},
		Security: admin,
	}, func(ctx context.Context, _ *struct{}) (*struct {
		Body struct {
			Overview OverviewView `json:"overview"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		snap, err := svc.Snapshot(ctx, p.TenantID)
		if err != nil {
			return nil, err
		}
		out := &struct {
			Body struct {
				Overview OverviewView `json:"overview"`
			}
		}{}
		out.Body.Overview = OverviewView{
			Students: snap.Students, Staff: snap.Staff, Classes: snap.Classes,
			Subjects: snap.Subjects, Notices: snap.Notices,
			AttendanceToday: AttendanceTodayView{
				Present: snap.AttendanceToday.Present, Absent: snap.AttendanceToday.Absent,
				Late: snap.AttendanceToday.Late, Excused: snap.AttendanceToday.Excused,
				Total: snap.AttendanceToday.Total,
			},
			OutstandingFees: snap.OutstandingFees,
		}
		if out.Body.Overview.OutstandingFees == nil {
			out.Body.Overview.OutstandingFees = map[string]int64{}
		}
		return out, nil
	})
}
