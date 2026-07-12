package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/ebnsina/muallim-api/internal/auth"
	"github.com/ebnsina/muallim-api/internal/catalog"
	"github.com/ebnsina/muallim-api/internal/tenant"
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

// draftCacheControl is what an author's view of an unpublished course must carry.
//
// A draft served with `public` caching is a draft a CDN will store and hand to
// strangers. `private, no-store` keeps it out of every cache between us and the
// author's browser.
const draftCacheControl = "private, no-store"

// CourseSummary is a course as it appears in a list.
type CourseSummary struct {
	ID          string     `json:"id" format:"uuid"`
	Slug        string     `json:"slug"`
	Title       string     `json:"title"`
	Summary     string     `json:"summary"`
	Difficulty  string     `json:"difficulty" enum:"beginner,intermediate,advanced,expert"`
	Status      string     `json:"status" enum:"draft,published,archived"`
	PublishedAt *time.Time `json:"published_at,omitempty"`

	// DripMode decides how the course releases its lessons.
	DripMode string `json:"drip_mode" enum:"none,scheduled,after_enrolment,sequential"`

	// LessonCount is how many lessons the course holds, for a listing that wants to
	// say so without loading each curriculum.
	LessonCount int `json:"lesson_count"`
}

// CourseDetail is a course as it appears on its own page: the summary, plus the
// copy a listing has no use for and would pay for by the row.
type CourseDetail struct {
	CourseSummary

	Description  string   `json:"description"`
	Objectives   []string `json:"objectives"`
	Requirements []string `json:"requirements"`
	Language     string   `json:"language"`

	// Instructor is the author's display name, empty for a course drafted before
	// the column existed or by an account since erased.
	Instructor string `json:"instructor"`

	// LearnerCount counts the active and completed enrolments — the people studying
	// it and the people who finished, which is what "350,392 learners" means.
	LearnerCount int `json:"learner_count"`

	UpdatedAt time.Time `json:"updated_at"`
}

// LessonView is a lesson within a curriculum.
type LessonView struct {
	ID              string `json:"id" format:"uuid"`
	Title           string `json:"title"`
	ContentType     string `json:"content_type" enum:"text,video,quiz,assignment,live,scorm,h5p"`
	DurationSeconds int    `json:"duration_seconds"`
	IsPreview       bool   `json:"is_preview"`
	Position        int    `json:"position"`

	// The drip schedule as stored. Read against the course's drip_mode: neither
	// means anything on its own.
	AvailableAt        *time.Time `json:"available_at,omitempty"`
	AvailableAfterDays *int       `json:"available_after_days,omitempty"`
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
		Course CourseDetail `json:"course"`
		Topics []TopicView  `json:"topics"`

		LessonCount     int `json:"lesson_count"`
		DurationSeconds int `json:"duration_seconds"`
	}
}

// learnerCounter counts a course's enrolled and finished learners. Declared here,
// by its consumer: the course page shows the number, and enrol owns it.
type learnerCounter interface {
	EnrolmentCount(ctx context.Context, tenantID uuid.UUID, slug string) (int, error)
}

