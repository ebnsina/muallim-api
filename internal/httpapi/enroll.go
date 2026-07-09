package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ebnsina/lms-api/internal/auth"
	"github.com/ebnsina/lms-api/internal/enroll"
	"github.com/ebnsina/lms-api/internal/tenant"
)

// lessonCacheControl keeps lesson content out of every shared cache.
//
// The response depends on who is asking — a preview for a stranger, the full
// body for an enrolled learner — so a shared cache keyed on the URL alone would
// serve one reader's entitlement to another. This is the paywall; it does not get
// a CDN.
const lessonCacheControl = "private, no-store"

// EnrolmentView is an enrolment as a client sees it.
type EnrolmentView struct {
	CourseSlug  string     `json:"course_slug"`
	CourseTitle string     `json:"course_title"`
	Status      string     `json:"status" enum:"active,completed,expired,cancelled"`
	Source      string     `json:"source" enum:"self,granted,purchase,import"`
	EnrolledAt  time.Time  `json:"enrolled_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`

	Progress ProgressView `json:"progress"`
}

// ProgressView is a learner's standing in one course.
type ProgressView struct {
	LessonsCompleted int `json:"lessons_completed"`
	LessonsTotal     int `json:"lessons_total"`
	Percent          int `json:"percent" minimum:"0" maximum:"100"`
}

// EnrolOutput confirms an enrolment.
type EnrolOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Status     string     `json:"status" enum:"active,completed,expired,cancelled"`
		Source     string     `json:"source" enum:"self,granted,purchase,import"`
		EnrolledAt time.Time  `json:"enrolled_at"`
		ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	}
}

// ListEnrolmentsOutput is a learner's dashboard.
type ListEnrolmentsOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Enrolments []EnrolmentView `json:"enrolments"`
	}
}

// ProgressOutput is a learner's standing in one course.
type ProgressOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Progress ProgressView `json:"progress"`
	}
}

// LessonContentOutput is a lesson as a learner reads it.
type LessonContentOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Lesson LessonContentView `json:"lesson"`

		// Access says why this lesson was readable: as a free preview, as an
		// enrolled learner, or as its author. Clients use it to decide whether to
		// show an enrol button.
		Access string `json:"access" enum:"preview,enrolled,author"`
	}
}

// LessonContentView is a lesson's full body.
type LessonContentView struct {
	ID              string     `json:"id" format:"uuid"`
	Title           string     `json:"title"`
	ContentType     string     `json:"content_type" enum:"text,video,quiz,assignment,live,scorm,h5p"`
	Content         string     `json:"content"`
	VideoSource     string     `json:"video_source" enum:"none,youtube,vimeo,embed,hosted"`
	VideoURL        string     `json:"video_url,omitempty"`
	DurationSeconds int        `json:"duration_seconds"`
	IsPreview       bool       `json:"is_preview"`
	Position        int        `json:"position"`
	CompletedAt     *time.Time `json:"completed_at,omitempty"`

	// AvailableAt is when this reader's copy of a dripped lesson opened, or opens.
	// Absent when the course does not drip, and in sequential mode, where a lesson
	// opens on an event rather than a clock.
	AvailableAt *time.Time `json:"available_at,omitempty"`
}

