package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ebnsina/muallim-api/internal/auth"
	"github.com/ebnsina/muallim-api/internal/calendar"
)

// EventView is one entry on the academic calendar.
type EventView struct {
	ID          string `json:"id" format:"uuid"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Kind        string `json:"kind" enum:"holiday,exam,event,term_start,term_end"`
	StartsOn    string `json:"starts_on" format:"date"`
	EndsOn      string `json:"ends_on,omitempty" format:"date"`
	CreatedAt   string `json:"created_at" format:"date-time"`
}

func eventView(e calendar.Event) EventView {
	v := EventView{
		ID: e.ID.String(), Title: e.Title, Kind: e.Kind,
		StartsOn:  e.StartsOn.Format(dateLayout),
		CreatedAt: e.CreatedAt.Format(time.RFC3339),
	}
	if e.Description != nil {
		v.Description = *e.Description
	}
	if e.EndsOn != nil {
		v.EndsOn = e.EndsOn.Format(dateLayout)
	}
	return v
}

func registerCalendar(api huma.API, svc *calendar.Service) {
	admin := []map[string][]string{{"bearer": {}}}

	huma.Register(api, huma.Operation{
		OperationID: "list-calendar-events",
		Method:      http.MethodGet,
		Path:        "/v1/calendar/events",
		Summary:     "The academic calendar, newest first",
		Description: "Keyset-paginated. Filter by kind and by a start-date window (from/to).",
		Tags:        []string{"Calendar"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		Kind   string `query:"kind" enum:"holiday,exam,event,term_start,term_end"`
		From   string `query:"from" format:"date"`
		To     string `query:"to" format:"date"`
		Limit  int    `query:"limit" minimum:"1" maximum:"100" default:"50"`
		Cursor string `query:"cursor"`
	}) (*struct {
		Body struct {
			Events     []EventView `json:"events"`
			NextCursor string      `json:"next_cursor,omitempty"`
			HasMore    bool        `json:"has_more"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		from, err := optionalDate(in.From)
		if err != nil {
			return nil, err
		}
		to, err := optionalDate(in.To)
		if err != nil {
			return nil, err
		}
		page, err := svc.ListEvents(ctx, p.TenantID,
			calendar.EventFilter{Kind: in.Kind, From: from, To: to},
			calendar.PageParams{Limit: in.Limit, Cursor: in.Cursor})
		if err != nil {
			return nil, calendarError(err)
		}
		out := &struct {
			Body struct {
				Events     []EventView `json:"events"`
				NextCursor string      `json:"next_cursor,omitempty"`
				HasMore    bool        `json:"has_more"`
			}
		}{}
		out.Body.Events = make([]EventView, 0, len(page.Events))
		for _, e := range page.Events {
			out.Body.Events = append(out.Body.Events, eventView(e))
		}
		out.Body.NextCursor = page.NextCursor
		out.Body.HasMore = page.HasMore
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "create-calendar-event",
		Method:        http.MethodPost,
		Path:          "/v1/calendar/events",
		Summary:       "Add an event to the calendar",
		DefaultStatus: http.StatusCreated,
		Tags:          []string{"Calendar"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		Body struct {
			Title       string `json:"title" minLength:"1" maxLength:"200"`
			Description string `json:"description,omitempty" maxLength:"2000"`
			Kind        string `json:"kind,omitempty" enum:"holiday,exam,event,term_start,term_end" default:"event"`
			StartsOn    string `json:"starts_on" format:"date"`
			EndsOn      string `json:"ends_on,omitempty" format:"date"`
		}
	}) (*struct {
		Body struct {
			Event EventView `json:"event"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		starts, err := optionalDate(in.Body.StartsOn)
		if err != nil {
			return nil, err
		}
		if starts == nil {
			return nil, huma.Error422UnprocessableEntity("A start date is required.")
		}
		ends, err := optionalDate(in.Body.EndsOn)
		if err != nil {
			return nil, err
		}
		ev, err := svc.CreateEvent(ctx, p.TenantID, calendar.NewEvent{
			Title: in.Body.Title, Description: optionalStringPtr(in.Body.Description),
			Kind: in.Body.Kind, StartsOn: *starts, EndsOn: ends,
		}, calendar.Author{UserID: p.UserID})
		if err != nil {
			return nil, calendarError(err)
		}
		out := &struct {
			Body struct {
				Event EventView `json:"event"`
			}
		}{}
		out.Body.Event = eventView(ev)
		return out, nil
	})

	update := func(ctx context.Context, in *struct {
		ID   string `path:"id" format:"uuid"`
		Body struct {
			Title       *string `json:"title,omitempty" maxLength:"200"`
			Description *string `json:"description,omitempty" maxLength:"2000"`
			Kind        *string `json:"kind,omitempty" enum:"holiday,exam,event,term_start,term_end"`
			StartsOn    *string `json:"starts_on,omitempty" format:"date"`
			EndsOn      *string `json:"ends_on,omitempty" format:"date"`
		}
	}) (*struct {
		Body struct {
			Event EventView `json:"event"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		id, err := parseUUID(in.ID, "event")
		if err != nil {
			return nil, err
		}
		patch := calendar.EventPatch{
			Title: in.Body.Title, Description: in.Body.Description, Kind: in.Body.Kind,
		}
		if in.Body.StartsOn != nil {
			starts, err := optionalDate(*in.Body.StartsOn)
			if err != nil {
				return nil, err
			}
			patch.StartsOn = starts
		}
		if in.Body.EndsOn != nil {
			ends, err := optionalDate(*in.Body.EndsOn)
			if err != nil {
				return nil, err
			}
			patch.EndsOn = ends
		}
		ev, err := svc.UpdateEvent(ctx, p.TenantID, id, patch)
		if err != nil {
			return nil, calendarError(err)
		}
		out := &struct {
			Body struct {
				Event EventView `json:"event"`
			}
		}{}
		out.Body.Event = eventView(ev)
		return out, nil
	}

	huma.Register(api, huma.Operation{
		OperationID: "update-calendar-event",
		Method:      http.MethodPatch,
		Path:        "/v1/calendar/events/{id}",
		Summary:     "Edit a calendar event",
		Tags:        []string{"Calendar"},
		Security:    admin,
	}, update)

	huma.Register(api, huma.Operation{
		OperationID: "replace-calendar-event",
		Method:      http.MethodPut,
		Path:        "/v1/calendar/events/{id}",
		Summary:     "Edit a calendar event (PUT alias)",
		Tags:        []string{"Calendar"},
		Security:    admin,
	}, update)

	huma.Register(api, huma.Operation{
		OperationID:   "delete-calendar-event",
		Method:        http.MethodDelete,
		Path:          "/v1/calendar/events/{id}",
		Summary:       "Remove a calendar event",
		DefaultStatus: http.StatusNoContent,
		Tags:          []string{"Calendar"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*struct{}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		id, err := parseUUID(in.ID, "event")
		if err != nil {
			return nil, err
		}
		if err := svc.DeleteEvent(ctx, p.TenantID, id); err != nil {
			return nil, calendarError(err)
		}
		return &struct{}{}, nil
	})
}

// calendarError maps the calendar package's sentinels onto status codes.
func calendarError(err error) error {
	switch {
	case errors.Is(err, calendar.ErrNotFound):
		return huma.Error404NotFound("We couldn't find that event.")
	case errors.Is(err, calendar.ErrInvalidPage):
		return huma.Error422UnprocessableEntity("That page link is no longer valid. Start from the first page.")
	case errors.Is(err, calendar.ErrInvalidEvent):
		return huma.Error422UnprocessableEntity("Check the event details and try again.")
	default:
		return err
	}
}
