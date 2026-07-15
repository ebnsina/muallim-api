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
	"github.com/ebnsina/muallim-api/internal/commerce"
	"github.com/ebnsina/muallim-api/internal/enroll"
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

	// Instructor is the author's display name, empty for a course drafted before the
	// column existed or by an account since erased. InstructorID is who they are —
	// two people in a workspace may share a name, so a client that lists "everything
	// by this person" follows the id and never the name.
	Instructor   string `json:"instructor"`
	InstructorID string `json:"instructor_id,omitempty" format:"uuid"`

	// LearnerCount counts the active and completed enrolments — the people studying
	// it and the people who finished, which is what "350,392 learners" means.
	LearnerCount int `json:"learner_count,omitempty"`

	// The rating, and how many people gave it. Absent means *unrated* — nobody has
	// reviewed it, or nobody asked. A client must never draw an average with no
	// reviews behind it, so it is never sent as a bare nought.
	RatingAverage float64 `json:"rating_average,omitempty"`
	RatingCount   int     `json:"rating_count,omitempty"`

	// The price. Absent is free, which is what every course is until somebody prices
	// one — and what they all were before this API could take money at all.
	Price *MoneyView `json:"price,omitempty"`

	// ImageURL is a stable path to the course thumbnail, absent when there is none.
	// It is not the signed object URL — that rotates and would break this response's
	// ETag — but a redirect endpoint that signs one on demand, so a card and a page
	// stay cacheable while the bytes stay private.
	ImageURL string `json:"image_url,omitempty"`
}

