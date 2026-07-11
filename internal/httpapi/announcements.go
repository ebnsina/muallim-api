package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/ebnsina/lms-api/internal/auth"
	"github.com/ebnsina/lms-api/internal/catalog"
)

// AnnouncementView is a notice as a client reads it.
type AnnouncementView struct {
	ID        string    `json:"id" format:"uuid"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

type AnnouncementsOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Announcements []AnnouncementView `json:"announcements"`
	}
}

type AnnouncementOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Announcement AnnouncementView `json:"announcement"`
	}
}

func announcementView(a catalog.Announcement) AnnouncementView {
	return AnnouncementView{ID: a.ID.String(), Title: a.Title, Body: a.Body, CreatedAt: a.CreatedAt}
}

// registerAnnouncements wires a course's notice board: an instructor posts and
// removes, and whoever may see the course reads.
func registerAnnouncements(api huma.API, svc *catalog.Service) {
	huma.Register(api, huma.Operation{
		OperationID: "list-announcements",
		Method:      http.MethodGet,
		Path:        "/v1/courses/{slug}/announcements",
		Summary:     "A course's announcements, newest first",
		Description: "Readable by whoever may see the course: a learner on a published one, the author " +
			"on a draft. The reader's permission decides which, never a query parameter.",
		Tags:     []string{"Catalog"},
		Security: []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		Slug string `path:"slug" maxLength:"100"`
	}) (*AnnouncementsOutput, error) {
		p, err := requirePrincipal(ctx)
		if err != nil {
			return nil, err
		}

		// An author sees a draft course's board; a learner sees only a published
		// course's — the same rule the course itself is shown by.
		announcements, err := svc.Announcements(ctx, p.TenantID, in.Slug, p.Can(auth.PermCourseWrite))
		if err != nil {
			return nil, catalogError(err)
		}

		out := &AnnouncementsOutput{CacheControl: "private, no-store"}
		out.Body.Announcements = make([]AnnouncementView, 0, len(announcements))
		for _, a := range announcements {
			out.Body.Announcements = append(out.Body.Announcements, announcementView(a))
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "post-announcement",
		Method:        http.MethodPost,
		Path:          "/v1/courses/{slug}/announcements",
		Summary:       "Post an announcement to a course",
		DefaultStatus: http.StatusCreated,
		Tags:          []string{"Catalog"},
		Security:      []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		Slug string `path:"slug" maxLength:"100"`
		Body struct {
			Title string `json:"title" minLength:"1" maxLength:"200"`
			Body  string `json:"body" minLength:"1" maxLength:"5000"`
		}
	}) (*AnnouncementOutput, error) {
		p, err := requirePermission(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}

		created, err := svc.PostAnnouncement(ctx, p.TenantID, in.Slug, p.UserID, in.Body.Title, in.Body.Body)
		if err != nil {
			return nil, catalogError(err)
		}

		out := &AnnouncementOutput{CacheControl: "private, no-store"}
		out.Body.Announcement = announcementView(created)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "delete-announcement",
		Method:        http.MethodDelete,
		Path:          "/v1/announcements/{id}",
		Summary:       "Remove an announcement",
		DefaultStatus: http.StatusNoContent,
		Tags:          []string{"Catalog"},
		Security:      []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID uuid.UUID `path:"id" format:"uuid"`
	}) (*struct{}, error) {
		p, err := requirePermission(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}

		if err := svc.DeleteAnnouncement(ctx, p.TenantID, in.ID); err != nil {
			return nil, catalogError(err)
		}
		return nil, nil
	})
}
