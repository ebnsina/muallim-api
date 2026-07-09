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

// TopicOutput is a single topic.
type TopicOutput struct {
	Body struct {
		Topic TopicView `json:"topic"`
	}
}

// LessonOutput is a single lesson.
type LessonOutput struct {
	Body struct {
		Lesson LessonView `json:"lesson"`
	}
}

// CourseOutput is a single course.
type CourseOutput struct {
	Body struct {
		Course CourseSummary `json:"course"`
	}
}

func registerAuthoring(api huma.API, svc *catalog.Service) {
	// ---- Topics

	huma.Register(api, huma.Operation{
		OperationID:   "add-topic",
		Method:        http.MethodPost,
		Path:          "/v1/courses/{slug}/topics",
		Summary:       "Append a topic to a course",
		Description:   "Requires course:write. The topic is appended after the last existing one.",
		Tags:          []string{"Authoring"},
		DefaultStatus: http.StatusCreated,
		Security:      []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		Slug string `path:"slug" maxLength:"200"`
		Body struct {
			Title string `json:"title" minLength:"1" maxLength:"300"`
		}
	}) (*TopicOutput, error) {
		p, author, err := authorFor(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}

		topic, err := svc.AddTopic(ctx, p.TenantID, in.Slug, catalog.NewTopic{Title: in.Body.Title}, author)
		if err != nil {
			return nil, catalogError(err)
		}

		out := &TopicOutput{}
		out.Body.Topic = topicView(topic)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "update-topic",
		Method:      http.MethodPatch,
		Path:        "/v1/topics/{id}",
		Summary:     "Rename a topic",
		Tags:        []string{"Authoring"},
		Security:    []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id" format:"uuid"`
		Body struct {
			// A pointer, so an omitted field is left alone rather than cleared.
			Title *string `json:"title,omitempty" minLength:"1" maxLength:"300"`
		}
	}) (*TopicOutput, error) {
		p, author, err := authorFor(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}
		id, err := parseUUID(in.ID, "topic")
		if err != nil {
			return nil, err
		}

		topic, err := svc.EditTopic(ctx, p.TenantID, id, catalog.TopicPatch{Title: in.Body.Title}, author)
		if err != nil {
			return nil, catalogError(err)
		}

		out := &TopicOutput{}
		out.Body.Topic = topicView(topic)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "delete-topic",
		Method:        http.MethodDelete,
		Path:          "/v1/topics/{id}",
		Summary:       "Delete a topic and its lessons",
		Description:   "The remaining topics close up, so positions stay dense.",
		Tags:          []string{"Authoring"},
		DefaultStatus: http.StatusNoContent,
		Security:      []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*struct{}, error) {
		p, author, err := authorFor(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}
		id, err := parseUUID(in.ID, "topic")
		if err != nil {
			return nil, err
		}
		if err := svc.RemoveTopic(ctx, p.TenantID, id, author); err != nil {
			return nil, catalogError(err)
		}
		return nil, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "reorder-topics",
		Method:        http.MethodPut,
		Path:          "/v1/courses/{slug}/topics/order",
		Summary:       "Set the order of a course's topics",
		Description:   "The list must name every topic of the course exactly once. A partial list is refused rather than half-applied.",
		Tags:          []string{"Authoring"},
		DefaultStatus: http.StatusNoContent,
		Security:      []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		Slug string `path:"slug" maxLength:"200"`
		Body struct {
			TopicIDs []string `json:"topic_ids" minItems:"1" maxItems:"500" doc:"Every topic of the course, in the order wanted."`
		}
	}) (*struct{}, error) {
		p, author, err := authorFor(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}
		order, err := parseUUIDs(in.Body.TopicIDs, "topic")
		if err != nil {
			return nil, err
		}
		if err := svc.ReorderTopics(ctx, p.TenantID, in.Slug, order, author); err != nil {
			return nil, catalogError(err)
		}
		return nil, nil
	})

	// ---- Lessons

	huma.Register(api, huma.Operation{
		OperationID:   "add-lesson",
		Method:        http.MethodPost,
		Path:          "/v1/topics/{id}/lessons",
		Summary:       "Append a lesson to a topic",
		Tags:          []string{"Authoring"},
		DefaultStatus: http.StatusCreated,
		Security:      []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id" format:"uuid"`
		Body struct {
			Title           string `json:"title" minLength:"1" maxLength:"300"`
			ContentType     string `json:"content_type,omitempty" enum:"text,video,quiz,assignment,live,scorm,h5p" default:"text"`
			Content         string `json:"content,omitempty" maxLength:"200000"`
			VideoSource     string `json:"video_source,omitempty" enum:"none,youtube,vimeo,embed,hosted" default:"none"`
			VideoURL        string `json:"video_url,omitempty" maxLength:"2000"`
			DurationSeconds int    `json:"duration_seconds,omitempty" minimum:"0" maximum:"86400"`
			IsPreview       bool   `json:"is_preview,omitempty" doc:"Visible to students before they enrol."`
		}
	}) (*LessonOutput, error) {
		p, author, err := authorFor(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}
		topicID, err := parseUUID(in.ID, "topic")
		if err != nil {
			return nil, err
		}

		lesson, err := svc.AddLesson(ctx, p.TenantID, topicID, catalog.NewLesson{
			Title:           in.Body.Title,
			ContentType:     defaultTo(in.Body.ContentType, "text"),
			Content:         in.Body.Content,
			VideoSource:     defaultTo(in.Body.VideoSource, catalog.VideoNone),
			VideoURL:        in.Body.VideoURL,
			DurationSeconds: in.Body.DurationSeconds,
			IsPreview:       in.Body.IsPreview,
		}, author)
		if err != nil {
			return nil, catalogError(err)
		}

		out := &LessonOutput{}
		out.Body.Lesson = lessonView(lesson)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "update-lesson",
		Method:      http.MethodPatch,
		Path:        "/v1/lessons/{id}",
		Summary:     "Edit a lesson",
		Description: "Omitted fields are left alone. Sending a field with an empty value clears it.",
		Tags:        []string{"Authoring"},
		Security:    []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id" format:"uuid"`
		Body struct {
			Title           *string `json:"title,omitempty" minLength:"1" maxLength:"300"`
			ContentType     *string `json:"content_type,omitempty" enum:"text,video,quiz,assignment,live,scorm,h5p"`
			Content         *string `json:"content,omitempty" maxLength:"200000"`
			VideoSource     *string `json:"video_source,omitempty" enum:"none,youtube,vimeo,embed,hosted"`
			VideoURL        *string `json:"video_url,omitempty" maxLength:"2000"`
			DurationSeconds *int    `json:"duration_seconds,omitempty" minimum:"0" maximum:"86400"`
			IsPreview       *bool   `json:"is_preview,omitempty"`

			// The drip schedule. Which one matters is decided by the course's mode,
			// not by which of them is set here.
			AvailableAt        *time.Time `json:"available_at,omitempty" doc:"Scheduled mode: the instant this lesson opens, for everybody"`
			AvailableAfterDays *int       `json:"available_after_days,omitempty" minimum:"0" maximum:"3650" doc:"After-enrolment mode: days after each learner's own enrolment"`
		}
	}) (*LessonOutput, error) {
		p, author, err := authorFor(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}
		id, err := parseUUID(in.ID, "lesson")
		if err != nil {
			return nil, err
		}

		lesson, err := svc.EditLesson(ctx, p.TenantID, id, catalog.LessonPatch{
			Title:           in.Body.Title,
			ContentType:     in.Body.ContentType,
			Content:         in.Body.Content,
			VideoSource:     in.Body.VideoSource,
			VideoURL:        in.Body.VideoURL,
			DurationSeconds: in.Body.DurationSeconds,
			IsPreview:       in.Body.IsPreview,

			AvailableAt:        in.Body.AvailableAt,
			AvailableAfterDays: in.Body.AvailableAfterDays,
		}, author)
		if err != nil {
			return nil, catalogError(err)
		}

		out := &LessonOutput{}
		out.Body.Lesson = lessonView(lesson)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "delete-lesson",
		Method:        http.MethodDelete,
		Path:          "/v1/lessons/{id}",
		Summary:       "Delete a lesson",
		Tags:          []string{"Authoring"},
		DefaultStatus: http.StatusNoContent,
		Security:      []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*struct{}, error) {
		p, author, err := authorFor(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}
		id, err := parseUUID(in.ID, "lesson")
		if err != nil {
			return nil, err
		}
		if err := svc.RemoveLesson(ctx, p.TenantID, id, author); err != nil {
			return nil, catalogError(err)
		}
		return nil, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "reorder-lessons",
		Method:        http.MethodPut,
		Path:          "/v1/topics/{id}/lessons/order",
		Summary:       "Set the order of a topic's lessons",
		Description:   "The list must name every lesson of the topic exactly once.",
		Tags:          []string{"Authoring"},
		DefaultStatus: http.StatusNoContent,
		Security:      []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id" format:"uuid"`
		Body struct {
			LessonIDs []string `json:"lesson_ids" minItems:"1" maxItems:"1000"`
		}
	}) (*struct{}, error) {
		p, author, err := authorFor(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}
		topicID, err := parseUUID(in.ID, "topic")
		if err != nil {
			return nil, err
		}
		order, err := parseUUIDs(in.Body.LessonIDs, "lesson")
		if err != nil {
			return nil, err
		}
		if err := svc.ReorderLessons(ctx, p.TenantID, topicID, order, author); err != nil {
			return nil, catalogError(err)
		}
		return nil, nil
	})

	// ---- Publishing

	huma.Register(api, huma.Operation{
		OperationID: "publish-course",
		Method:      http.MethodPost,
		Path:        "/v1/courses/{slug}/publish",
		Summary:     "Make a course visible to students",
		Description: "Requires course:publish, which is a separate permission from course:write — " +
			"drafting and releasing are different acts. A course with no lessons cannot be published.",
		Tags:     []string{"Authoring"},
		Security: []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		Slug string `path:"slug" maxLength:"200"`
	}) (*CourseOutput, error) {
		p, author, err := authorFor(ctx, auth.PermCoursePublish)
		if err != nil {
			return nil, err
		}

		course, err := svc.Publish(ctx, p.TenantID, in.Slug, author)
		if err != nil {
			return nil, catalogError(err)
		}

		out := &CourseOutput{}
		out.Body.Course = courseSummary(course)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "unpublish-course",
		Method:      http.MethodPost,
		Path:        "/v1/courses/{slug}/unpublish",
		Summary:     "Return a course to draft",
		Description: "Students lose access. Enrolments are untouched: unpublishing is an editorial act, not a refund.",
		Tags:        []string{"Authoring"},
		Security:    []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		Slug string `path:"slug" maxLength:"200"`
	}) (*CourseOutput, error) {
		p, author, err := authorFor(ctx, auth.PermCoursePublish)
		if err != nil {
			return nil, err
		}

		course, err := svc.Unpublish(ctx, p.TenantID, in.Slug, author)
		if err != nil {
			return nil, catalogError(err)
		}

		out := &CourseOutput{}
		out.Body.Course = courseSummary(course)
		return out, nil
	})
}

