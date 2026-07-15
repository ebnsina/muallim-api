package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ebnsina/muallim-api/internal/auth"
	"github.com/ebnsina/muallim-api/internal/hifz"
)

// HifzEntryView is one recitation on one day.
type HifzEntryView struct {
	ID        string `json:"id" format:"uuid"`
	StudentID string `json:"student_id" format:"uuid"`
	OnDate    string `json:"on_date" format:"date"`
	Kind      string `json:"kind" enum:"sabaq,sabqi,manzil"`
	Surah     int    `json:"surah" minimum:"1" maximum:"114"`
	AyahFrom  int    `json:"ayah_from" minimum:"1"`
	AyahTo    int    `json:"ayah_to" minimum:"1"`
	Rating    string `json:"rating" enum:"excellent,good,fair,weak"`
	Note      string `json:"note,omitempty"`
}

// HifzSummaryView reads where a student's Sabaq stands and their recent activity.
type HifzSummaryView struct {
	CurrentSabaq *HifzEntryView `json:"current_sabaq,omitempty"`
	Counts       map[string]int `json:"counts"`
}

func hifzEntryView(e hifz.Entry) HifzEntryView {
	return HifzEntryView{
		ID: e.ID.String(), StudentID: e.StudentID.String(), OnDate: e.OnDate.Format(dateLayout),
		Kind: e.Kind, Surah: e.Surah, AyahFrom: e.AyahFrom, AyahTo: e.AyahTo,
		Rating: e.Rating, Note: e.Note,
	}
}

func registerHifz(api huma.API, svc *hifz.Service) {
	admin := []map[string][]string{{"bearer": {}}}

	huma.Register(api, huma.Operation{
		OperationID: "student-hifz-log",
		Method:      http.MethodGet,
		Path:        "/v1/students/{id}/hifz",
		Summary:     "A student's hifz log, newest first",
		Description: "Keyset-paginated: pass the next_cursor from the previous page.",
		Tags:        []string{"Hifz"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		ID     string `path:"id" format:"uuid"`
		Limit  int    `query:"limit" minimum:"1" maximum:"100" default:"50"`
		Cursor string `query:"cursor"`
	}) (*struct {
		Body struct {
			Entries    []HifzEntryView `json:"entries"`
			NextCursor string          `json:"next_cursor,omitempty"`
			HasMore    bool            `json:"has_more"`
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
		page, err := svc.StudentLog(ctx, p.TenantID, studentID, hifz.PageParams{Limit: in.Limit, Cursor: in.Cursor})
		if err != nil {
			return nil, hifzError(err)
		}
		out := &struct {
			Body struct {
				Entries    []HifzEntryView `json:"entries"`
				NextCursor string          `json:"next_cursor,omitempty"`
				HasMore    bool            `json:"has_more"`
			}
		}{}
		out.Body.Entries = make([]HifzEntryView, 0, len(page.Entries))
		for _, e := range page.Entries {
			out.Body.Entries = append(out.Body.Entries, hifzEntryView(e))
		}
		out.Body.NextCursor = page.NextCursor
		out.Body.HasMore = page.HasMore
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "student-hifz-summary",
		Method:      http.MethodGet,
		Path:        "/v1/students/{id}/hifz/summary",
		Summary:     "Where a student's Sabaq stands, and recent activity",
		Description: "Counts recitations by kind over the trailing window (default 30 days).",
		Tags:        []string{"Hifz"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id" format:"uuid"`
		Days int    `query:"days" minimum:"1" maximum:"365" default:"30"`
	}) (*struct {
		Body struct {
			Summary HifzSummaryView `json:"summary"`
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
		since := time.Now().AddDate(0, 0, -in.Days)
		summary, err := svc.Summary(ctx, p.TenantID, studentID, since)
		if err != nil {
			return nil, hifzError(err)
		}
		out := &struct {
			Body struct {
				Summary HifzSummaryView `json:"summary"`
			}
		}{}
		out.Body.Summary.Counts = summary.Counts
		if out.Body.Summary.Counts == nil {
			out.Body.Summary.Counts = map[string]int{}
		}
		if summary.CurrentSabaq != nil {
			v := hifzEntryView(*summary.CurrentSabaq)
			out.Body.Summary.CurrentSabaq = &v
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "log-hifz",
		Method:        http.MethodPost,
		Path:          "/v1/students/{id}/hifz",
		Summary:       "Log a recitation for a student",
		DefaultStatus: http.StatusCreated,
		Tags:          []string{"Hifz"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id" format:"uuid"`
		Body struct {
			OnDate   string `json:"on_date" format:"date"`
			Kind     string `json:"kind" enum:"sabaq,sabqi,manzil"`
			Surah    int    `json:"surah" minimum:"1" maximum:"114"`
			AyahFrom int    `json:"ayah_from" minimum:"1"`
			AyahTo   int    `json:"ayah_to" minimum:"1"`
			Rating   string `json:"rating,omitempty" enum:"excellent,good,fair,weak"`
			Note     string `json:"note,omitempty" maxLength:"500"`
		}
	}) (*struct {
		Body struct {
			Entry HifzEntryView `json:"entry"`
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
		on, err := time.Parse(dateLayout, in.Body.OnDate)
		if err != nil {
			return nil, huma.Error422UnprocessableEntity("on_date must be a calendar date, YYYY-MM-DD.")
		}
		entry, err := svc.Log(ctx, p.TenantID, hifz.NewEntry{
			StudentID: studentID, OnDate: on, Kind: in.Body.Kind, Surah: in.Body.Surah,
			AyahFrom: in.Body.AyahFrom, AyahTo: in.Body.AyahTo, Rating: in.Body.Rating, Note: in.Body.Note,
		}, hifz.Author{UserID: p.UserID})
		if err != nil {
			return nil, hifzError(err)
		}
		out := &struct {
			Body struct {
				Entry HifzEntryView `json:"entry"`
			}
		}{}
		out.Body.Entry = hifzEntryView(entry)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "delete-hifz",
		Method:        http.MethodDelete,
		Path:          "/v1/hifz/{id}",
		Summary:       "Remove a hifz log entry",
		DefaultStatus: http.StatusNoContent,
		Tags:          []string{"Hifz"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*struct{}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		id, err := parseUUID(in.ID, "hifz entry")
		if err != nil {
			return nil, err
		}
		if err := svc.Delete(ctx, p.TenantID, id); err != nil {
			return nil, hifzError(err)
		}
		return &struct{}{}, nil
	})
}

// hifzError maps the hifz package's sentinels onto status codes.
func hifzError(err error) error {
	switch {
	case errors.Is(err, hifz.ErrNotFound):
		return huma.Error404NotFound("Not found.")
	case errors.Is(err, hifz.ErrInvalidPage):
		return huma.Error422UnprocessableEntity("That page cursor is not valid.")
	case errors.Is(err, hifz.ErrInvalidEntry):
		return huma.Error422UnprocessableEntity(err.Error())
	default:
		return err
	}
}