func registerEnrolment(api huma.API, svc *enroll.Service) {
	huma.Register(api, huma.Operation{
		OperationID:   "enrol",
		Method:        http.MethodPost,
		Path:          "/v1/courses/{slug}/enrol",
		Summary:       "Enrol in a published course",
		Description:   "Re-enrolling after cancelling reactivates the original enrolment, so progress survives.",
		Tags:          []string{"Learning"},
		DefaultStatus: http.StatusCreated,
		Security:      []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		Slug string `path:"slug" maxLength:"200"`
	}) (*EnrolOutput, error) {
		p, err := requirePrincipal(ctx)
		if err != nil {
			return nil, err
		}

		enrolment, err := svc.Enrol(ctx, p.TenantID, in.Slug, actorFrom(ctx, p), enroll.SourceSelf)
		if err != nil {
			return nil, enrolError(err)
		}

		out := &EnrolOutput{CacheControl: lessonCacheControl}
		out.Body.Status = enrolment.Status
		out.Body.Source = enrolment.Source
		out.Body.EnrolledAt = enrolment.EnrolledAt
		out.Body.ExpiresAt = enrolment.ExpiresAt
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "cancel-enrolment",
		Method:        http.MethodDelete,
		Path:          "/v1/courses/{slug}/enrol",
		Summary:       "Leave a course",
		Description:   "Access ends. Progress is kept, so re-enrolling restores it.",
		Tags:          []string{"Learning"},
		DefaultStatus: http.StatusNoContent,
		Security:      []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		Slug string `path:"slug" maxLength:"200"`
	}) (*struct{}, error) {
		p, err := requirePrincipal(ctx)
		if err != nil {
			return nil, err
		}
		if err := svc.Cancel(ctx, p.TenantID, in.Slug, actorFrom(ctx, p)); err != nil {
			return nil, enrolError(err)
		}
		return nil, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-my-enrolments",
		Method:      http.MethodGet,
		Path:        "/v1/me/enrolments",
		Summary:     "The courses I am studying",
		Description: "Each enrolment carries its course and its progress, fetched in one joined query.",
		Tags:        []string{"Learning"},
		Security:    []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		Limit int `query:"limit" minimum:"1" maximum:"100" default:"20"`
	}) (*ListEnrolmentsOutput, error) {
		p, err := requirePrincipal(ctx)
		if err != nil {
			return nil, err
		}

		enrolments, err := svc.Enrolments(ctx, p.TenantID, p.UserID, in.Limit)
		if err != nil {
			return nil, enrolError(err)
		}

		out := &ListEnrolmentsOutput{CacheControl: lessonCacheControl}
		out.Body.Enrolments = make([]EnrolmentView, 0, len(enrolments))
		for _, e := range enrolments {
			out.Body.Enrolments = append(out.Body.Enrolments, EnrolmentView{
				CourseSlug:  e.CourseSlug,
				CourseTitle: e.CourseTitle,
				Status:      e.Enrolment.Status,
				Source:      e.Enrolment.Source,
				EnrolledAt:  e.Enrolment.EnrolledAt,
				CompletedAt: e.Enrolment.CompletedAt,
				ExpiresAt:   e.Enrolment.ExpiresAt,
				Progress:    progressView(e.Progress),
			})
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-my-progress",
		Method:      http.MethodGet,
		Path:        "/v1/courses/{slug}/progress",
		Summary:     "My progress in one course",
		Tags:        []string{"Learning"},
		Security:    []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		Slug string `path:"slug" maxLength:"200"`
	}) (*ProgressOutput, error) {
		p, err := requirePrincipal(ctx)
		if err != nil {
			return nil, err
		}

		progress, err := svc.Progress(ctx, p.TenantID, in.Slug, p.UserID)
		if err != nil {
			return nil, enrolError(err)
		}

		out := &ProgressOutput{CacheControl: lessonCacheControl}
		out.Body.Progress = progressView(progress)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "read-lesson",
		Method:      http.MethodGet,
		Path:        "/v1/lessons/{id}/content",
		Summary:     "Read a lesson",
		Description: "A preview lesson of a published course is readable by anyone. Everything else needs " +
			"a live enrolment, or authoring rights. A reader who may not see a lesson receives 404, " +
			"not 403: they learn nothing about whether it exists.",
		Tags: []string{"Learning"},
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*LessonContentOutput, error) {
		lessonID, err := parseUUID(in.ID, "lesson")
		if err != nil {
			return nil, err
		}

		// An anonymous reader is legitimate: that is what a preview is for. The
		// authoring flag is an authorisation decision, never a request parameter.
		reader := enroll.Reader{}
		if p, ok := principalFrom(ctx); ok {
			reader.UserID = p.UserID
			reader.CanAuthor = p.Can(auth.PermCourseWrite)
		}

		lesson, access, err := svc.Lesson(ctx, tenant.ID(ctx), lessonID, reader)
		if err != nil {
			return nil, enrolError(err)
		}

		out := &LessonContentOutput{CacheControl: lessonCacheControl}
		out.Body.Lesson = LessonContentView{
			ID:              lesson.ID.String(),
			Title:           lesson.Title,
			ContentType:     lesson.ContentType,
			Content:         lesson.Content,
			VideoSource:     lesson.VideoSource,
			VideoURL:        lesson.VideoURL,
			DurationSeconds: lesson.DurationSeconds,
			IsPreview:       lesson.IsPreview,
			Position:        lesson.Position,
			CompletedAt:     lesson.CompletedAt,
			AvailableAt:     lesson.AvailableAt,
		}
		out.Body.Access = access.String()
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "complete-lesson",
		Method:      http.MethodPost,
		Path:        "/v1/lessons/{id}/complete",
		Summary:     "Mark a lesson complete, or reopen it",
		Description: "Requires a live enrolment: reading a free preview is not studying a course. " +
			"Course progress is recomputed in the same transaction, and finishing the last lesson " +
			"completes the enrolment.",
		Tags:     []string{"Learning"},
		Security: []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id" format:"uuid"`
		Body struct {
			Complete *bool `json:"complete,omitempty" doc:"Defaults to true. Send false to reopen a lesson."`
		}
	}) (*ProgressOutput, error) {
		p, err := requirePrincipal(ctx)
		if err != nil {
			return nil, err
		}
		lessonID, err := parseUUID(in.ID, "lesson")
		if err != nil {
			return nil, err
		}

		complete := true
		if in.Body.Complete != nil {
			complete = *in.Body.Complete
		}

		progress, err := svc.CompleteLesson(ctx, p.TenantID, lessonID, actorFrom(ctx, p), complete)
		if err != nil {
			return nil, enrolError(err)
		}

		out := &ProgressOutput{CacheControl: lessonCacheControl}
		out.Body.Progress = progressView(progress)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "grant-enrolment",
		Method:        http.MethodPost,
		Path:          "/v1/courses/{slug}/enrolments",
		Summary:       "Enrol somebody else",
		Description:   "Requires course:write. May target an unpublished course, which is how an instructor previews with a colleague.",
		Tags:          []string{"Learning"},
		DefaultStatus: http.StatusCreated,
		Security:      []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		Slug string `path:"slug" maxLength:"200"`
		Body struct {
			UserID string `json:"user_id" format:"uuid"`
		}
	}) (*EnrolOutput, error) {
		p, err := requirePermission(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}
		learnerID, err := parseUUID(in.Body.UserID, "user")
		if err != nil {
			return nil, err
		}

		enrolment, err := svc.Grant(ctx, p.TenantID, in.Slug, learnerID, actorFrom(ctx, p))
		if err != nil {
			return nil, enrolError(err)
		}

		out := &EnrolOutput{CacheControl: lessonCacheControl}
		out.Body.Status = enrolment.Status
		out.Body.Source = enrolment.Source
		out.Body.EnrolledAt = enrolment.EnrolledAt
		return out, nil
	})
}