func registerCatalog(api huma.API, svc *catalog.Service, learners learnerCounter) {
	huma.Register(api, huma.Operation{
		OperationID: "list-courses",
		Method:      http.MethodGet,
		Path:        "/v1/courses",
		Summary:     "List published courses",
		Description: "Returns one keyset-paginated page of the tenant's published courses. " +
			"There is no total count: counting matching rows costs a full scan on every request.",
		Tags: []string{"Catalog"},
	}, func(ctx context.Context, in *struct {
		Limit      int    `query:"limit" minimum:"1" maximum:"100" default:"20" doc:"Page size"`
		Cursor     string `query:"cursor" maxLength:"128" doc:"Opaque cursor from a previous page"`
		Q          string `query:"q" maxLength:"200" doc:"Filter to courses whose title contains this"`
		Difficulty string `query:"difficulty" maxLength:"20" doc:"Filter to one of: beginner, intermediate, advanced, expert"`
	}) (*ListCoursesOutput, error) {
		// IncludeDrafts is left false. This route is anonymous, and there is no
		// query parameter that could set it.
		page, err := svc.ListCourses(ctx, tenant.ID(ctx), catalog.ListParams{
			Limit:      in.Limit,
			Cursor:     in.Cursor,
			Search:     in.Q,
			Difficulty: in.Difficulty,
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
		OperationID: "list-authored-courses",
		Method:      http.MethodGet,
		Path:        "/v1/me/courses",
		Summary:     "List every course in this workspace, drafts included",
		Description: "Requires course:write. A separate route rather than a status filter on the public " +
			"listing: what an author may see follows from their permission, never from a query parameter.",
		Tags:     []string{"Catalog"},
		Security: []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		Limit      int    `query:"limit" minimum:"1" maximum:"100" default:"20" doc:"Page size"`
		Cursor     string `query:"cursor" maxLength:"128" doc:"Opaque cursor from a previous page"`
		Q          string `query:"q" maxLength:"200" doc:"Filter to courses whose title contains this"`
		Difficulty string `query:"difficulty" maxLength:"20" doc:"Filter to one of: beginner, intermediate, advanced, expert"`
	}) (*ListCoursesOutput, error) {
		if _, err := requirePermission(ctx, auth.PermCourseWrite); err != nil {
			return nil, err
		}

		page, err := svc.ListCourses(ctx, tenant.ID(ctx), catalog.ListParams{
			Limit:         in.Limit,
			Cursor:        in.Cursor,
			Search:        in.Q,
			Difficulty:    in.Difficulty,
			IncludeDrafts: true,
		})
		if err != nil {
			return nil, catalogError(err)
		}

		// Drafts must never reach a shared cache, whoever asked for them.
		out := &ListCoursesOutput{CacheControl: draftCacheControl}
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
		// Only somebody who may edit courses may see one that is not published.
		// Everyone else gets ErrNotFound — the same answer as for a course that does
		// not exist, because "this exists but you may not see it" is a fact about
		// the workspace's plans that strangers have no business learning.
		p, isAuthor := principalFrom(ctx)
		canSeeDrafts := isAuthor && p.Can(auth.PermCourseWrite)

		curriculum, err := svc.Curriculum(ctx, tenant.ID(ctx), in.Slug, canSeeDrafts)
		if err != nil {
			return nil, catalogError(err)
		}

		// A draft must never be cached by anything shared. Deciding this from the
		// course's own status, rather than from who asked, means an author fetching a
		// published course still gets the fast public path.
		cacheControl := catalogCacheControl
		if curriculum.Course.Status != catalog.StatusPublished {
			cacheControl = draftCacheControl
		}

		// One more query, not one per lesson. A draft has nobody enrolled and nobody
		// to show a count to, so it does not pay for one.
		var learnerCount int
		if curriculum.Course.Status == catalog.StatusPublished {
			learnerCount, err = learners.EnrolmentCount(ctx, tenant.ID(ctx), in.Slug)
			if err != nil {
				return nil, catalogError(err)
			}
		}

		out := &CurriculumOutput{CacheControl: cacheControl}
		out.Body.Course = courseDetail(curriculum.Course, learnerCount)
		out.Body.Topics = make([]TopicView, 0, len(curriculum.Topics))
		for _, t := range curriculum.Topics {
			lessons := make([]LessonView, 0, len(t.Lessons))
			for _, l := range t.Lessons {
				lessons = append(lessons, LessonView{
					ID:                 l.ID.String(),
					Title:              l.Title,
					ContentType:        l.ContentType,
					DurationSeconds:    l.DurationSeconds,
					IsPreview:          l.IsPreview,
					AvailableAt:        l.AvailableAt,
					AvailableAfterDays: l.AvailableAfterDays,
					Position:           l.Position,
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

// CreateCourseOutput is a newly drafted course.
type CreateCourseOutput struct {
	Body struct {
		Course CourseSummary `json:"course"`
	}
}

// UpdateCourseOutput is a course after its copy was rewritten.
type UpdateCourseOutput struct {
	Body struct {
		Course CourseDetail `json:"course"`
	}
}

func registerCourseCopy(api huma.API, svc *catalog.Service) {
	huma.Register(api, huma.Operation{
		OperationID: "update-course",
		Method:      http.MethodPatch,
		Path:        "/v1/courses/{slug}",
		Summary:     "Edit a course's copy",
		Description: "Requires course:write. Rewrites what a course says about itself: its pitch, " +
			"what a learner will be able to do, and what it asks of them first. " +
			"An omitted field is left alone; an empty list clears the list.",
		Tags:     []string{"Authoring"},
		Security: []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		Slug string `path:"slug" maxLength:"200"`
		Body struct {
			// Pointers: an omitted field is left alone rather than cleared.
			Title        *string   `json:"title,omitempty" minLength:"1" maxLength:"200"`
			Summary      *string   `json:"summary,omitempty" maxLength:"1000"`
			Description  *string   `json:"description,omitempty" maxLength:"20000" doc:"The long pitch. Plain text; newlines are kept."`
			Difficulty   *string   `json:"difficulty,omitempty" enum:"beginner,intermediate,advanced,expert"`
			Language     *string   `json:"language,omitempty" maxLength:"20" doc:"The language it is taught in, as a tag: en, bn, ar."`
			Objectives   *[]string `json:"objectives,omitempty" maxItems:"20" doc:"What a learner will be able to do. Shown as \"what you'll learn\"."`
			Requirements *[]string `json:"requirements,omitempty" maxItems:"20" doc:"What a learner needs before starting."`
		}
	}) (*UpdateCourseOutput, error) {
		p, author, err := authorFor(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}

		course, err := svc.EditCourse(ctx, p.TenantID, in.Slug, catalog.CoursePatch{
			Title:        in.Body.Title,
			Summary:      in.Body.Summary,
			Description:  in.Body.Description,
			Difficulty:   in.Body.Difficulty,
			Language:     in.Body.Language,
			Objectives:   in.Body.Objectives,
			Requirements: in.Body.Requirements,
		}, author)
		if err != nil {
			return nil, catalogError(err)
		}

		// The learner count is not this endpoint's business, and an author editing
		// copy has not changed it. Zero here rather than a query nobody asked for.
		out := &UpdateCourseOutput{}
		out.Body.Course = courseDetail(course, 0)
		return out, nil
	})
}

func registerCatalogWrites(api huma.API, svc *catalog.Service) {
	huma.Register(api, huma.Operation{
		OperationID: "create-course",
		Method:      http.MethodPost,
		Path:        "/v1/courses",
		Summary:     "Draft a new course",
		Description: "Requires the course:write permission. The course is created as a draft; " +
			"publishing is a separately authorised act.",
		Tags:          []string{"Catalog"},
		DefaultStatus: http.StatusCreated,
		Security:      []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		Body struct {
			Slug  string `json:"slug" minLength:"1" maxLength:"200" pattern:"^[a-z0-9]+(?:-[a-z0-9]+)*$" doc:"Lowercase letters, digits, hyphens. Unique within the workspace."`
			Title string `json:"title" minLength:"1" maxLength:"300"`

			// Optional. Huma treats a field as required unless the json tag says
			// otherwise, and a required `summary` would make schema validation reject
			// a request before authorisation ever ran — answering 422 where the caller
			// deserves 401.
			Summary    string `json:"summary,omitempty" maxLength:"2000"`
			Difficulty string `json:"difficulty,omitempty" enum:"beginner,intermediate,advanced,expert" default:"beginner"`
		}
	}) (*CreateCourseOutput, error) {
		// Authorisation happens here, where the operation's requirement is known.
		// Middleware established who the caller is; only this line knows that
		// drafting a course needs course:write.
		p, err := requirePermission(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}

		rc := requestContextFrom(ctx)
		course, err := svc.CreateCourse(ctx, p.TenantID, catalog.NewCourse{
			Slug:       in.Body.Slug,
			Title:      in.Body.Title,
			Summary:    in.Body.Summary,
			Difficulty: in.Body.Difficulty,
		}, catalog.Author{UserID: p.UserID, IP: rc.IP, UserAgent: rc.UserAgent})
		if err != nil {
			return nil, catalogError(err)
		}

		out := &CreateCourseOutput{}
		out.Body.Course = courseSummary(course)
		return out, nil
	})
}

// courseDetail maps a course to its page view. The slices are never nil: `[]` and
// `null` are the same emptiness to a client that has to branch on it anyway.
func courseDetail(c catalog.Course, learnerCount int) CourseDetail {
	objectives, requirements := c.Objectives, c.Requirements
	if objectives == nil {
		objectives = []string{}
	}
	if requirements == nil {
		requirements = []string{}
	}

	return CourseDetail{
		CourseSummary: courseSummary(c),
		Description:   c.Description,
		Objectives:    objectives,
		Requirements:  requirements,
		Language:      c.Language,
		Instructor:    c.InstructorName,
		LearnerCount:  learnerCount,
		UpdatedAt:     c.UpdatedAt,
	}
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
		DripMode:    c.DripMode,
		LessonCount: c.LessonCount,
	}
}

// catalogError maps the catalog package's sentinels onto status codes. This is
// the only place that translation happens; the domain never imports net/http.
func catalogError(err error) error {
	switch {
	case errors.Is(err, catalog.ErrNotFound):
		return huma.Error404NotFound("Course not found.")

	case errors.Is(err, catalog.ErrInvalidCourse), errors.Is(err, catalog.ErrInvalidDifficulty):
		return huma.Error422UnprocessableEntity(err.Error())

	case errors.Is(err, catalog.ErrPrerequisiteCycle):
		// 422: the request was understood and is impossible. A course that requires
		// itself, however indirectly, is a course nobody can ever start.
		return huma.Error422UnprocessableEntity(err.Error())

	case errors.Is(err, catalog.ErrPrerequisiteExists):
		return huma.Error409Conflict("That course is already a prerequisite.")

	case errors.Is(err, catalog.ErrInvalidDripMode):
		return huma.Error422UnprocessableEntity(err.Error())
	case errors.Is(err, catalog.ErrInvalidPage):
		return huma.Error422UnprocessableEntity("The cursor is not valid.")
	case errors.Is(err, catalog.ErrInvalidLimit), errors.Is(err, catalog.ErrInvalidSlug):
		return huma.Error422UnprocessableEntity(err.Error())
	case errors.Is(err, catalog.ErrSlugTaken):
		return huma.Error409Conflict("A course with that slug already exists in this workspace.")

	case errors.Is(err, catalog.ErrInvalidLesson):
		return huma.Error422UnprocessableEntity(err.Error())

	case errors.Is(err, catalog.ErrInvalidAnnouncement):
		return huma.Error422UnprocessableEntity("An announcement needs a title and a body.")

	case errors.Is(err, catalog.ErrInvalidVideo):
		// 422, and the sentence names the URL's problem: this is the one lesson field
		// whose validity depends on how the workspace is configured, so an author who
		// is refused deserves to know it was the video and why.
		return huma.Error422UnprocessableEntity(err.Error())

	case errors.Is(err, catalog.ErrIncompleteOrder):
		return huma.Error422UnprocessableEntity(err.Error())

	case errors.Is(err, catalog.ErrEmptyCourse):
		return huma.Error409Conflict("A course needs at least one lesson before it can be published.")

	case errors.Is(err, catalog.ErrAlreadyPublished):
		return huma.Error409Conflict("That course is already published.")
	default:
		// Anything unexpected: the wrapped cause is logged with a correlation ID by
		// the recovery and access-log middleware; the client learns nothing more.
		return err
	}
}
