package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ebnsina/lms-api/internal/catalog"
	"github.com/ebnsina/lms-api/internal/tenant"
)

// catalogCacheControl lets shared caches and browsers hold a published catalog
// briefly, and serve it stale for five minutes while revalidating in the
// background.
//
// `public` is safe here and only here: the published catalog contains no
// user-specific data, and a shared cache keys on the absolute URL, whose host
// identifies the tenant. Any endpoint that reflects who is asking must be
// `private`.
const catalogCacheControl = "public, max-age=60, stale-while-revalidate=300"

// CourseSummary is a course as it appears in a list.
type CourseSummary struct {
	ID          string     `json:"id" format:"uuid"`
	Slug        string     `json:"slug"`
	Title       string     `json:"title"`
	Summary     string     `json:"summary"`
	Difficulty  string     `json:"difficulty" enum:"beginner,intermediate,advanced,expert"`
	Status      string     `json:"status" enum:"draft,published,archived"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
}

// LessonView is a lesson within a curriculum.
type LessonView struct {
	ID              string `json:"id" format:"uuid"`
	Title           string `json:"title"`
	ContentType     string `json:"content_type" enum:"text,video,quiz,assignment,live,scorm,h5p"`
	DurationSeconds int    `json:"duration_seconds"`
	IsPreview       bool   `json:"is_preview"`
	Position        int    `json:"position"`
}

// TopicView is a topic with its lessons.
type TopicView struct {
	ID       string       `json:"id" format:"uuid"`
	Title    string       `json:"title"`
	Position int          `json:"position"`
	Lessons  []LessonView `json:"lessons"`
}

// ListCoursesOutput is a keyset page of courses.
type ListCoursesOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Courses []CourseSummary `json:"courses"`

		// NextCursor is opaque. Pass it back as `cursor` to fetch the next page;
		// it is absent on the last page. Do not parse it: the sort key it encodes
		// is an implementation detail we reserve the right to change.
		NextCursor string `json:"next_cursor,omitempty"`
		HasMore    bool   `json:"has_more"`
	}
}

// CurriculumOutput is a course with its full topic and lesson tree.
type CurriculumOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Course CourseSummary `json:"course"`
		Topics []TopicView   `json:"topics"`

		LessonCount     int `json:"lesson_count"`
		DurationSeconds int `json:"duration_seconds"`
	}
}

func registerCatalog(api huma.API, svc *catalog.Service) {
	huma.Register(api, huma.Operation{
		OperationID: "list-courses",
		Method:      http.MethodGet,
		Path:        "/v1/courses",
		Summary:     "List published courses",
		Description: "Returns one keyset-paginated page of the tenant's published courses. " +
			"There is no total count: counting matching rows costs a full scan on every request.",
		Tags: []string{"Catalog"},
	}, func(ctx context.Context, in *struct {
		Limit  int    `query:"limit" minimum:"1" maximum:"100" default:"20" doc:"Page size"`
		Cursor string `query:"cursor" maxLength:"128" doc:"Opaque cursor from a previous page"`
	}) (*ListCoursesOutput, error) {
		page, err := svc.ListCourses(ctx, tenant.ID(ctx), catalog.ListParams{
			Limit:  in.Limit,
			Cursor: in.Cursor,
		})
		if err != nil {
			return nil, catalogError(err)
		}

		out := &ListCoursesOutput{CacheControl: catalogCacheControl}
		out.Body.Courses = make([]CourseSummary, 0, len(page.Courses))
		for _, c := range page.Courses {
			out.Body.Courses = append(out.Body.Courses, courseSummary(c))
		}
		out.Body.NextCursor = page.NextCursor
		out.Body.HasMore = page.HasMore
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-course",
		Method:      http.MethodGet,
		Path:        "/v1/courses/{slug}",
		Summary:     "Get a course with its curriculum",
		Description: "Returns the course together with every topic and lesson. " +
			"Always three queries, regardless of the size of the course.",
		Tags: []string{"Catalog"},
	}, func(ctx context.Context, in *struct {
		Slug string `path:"slug" maxLength:"200" doc:"Course slug, unique within the tenant"`
	}) (*CurriculumOutput, error) {
		curriculum, err := svc.Curriculum(ctx, tenant.ID(ctx), in.Slug)
		if err != nil {
			return nil, catalogError(err)
		}

		out := &CurriculumOutput{CacheControl: catalogCacheControl}
		out.Body.Course = courseSummary(curriculum.Course)
		out.Body.Topics = make([]TopicView, 0, len(curriculum.Topics))
		for _, t := range curriculum.Topics {
			lessons := make([]LessonView, 0, len(t.Lessons))
			for _, l := range t.Lessons {
				lessons = append(lessons, LessonView{
					ID:              l.ID.String(),
					Title:           l.Title,
					ContentType:     l.ContentType,
					DurationSeconds: l.DurationSeconds,
					IsPreview:       l.IsPreview,
					Position:        l.Position,
				})
			}
			out.Body.Topics = append(out.Body.Topics, TopicView{
				ID:       t.ID.String(),
				Title:    t.Title,
				Position: t.Position,
				Lessons:  lessons,
			})
		}
		out.Body.LessonCount = curriculum.LessonCount()
		out.Body.DurationSeconds = int(curriculum.TotalDuration().Seconds())
		return out, nil
	})
}

func courseSummary(c catalog.Course) CourseSummary {
	return CourseSummary{
		ID:          c.ID.String(),
		Slug:        c.Slug,
		Title:       c.Title,
		Summary:     c.Summary,
		Difficulty:  c.Difficulty,
		Status:      c.Status,
		PublishedAt: c.PublishedAt,
	}
}

// catalogError maps the catalog package's sentinels onto status codes. This is
// the only place that translation happens; the domain never imports net/http.
func catalogError(err error) error {
	switch {
	case errors.Is(err, catalog.ErrNotFound):
		return huma.Error404NotFound("Course not found.")
	case errors.Is(err, catalog.ErrInvalidPage):
		return huma.Error422UnprocessableEntity("The cursor is not valid.")
	case errors.Is(err, catalog.ErrInvalidLimit):
		return huma.Error422UnprocessableEntity(err.Error())
	default:
		// Anything unexpected: the wrapped cause is logged with a correlation ID by
		// the recovery and access-log middleware; the client learns nothing more.
		return err
	}
}
