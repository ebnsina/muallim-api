package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ebnsina/muallim-api/internal/auth"
	"github.com/ebnsina/muallim-api/internal/catalog"
	"github.com/ebnsina/muallim-api/internal/enroll"
	"github.com/ebnsina/muallim-api/internal/liveclass"
)

// A live session is addressed under its course to schedule and list, and by its own
// id to change or remove. Who may list a course's sessions is the course's own
// access model: an instructor who may write it, or a learner enrolled on it. A
// reader who is neither gets 404, never 403 — the same rule that hides an
// unpublished course.

// LiveSessionView is a scheduled live class as anybody who may read the course sees
// it — the meeting link included, because seeing the session is what lets you join.
type LiveSessionView struct {
	ID          string     `json:"id" format:"uuid"`
	CourseID    string     `json:"course_id" format:"uuid"`
	Title       string     `json:"title"`
	Description string     `json:"description,omitempty"`
	JoinURL     string     `json:"join_url,omitempty" format:"uri"`
	StartsAt    time.Time  `json:"starts_at"`
	EndsAt      *time.Time `json:"ends_at,omitempty"`
	HostUserID  string     `json:"host_user_id,omitempty" format:"uuid"`
}

func liveSessionView(s liveclass.Session) LiveSessionView {
	v := LiveSessionView{
		ID: s.ID.String(), CourseID: s.CourseID.String(), Title: s.Title,
		Description: s.Description, JoinURL: s.JoinURL, StartsAt: s.StartsAt, EndsAt: s.EndsAt,
	}
	if s.HostUserID != nil {
		v.HostUserID = s.HostUserID.String()
	}
	return v
}