// CourseDetail is a course as it appears on its own page: the summary, plus the
// copy a listing has no use for and would pay for by the row.
type CourseDetail struct {
	CourseSummary

	Description  string   `json:"description"`
	Objectives   []string `json:"objectives"`
	Requirements []string `json:"requirements"`
	Language     string   `json:"language"`

	// The preview clip. `preview_embed_url` is written by muallim-api from a
	// validated id and is the only one of these safe to put in an iframe; a client
	// that frames `preview_url` is framing whatever an author typed.
	PreviewSource   string `json:"preview_source" enum:"none,youtube,vimeo,embed,hosted"`
	PreviewURL      string `json:"preview_url,omitempty"`
	PreviewEmbedURL string `json:"preview_embed_url,omitempty" format:"uri"`

	// The instructor, the learner count, and the rating are on the summary: a
	// catalogue card wants them as much as a course page does, and a field that
	// exists twice is a field that will disagree with itself.

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

// courseFacts is what a catalogue knows about a course that the catalogue does not
// own: how many people are on it, and what they made of it. Declared here by its
// consumer; `enroll` holds both the enrolments and the reviews.
//
// Batched by id, because the listing draws twenty cards and a query per card is
// the N+1 this codebase does not write.
type courseFacts interface {
	Facts(ctx context.Context, tenantID uuid.UUID, courseIDs []uuid.UUID) (map[uuid.UUID]enroll.CourseFacts, error)
}

// coursePricing is what a course costs, batched by id. Nil when this deployment
// sells nothing, and then every course is free — which is what they all were.
type coursePricing interface {
	PricesOf(ctx context.Context, tenantID uuid.UUID, courseIDs []uuid.UUID) (map[uuid.UUID]commerce.Money, error)
}

// priceView renders a price, or nothing at all for a course that has none.
func priceView(m commerce.Money, ok bool) *MoneyView {
	if !ok {
		return nil
	}
	return &MoneyView{AmountMinor: m.AmountMinor, Currency: m.Currency}
}

/*
pricesFor loads the price of every course on a page.

One call for the page, and nil when nothing is for sale. A price per card would be
the N+1 this codebase does not write, and a deployment with no gateway pays for
nothing at all.
*/
func pricesFor(ctx context.Context, pricing coursePricing, courses []catalog.Course) (map[uuid.UUID]commerce.Money, error) {
	if pricing == nil || len(courses) == 0 {
		return map[uuid.UUID]commerce.Money{}, nil
	}

	ids := make([]uuid.UUID, len(courses))
	for i, c := range courses {
		ids[i] = c.ID
	}
	return pricing.PricesOf(ctx, tenant.ID(ctx), ids)
}

func registerCatalog(api huma.API, svc *catalog.Service, facts courseFacts, pricing coursePricing) {
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
		Author     string `query:"author" format:"uuid" doc:"Filter to the courses one person wrote, by their id"`
	}) (*ListCoursesOutput, error) {
		author, err := optionalUUID(in.Author)
		if err != nil {
			return nil, huma.Error422UnprocessableEntity("author must be a uuid.")
		}

		// IncludeDrafts is left false. This route is anonymous, and there is no
		// query parameter that could set it.
		page, err := svc.ListCourses(ctx, tenant.ID(ctx), catalog.ListParams{
			Limit:      in.Limit,
			Cursor:     in.Cursor,
			Search:     in.Q,
			Difficulty: in.Difficulty,
			Author:     author,
		})
		if err != nil {
			return nil, catalogError(err)
		}

		known, err := factsFor(ctx, facts, page.Courses)
		if err != nil {
			return nil, catalogError(err)
		}
		prices, err := pricesFor(ctx, pricing, page.Courses)
		if err != nil {
			return nil, catalogError(err)
		}

		out := &ListCoursesOutput{CacheControl: catalogCacheControl}
		out.Body.Courses = make([]CourseSummary, 0, len(page.Courses))
		for _, c := range page.Courses {
			summary := courseSummary(c, known[c.ID])
			price, sold := prices[c.ID]
			summary.Price = priceView(price, sold)
			out.Body.Courses = append(out.Body.Courses, summary)
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

		// An author's own listing gets the facts too: "how many are on it, what do
		// they think of it" is the first thing anybody asks about a course they wrote.
		known, err := factsFor(ctx, facts, page.Courses)
		if err != nil {
			return nil, catalogError(err)
		}
		prices, err := pricesFor(ctx, pricing, page.Courses)
		if err != nil {
			return nil, catalogError(err)
		}

		// Drafts must never reach a shared cache, whoever asked for them.
		out := &ListCoursesOutput{CacheControl: draftCacheControl}
		out.Body.Courses = make([]CourseSummary, 0, len(page.Courses))
		for _, c := range page.Courses {
			summary := courseSummary(c, known[c.ID])
			price, sold := prices[c.ID]
			summary.Price = priceView(price, sold)
			out.Body.Courses = append(out.Body.Courses, summary)
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

		// One more query, not one per lesson. A draft has nobody enrolled, nobody who
		// has reviewed it, and nobody to show either to, so it does not pay for one.
		var known enroll.CourseFacts
		if curriculum.Course.Status == catalog.StatusPublished {
			page, err := factsFor(ctx, facts, []catalog.Course{curriculum.Course})
			if err != nil {
				return nil, catalogError(err)
			}
			known = page[curriculum.Course.ID]
		}

		prices, err := pricesFor(ctx, pricing, []catalog.Course{curriculum.Course})
		if err != nil {
			return nil, catalogError(err)
		}

		out := &CurriculumOutput{CacheControl: cacheControl}
		out.Body.Course = courseDetail(curriculum.Course, known)
		price, sold := prices[curriculum.Course.ID]
		out.Body.Course.Price = priceView(price, sold)
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

	huma.Register(api, huma.Operation{
		OperationID:   "get-course-image",
		Method:        http.MethodGet,
		Path:          "/v1/courses/{slug}/image",
		Summary:       "Redirect to a course's thumbnail",
		DefaultStatus: http.StatusFound,
		Description: "A 302 to a short-lived signed URL for the thumbnail, served inline. A draft's image is " +
			"only visible to someone who may edit the course; everyone else gets 404, as they do for the course.",
		Tags: []string{"Catalog"},
	}, func(ctx context.Context, in *struct {
		Slug string `path:"slug" maxLength:"200"`
	}) (*ImageRedirect, error) {
		// Same visibility as the course itself: only an author sees a draft's image.
		p, isAuthor := principalFrom(ctx)
		canSeeDrafts := isAuthor && p.Can(auth.PermCourseWrite)

		url, err := svc.ImageURL(ctx, tenant.ID(ctx), in.Slug, canSeeDrafts)
		if err != nil {
			return nil, catalogError(err)
		}
		return &ImageRedirect{Location: url, CacheControl: "private, no-store"}, nil
	})
}

// ImageRedirect is a 302 to a signed thumbnail URL. The Location is the signed
// object URL; no-store because it rotates and is private to this viewer.
type ImageRedirect struct {
	Location     string `header:"Location"`
	CacheControl string `header:"Cache-Control"`
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

			// The preview clip. Sent together or not at all: a source without its URL
			// keeps the player it had, which would play the wrong thing.
			PreviewSource *string `json:"preview_source,omitempty" enum:"none,youtube,vimeo,embed,hosted" doc:"Who serves the preview. 'none' removes it."`
			PreviewURL    *string `json:"preview_url,omitempty" maxLength:"2000" doc:"What the author supplied: a YouTube or Vimeo link, or a hosted id."`
		}
	}) (*UpdateCourseOutput, error) {
		p, author, err := authorFor(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}

		course, err := svc.EditCourse(ctx, p.TenantID, in.Slug, catalog.CoursePatch{
			Title:         in.Body.Title,
			Summary:       in.Body.Summary,
			Description:   in.Body.Description,
			Difficulty:    in.Body.Difficulty,
			Language:      in.Body.Language,
			Objectives:    in.Body.Objectives,
			Requirements:  in.Body.Requirements,
			PreviewSource: in.Body.PreviewSource,
			PreviewURL:    in.Body.PreviewURL,
		}, author)
		if err != nil {
			return nil, catalogError(err)
		}

		// The learner count is not this endpoint's business, and an author editing
		// copy has not changed it. Zero here rather than a query nobody asked for.
		out := &UpdateCourseOutput{}
		out.Body.Course = courseDetail(course, enroll.CourseFacts{})
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
		out.Body.Course = courseSummary(course, enroll.CourseFacts{})
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "presign-course-image",
		Method:        http.MethodPost,
		Path:          "/v1/courses/{slug}/image/uploads",
		Summary:       "Ask for a URL to upload a course thumbnail to",
		DefaultStatus: http.StatusCreated,
		Description: "Requires course:write. Returns a URL that accepts one image of the declared size for " +
			"fifteen minutes; the bytes go straight to the object store. Confirm the upload to record it.",
		Tags:     []string{"Catalog"},
		Security: []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		Slug string `path:"slug" maxLength:"200"`
		Body struct {
			ContentType string `json:"content_type" enum:"image/png,image/jpeg,image/webp"`
			Bytes       int64  `json:"bytes" minimum:"1" maximum:"5242880"`
		}
	}) (*PresignOutput, error) {
		p, err := requirePermission(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}

		upload, key, err := svc.PresignImage(ctx, p.TenantID, in.Slug, in.Body.ContentType, in.Body.Bytes)
		if err != nil {
			return nil, catalogError(err)
		}

		out := &PresignOutput{CacheControl: "private, no-store"}
		out.Body.UploadURL = upload.URL
		out.Body.Method = upload.Method
		out.Body.Headers = upload.Headers
		out.Body.Key = key
		out.Body.ExpiresAt = upload.ExpiresAt
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "confirm-course-image",
		Method:        http.MethodPut,
		Path:          "/v1/courses/{slug}/image",
		Summary:       "Record a thumbnail you uploaded",
		DefaultStatus: http.StatusNoContent,
		Description: "Requires course:write. The object store is asked what is really at the key before it is " +
			"recorded; a key that is not this course's, or that nothing was uploaded to, is refused. The " +
			"image it replaces is deleted.",
		Tags:     []string{"Catalog"},
		Security: []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		Slug string `path:"slug" maxLength:"200"`
		Body struct {
			Key string `json:"key" minLength:"1" maxLength:"1024"`
		}
	}) (*struct{}, error) {
		p, err := requirePermission(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}

		if err := svc.ConfirmImage(ctx, p.TenantID, in.Slug, in.Body.Key); err != nil {
			return nil, catalogError(err)
		}
		return &struct{}{}, nil
	})
}