// authorFor authorises the caller and packages the audit detail every authoring
// action records.
func authorFor(ctx context.Context, permission string) (auth.Principal, catalog.Author, error) {
	p, err := requirePermission(ctx, permission)
	if err != nil {
		return auth.Principal{}, catalog.Author{}, err
	}
	rc := requestContextFrom(ctx)
	return p, catalog.Author{UserID: p.UserID, IP: rc.IP, UserAgent: rc.UserAgent}, nil
}

func parseUUID(raw, what string) (uuid.UUID, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, huma.Error422UnprocessableEntity("The " + what + " id is not a valid UUID.")
	}
	return id, nil
}

func parseUUIDs(raw []string, what string) ([]uuid.UUID, error) {
	ids := make([]uuid.UUID, 0, len(raw))
	for _, r := range raw {
		id, err := parseUUID(r, what)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func defaultTo(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func topicView(t catalog.Topic) TopicView {
	lessons := make([]LessonView, 0, len(t.Lessons))
	for _, l := range t.Lessons {
		lessons = append(lessons, lessonView(l))
	}
	return TopicView{
		ID:       t.ID.String(),
		Title:    t.Title,
		Position: t.Position,
		Lessons:  lessons,
	}
}

func lessonView(l catalog.Lesson) LessonView {
	return LessonView{
		ID:              l.ID.String(),
		Title:           l.Title,
		ContentType:     l.ContentType,
		DurationSeconds: l.DurationSeconds,
		IsPreview:       l.IsPreview,
		Position:        l.Position,
	}
}