func actorFrom(ctx context.Context, p auth.Principal) enroll.Actor {
	rc := requestContextFrom(ctx)
	return enroll.Actor{UserID: p.UserID, IP: rc.IP, UserAgent: rc.UserAgent}
}

func progressView(p enroll.Progress) ProgressView {
	return ProgressView{
		LessonsCompleted: p.LessonsCompleted,
		LessonsTotal:     p.LessonsTotal,
		Percent:          p.Percent,
	}
}

// enrolError maps the enroll package's sentinels onto status codes.
func enrolError(err error) error {
	// Checked before the sentinel switch, because it carries detail the switch
	// would throw away: which courses the learner still owes. errors.As, not a
	// parsed message.
	var unmet *enroll.UnmetPrerequisites
	if errors.As(err, &unmet) {
		return huma.Error403Forbidden(prerequisiteMessage(unmet.Missing))
	}

	// 403, not 404. The learner is enrolled and can see this lesson in the
	// curriculum; telling them it does not exist is a lie they disprove by
	// scrolling.
	var locked *enroll.LessonLocked
	if errors.As(err, &locked) {
		return huma.Error403Forbidden(lockedMessage(locked.AvailableAt))
	}

	switch {
	case errors.Is(err, enroll.ErrNotFound):
		return huma.Error404NotFound("Not found.")

	case errors.Is(err, enroll.ErrNotEnrolled):
		// 403, not 404: the course is published and visible, so there is nothing to
		// conceal. The client should show an enrol button.
		return huma.Error403Forbidden("You are not enrolled in this course.")

	case errors.Is(err, enroll.ErrAlreadyEnrolled):
		return huma.Error409Conflict("That person is already enrolled.")

	case errors.Is(err, enroll.ErrCourseNotOpen):
		return huma.Error409Conflict("That course is not open for enrolment.")

	case errors.Is(err, enroll.ErrLessonLocked):
		// Reached when a caller loses the detail on the way here. The status is the
		// same; the sentence is merely less useful.
		return huma.Error403Forbidden("This lesson has not been released yet.")

	case errors.Is(err, enroll.ErrPrerequisitesUnmet):
		// Reached only when a caller loses the detail on the way here. The status is
		// the same either way; a client that shows the generic sentence is merely
		// less helpful, not wrong.
		return huma.Error403Forbidden("Finish this course's prerequisites first.")

	case errors.Is(err, enroll.ErrEnrolmentEnded):
		return huma.Error403Forbidden("Your enrolment has expired.")

	default:
		return err
	}
}

// lockedMessage says when a dripped lesson opens, when anybody can know.
//
// Sequential drip has no date: the lesson opens when the learner finishes the one
// before it, and inventing a date would be worse than admitting there is none.
func lockedMessage(at *time.Time) string {
	if at == nil {
		return "Finish the previous lesson to unlock this one."
	}
	return "This lesson unlocks on " + at.UTC().Format("2 January 2006") + "."
}

// prerequisiteMessage names the courses standing in the learner's way.
//
// 403 rather than 404: the course is published and plainly visible, and the
// answer is "finish those first". A 404 here would hide the reason along with
// the button.
func prerequisiteMessage(missing []enroll.MissingCourse) string {
	titles := make([]string, 0, len(missing))
	for _, c := range missing {
		titles = append(titles, c.Title)
	}

	switch len(titles) {
	case 0:
		// Not reachable: the service only raises this with at least one course.
		return "Finish this course's prerequisites first."
	case 1:
		return "Finish " + titles[0] + " before enrolling on this course."
	default:
		return "Finish these courses first: " + strings.Join(titles, ", ") + "."
	}
}