// courseDetail maps a course to its page view. The slices are never nil: `[]` and
// `null` are the same emptiness to a client that has to branch on it anyway.
func courseDetail(c catalog.Course, f enroll.CourseFacts) CourseDetail {
	objectives, requirements := c.Objectives, c.Requirements
	if objectives == nil {
		objectives = []string{}
	}
	if requirements == nil {
		requirements = []string{}
	}

	return CourseDetail{
		CourseSummary:   courseSummary(c, f),
		Description:     c.Description,
		Objectives:      objectives,
		Requirements:    requirements,
		Language:        c.Language,
		PreviewSource:   c.Preview.Source,
		PreviewURL:      c.Preview.URL,
		PreviewEmbedURL: c.Preview.EmbedURL,
		UpdatedAt:       c.UpdatedAt,
	}
}

/*
factsFor loads the learner counts and ratings for a page of courses.

One call for the page, not one per card — and the map it returns reads a missing
course as the zero value, which says "nobody enrolled, nobody reviewed". That is
the truth for a course nobody has touched, and it is why the caller never has to
ask whether a course is in the map.
*/
func factsFor(ctx context.Context, facts courseFacts, courses []catalog.Course) (map[uuid.UUID]enroll.CourseFacts, error) {
	if len(courses) == 0 {
		return map[uuid.UUID]enroll.CourseFacts{}, nil
	}

	ids := make([]uuid.UUID, len(courses))
	for i, c := range courses {
		ids[i] = c.ID
	}
	return facts.Facts(ctx, tenant.ID(ctx), ids)
}