func registerLiveSessions(api huma.API, svc *liveclass.Service, courses *catalog.Service, learning *enroll.Service) {
	secured := []map[string][]string{{"bearer": {}}}

	huma.Register(api, huma.Operation{
		OperationID:   "create-live-session",
		Method:        http.MethodPost,
		Path:          "/v1/courses/{slug}/live-sessions",
		Summary:       "Schedule a live session on a course",
		DefaultStatus: http.StatusCreated,
		Description: "Bring-your-own-link: paste a Zoom/Meet/Jitsi URL and enrolled learners see it and " +
			"join. Requires course:write; the host is you.",
		Tags:     []string{"Live Sessions"},
		Security: secured,
	}, func(ctx context.Context, in *struct {
		Slug string `path:"slug"`
		Body struct {
			Title       string     `json:"title" minLength:"1" maxLength:"200"`
			Description string     `json:"description,omitempty" maxLength:"4000"`
			JoinURL     string     `json:"join_url,omitempty" format:"uri" maxLength:"2048"`
			StartsAt    time.Time  `json:"starts_at"`
			EndsAt      *time.Time `json:"ends_at,omitempty"`
		}
	}) (*struct {
		Body struct {
			Session LiveSessionView `json:"session"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}
		course, err := courses.Curriculum(ctx, p.TenantID, in.Slug, true)
		if err != nil {
			return nil, liveClassError(err)
		}

		host := p.UserID
		session, err := svc.Create(ctx, p.TenantID, course.Course.ID, liveclass.NewSession{
			Title: in.Body.Title, Description: in.Body.Description, JoinURL: in.Body.JoinURL,
			StartsAt: in.Body.StartsAt, EndsAt: in.Body.EndsAt, HostUserID: &host,
		}, liveclass.Author{UserID: p.UserID})
		if err != nil {
			return nil, liveClassError(err)
		}

		out := &struct {
			Body struct {
				Session LiveSessionView `json:"session"`
			}
		}{}
		out.Body.Session = liveSessionView(session)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-live-sessions",
		Method:      http.MethodGet,
		Path:        "/v1/courses/{slug}/live-sessions",
		Summary:     "A course's live sessions, soonest first",
		Description: "Readable by anybody who may write the course, or who is enrolled on it. A reader who " +
			"is neither receives 404. Keyset-paginated, newest start first.",
		Tags:     []string{"Live Sessions"},
		Security: secured,
	}, func(ctx context.Context, in *struct {
		Slug   string `path:"slug"`
		Limit  int    `query:"limit" minimum:"1" maximum:"100" default:"50"`
		Cursor string `query:"cursor"`
	}) (*struct {
		CacheControl string `header:"Cache-Control"`
		Body         struct {
			Sessions   []LiveSessionView `json:"sessions"`
			NextCursor string            `json:"next_cursor,omitempty"`
			HasMore    bool              `json:"has_more"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermCourseRead)
		if err != nil {
			return nil, err
		}
		course, err := courses.Curriculum(ctx, p.TenantID, in.Slug, true)
		if err != nil {
			return nil, liveClassError(err)
		}

		// The access gate: an author who may write the course, or a learner enrolled
		// on it. Anyone else is told the course has no such thing to list — 404, not
		// 403, so the answer does not leak who is enrolled where.
		if !auth.Can(p.Role, auth.PermCourseWrite) {
			enrolled, err := learning.IsEnrolled(ctx, p.TenantID, course.Course.ID, p.UserID)
			if err != nil {
				return nil, liveClassError(err)
			}
			if !enrolled {
				return nil, huma.Error404NotFound("We couldn't find that live class.")
			}
		}

		page, err := svc.ListForCourse(ctx, p.TenantID, course.Course.ID,
			liveclass.PageParams{Limit: in.Limit, Cursor: in.Cursor})
		if err != nil {
			return nil, liveClassError(err)
		}

		out := &struct {
			CacheControl string `header:"Cache-Control"`
			Body         struct {
				Sessions   []LiveSessionView `json:"sessions"`
				NextCursor string            `json:"next_cursor,omitempty"`
				HasMore    bool              `json:"has_more"`
			}
		}{CacheControl: lessonCacheControl}
		out.Body.Sessions = make([]LiveSessionView, 0, len(page.Sessions))
		for _, s := range page.Sessions {
			out.Body.Sessions = append(out.Body.Sessions, liveSessionView(s))
		}
		out.Body.NextCursor = page.NextCursor
		out.Body.HasMore = page.HasMore
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "update-live-session",
		Method:      http.MethodPatch,
		Path:        "/v1/live-sessions/{id}",
		Summary:     "Change a live session",
		Description: "An omitted field is left alone; a null join_url or ends_at erases it. Requires course:write.",
		Tags:        []string{"Live Sessions"},
		Security:    secured,
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id" format:"uuid"`
		Body struct {
			Title       *string             `json:"title,omitempty" minLength:"1" maxLength:"200"`
			Description *string             `json:"description,omitempty" maxLength:"4000"`
			StartsAt    *time.Time          `json:"starts_at,omitempty"`
			JoinURL     Optional[string]    `json:"join_url,omitempty" doc:"A URL, or null to remove the link."`
			EndsAt      Optional[time.Time] `json:"ends_at,omitempty" doc:"An instant, or null to remove the end time."`
		}
	}) (*struct {
		Body struct {
			Session LiveSessionView `json:"session"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}
		id, err := parseUUID(in.ID, "live session")
		if err != nil {
			return nil, err
		}

		patch := liveclass.SessionPatch{
			Title: in.Body.Title, Description: in.Body.Description, StartsAt: in.Body.StartsAt,
		}
		if in.Body.JoinURL.Sent {
			if in.Body.JoinURL.Null {
				patch.ClearJoinURL = true
			} else {
				patch.JoinURL = &in.Body.JoinURL.Value
			}
		}
		if in.Body.EndsAt.Sent {
			if in.Body.EndsAt.Null {
				patch.ClearEndsAt = true
			} else {
				patch.EndsAt = &in.Body.EndsAt.Value
			}
		}

		session, err := svc.Update(ctx, p.TenantID, id, patch, liveclass.Author{UserID: p.UserID})
		if err != nil {
			return nil, liveClassError(err)
		}

		out := &struct {
			Body struct {
				Session LiveSessionView `json:"session"`
			}
		}{}
		out.Body.Session = liveSessionView(session)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "delete-live-session",
		Method:        http.MethodDelete,
		Path:          "/v1/live-sessions/{id}",
		Summary:       "Remove a live session",
		DefaultStatus: http.StatusNoContent,
		Tags:          []string{"Live Sessions"},
		Security:      secured,
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*struct{}, error) {
		p, err := requirePermission(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}
		id, err := parseUUID(in.ID, "live session")
		if err != nil {
			return nil, err
		}
		if err := svc.Delete(ctx, p.TenantID, id, liveclass.Author{UserID: p.UserID}); err != nil {
			return nil, liveClassError(err)
		}
		return &struct{}{}, nil
	})
}

// liveClassError maps the liveclass package's sentinels onto status codes. This is
// the only place that translation happens; the domain never imports net/http.
func liveClassError(err error) error {
	switch {
	case errors.Is(err, liveclass.ErrNotFound), errors.Is(err, catalog.ErrNotFound):
		// An unknown session, or an unknown course slug, are both a 404 — neither
		// admits existence to a caller who has none.
		return huma.Error404NotFound("We couldn't find that live class.")
	case errors.Is(err, liveclass.ErrInvalidSession):
		return huma.Error422UnprocessableEntity("Check the live class details and try again.")
	case errors.Is(err, liveclass.ErrInvalidPage):
		return huma.Error422UnprocessableEntity("That page link is no longer valid. Start from the first page.")
	default:
		return err
	}
}