// courseSummary maps a course, and whatever is known about it from outside the
// catalogue. A zero `f` is a course nobody asked the facts for: the fields are
// omitted rather than sent as noughts, so a write's echo never claims a rated
// course is unrated.
func courseSummary(c catalog.Course, f enroll.CourseFacts) CourseSummary {
	return CourseSummary{
		ID:            c.ID.String(),
		Slug:          c.Slug,
		Title:         c.Title,
		Summary:       c.Summary,
		Difficulty:    c.Difficulty,
		Status:        c.Status,
		PublishedAt:   c.PublishedAt,
		DripMode:      c.DripMode,
		LessonCount:   c.LessonCount,
		Instructor:    c.InstructorName,
		InstructorID:  instructorID(c),
		LearnerCount:  f.Learners,
		RatingAverage: f.RatingAverage,
		RatingCount:   f.RatingCount,
		ImageURL:      courseImageURL(c),
	}
}

// courseImageURL is the stable redirect path for a course's thumbnail, or empty
// when it has none. The path carries the slug a card already holds; the endpoint it
// points at signs the object URL on demand and applies the course's own visibility.
func courseImageURL(c catalog.Course) string {
	if c.ImageKey == "" {
		return ""
	}
	return "/v1/courses/" + c.Slug + "/image"
}

// optionalUUID reads a query parameter that may simply not be there. An empty
// string is "no filter"; anything else must be a uuid or the request is refused —
// a malformed id is a client bug, not a listing of everything.
func optionalUUID(raw string) (*uuid.UUID, error) {
	if raw == "" {
		return nil, nil
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return nil, err
	}
	return &id, nil
}

// instructorID renders the author's id, or nothing at all for a course whose
// author has been erased — there is no such person to list the courses of.
func instructorID(c catalog.Course) string {
	if c.CreatedBy == nil {
		return ""
	}
	return c.CreatedBy.String()
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

	case errors.Is(err, catalog.ErrInvalidImage):
		return huma.Error422UnprocessableEntity(err.Error())

	case errors.Is(err, catalog.ErrNoImage):
		// 404: a course with no thumbnail has nothing at this path, same as a course
		// that does not exist — the redirect endpoint reveals no more than the page.
		return huma.Error404NotFound("That course has no image.")

	case errors.Is(err, catalog.ErrNoStore):
		return huma.Error503ServiceUnavailable("Image uploads are not available on this server.")

	default:
		// Anything unexpected: the wrapped cause is logged with a correlation ID by
		// the recovery and access-log middleware; the client learns nothing more.
		return err
	}
}
