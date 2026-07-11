package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/ebnsina/lms-api/internal/learn"
)

// NoteView is a learner's note as a client reads it. An empty body is a real,
// renderable state — a lesson with no note yet — so the field is always present.
type NoteView struct {
	Body string `json:"body"`
	// UpdatedAt is absent until the note has been written at least once.
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// NoteOutput carries one note. Never shared-cacheable: it is one person's private
// margin, and belongs in no cache a proxy keeps.
type NoteOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Note NoteView `json:"note"`
	}
}

func noteView(note learn.Note) NoteView {
	view := NoteView{Body: note.Body}
	if !note.UpdatedAt.IsZero() {
		when := note.UpdatedAt
		view.UpdatedAt = &when
	}
	return view
}

// registerNotes wires a learner's private note on a lesson. Both routes take the
// caller from the session and never a user id from the request: a note is read
// and written only by the person whose note it is.
func registerNotes(api huma.API, svc *learn.Service) {
	huma.Register(api, huma.Operation{
		OperationID: "my-lesson-note",
		Method:      http.MethodGet,
		Path:        "/v1/lessons/{id}/note",
		Summary:     "Your private note on a lesson",
		Description: "The margin you keep on a lesson. A lesson you have never noted answers with an " +
			"empty note, not a 404, so a client renders one shape either way.",
		Tags:     []string{"Learn"},
		Security: []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID uuid.UUID `path:"id" format:"uuid"`
	}) (*NoteOutput, error) {
		p, err := requirePrincipal(ctx)
		if err != nil {
			return nil, err
		}

		note, err := svc.Note(ctx, p.TenantID, p.UserID, in.ID)
		if err != nil {
			return nil, learnError(err)
		}

		out := &NoteOutput{CacheControl: "private, no-store"}
		out.Body.Note = noteView(note)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "save-lesson-note",
		Method:      http.MethodPut,
		Path:        "/v1/lessons/{id}/note",
		Summary:     "Write your note on a lesson",
		Description: "Idempotent: the body you send is the whole note, replacing whatever was there. An " +
			"empty body clears it, because a note a learner has emptied and one they never wrote are " +
			"the same to them.",
		Tags:     []string{"Learn"},
		Security: []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID   uuid.UUID `path:"id" format:"uuid"`
		Body struct {
			Body string `json:"body" maxLength:"10000" doc:"The whole note. Empty clears it."`
		}
	}) (*NoteOutput, error) {
		p, err := requirePrincipal(ctx)
		if err != nil {
			return nil, err
		}

		note, err := svc.Save(ctx, p.TenantID, p.UserID, in.ID, in.Body.Body)
		if err != nil {
			return nil, learnError(err)
		}

		out := &NoteOutput{CacheControl: "private, no-store"}
		out.Body.Note = noteView(note)
		return out, nil
	})
}

// HighlightView is one of a learner's marks as a client reads it.
type HighlightView struct {
	ID        string    `json:"id" format:"uuid"`
	Quote     string    `json:"quote"`
	Note      string    `json:"note"`
	Start     int       `json:"start"`
	End       int       `json:"end"`
	CreatedAt time.Time `json:"created_at"`
}

// HighlightsOutput carries a learner's marks on a lesson. Private, so shared with
// no cache.
type HighlightsOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Highlights []HighlightView `json:"highlights"`
	}
}

// HighlightOutput carries one mark, for the create and edit responses.
type HighlightOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Highlight HighlightView `json:"highlight"`
	}
}

func highlightView(h learn.Highlight) HighlightView {
	return HighlightView{
		ID:        h.ID.String(),
		Quote:     h.Quote,
		Note:      h.Note,
		Start:     h.Start,
		End:       h.End,
		CreatedAt: h.CreatedAt,
	}
}

// registerHighlights wires a learner's marks on a lesson. Like the note, every
// route takes the caller from the session: a mark is read, changed, and removed
// only by the person who made it.
func registerHighlights(api huma.API, svc *learn.Service) {
	huma.Register(api, huma.Operation{
		OperationID: "my-lesson-highlights",
		Method:      http.MethodGet,
		Path:        "/v1/lessons/{id}/highlights",
		Summary:     "The passages you have marked in a lesson",
		Tags:        []string{"Learn"},
		Security:    []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID uuid.UUID `path:"id" format:"uuid"`
	}) (*HighlightsOutput, error) {
		p, err := requirePrincipal(ctx)
		if err != nil {
			return nil, err
		}

		highlights, err := svc.Highlights(ctx, p.TenantID, p.UserID, in.ID)
		if err != nil {
			return nil, learnError(err)
		}

		out := &HighlightsOutput{CacheControl: "private, no-store"}
		out.Body.Highlights = make([]HighlightView, 0, len(highlights))
		for _, h := range highlights {
			out.Body.Highlights = append(out.Body.Highlights, highlightView(h))
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "add-lesson-highlight",
		Method:        http.MethodPost,
		Path:          "/v1/lessons/{id}/highlights",
		Summary:       "Mark a passage in a lesson",
		DefaultStatus: http.StatusCreated,
		Description: "The offsets are character positions into the lesson's text, end exclusive; the " +
			"quote is the text they cover, kept so the passage can be recognised after the lesson is edited.",
		Tags:     []string{"Learn"},
		Security: []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID   uuid.UUID `path:"id" format:"uuid"`
		Body struct {
			Quote string `json:"quote" minLength:"1" maxLength:"2000"`
			Note  string `json:"note,omitempty" maxLength:"10000"`
			Start int    `json:"start" minimum:"0"`
			End   int    `json:"end" minimum:"1"`
		}
	}) (*HighlightOutput, error) {
		p, err := requirePrincipal(ctx)
		if err != nil {
			return nil, err
		}

		created, err := svc.AddHighlight(ctx, p.TenantID, p.UserID, in.ID, learn.Highlight{
			Quote: in.Body.Quote,
			Note:  in.Body.Note,
			Start: in.Body.Start,
			End:   in.Body.End,
		})
		if err != nil {
			return nil, learnError(err)
		}

		out := &HighlightOutput{CacheControl: "private, no-store"}
		out.Body.Highlight = highlightView(created)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "edit-lesson-highlight-note",
		Method:      http.MethodPatch,
		Path:        "/v1/highlights/{id}",
		Summary:     "Change the note on a marked passage",
		Tags:        []string{"Learn"},
		Security:    []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID   uuid.UUID `path:"id" format:"uuid"`
		Body struct {
			Note string `json:"note" maxLength:"10000"`
		}
	}) (*struct{}, error) {
		p, err := requirePrincipal(ctx)
		if err != nil {
			return nil, err
		}

		if err := svc.EditHighlightNote(ctx, p.TenantID, p.UserID, in.ID, in.Body.Note); err != nil {
			return nil, learnError(err)
		}
		return nil, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "delete-lesson-highlight",
		Method:        http.MethodDelete,
		Path:          "/v1/highlights/{id}",
		Summary:       "Remove a marked passage",
		DefaultStatus: http.StatusNoContent,
		Tags:          []string{"Learn"},
		Security:      []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID uuid.UUID `path:"id" format:"uuid"`
	}) (*struct{}, error) {
		p, err := requirePrincipal(ctx)
		if err != nil {
			return nil, err
		}

		if err := svc.RemoveHighlight(ctx, p.TenantID, p.UserID, in.ID); err != nil {
			return nil, learnError(err)
		}
		return nil, nil
	})
}

// CourseNoteView and CourseHighlightView carry the lesson id, which the per-lesson
// views leave out: on a course-wide page the client groups by lesson, so it must
// know which lesson each belongs to.
type CourseNoteView struct {
	LessonID  string    `json:"lesson_id" format:"uuid"`
	Body      string    `json:"body"`
	UpdatedAt time.Time `json:"updated_at"`
}

type CourseHighlightView struct {
	ID        string    `json:"id" format:"uuid"`
	LessonID  string    `json:"lesson_id" format:"uuid"`
	Quote     string    `json:"quote"`
	Note      string    `json:"note"`
	Start     int       `json:"start"`
	End       int       `json:"end"`
	CreatedAt time.Time `json:"created_at"`
}

// AnnotationsOutput is everything a learner has kept across a course.
type AnnotationsOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Notes      []CourseNoteView      `json:"notes"`
		Highlights []CourseHighlightView `json:"highlights"`
	}
}

// registerCourseAnnotations wires the learner's revision page: their notes and
// marks across a whole course, in one read.
func registerCourseAnnotations(api huma.API, svc *learn.Service) {
	huma.Register(api, huma.Operation{
		OperationID: "my-course-annotations",
		Method:      http.MethodGet,
		Path:        "/v1/courses/{slug}/annotations",
		Summary:     "Every note and highlight you have kept in a course",
		Description: "For a revision page. The caller is the learner; no user id is taken from the " +
			"request. Grouping by lesson is the client's to do — it has the curriculum and its titles.",
		Tags:     []string{"Learn"},
		Security: []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		Slug string `path:"slug" maxLength:"100"`
	}) (*AnnotationsOutput, error) {
		p, err := requirePrincipal(ctx)
		if err != nil {
			return nil, err
		}

		notes, highlights, err := svc.CourseAnnotations(ctx, p.TenantID, p.UserID, in.Slug)
		if err != nil {
			return nil, learnError(err)
		}

		out := &AnnotationsOutput{CacheControl: "private, no-store"}
		out.Body.Notes = make([]CourseNoteView, 0, len(notes))
		for _, n := range notes {
			out.Body.Notes = append(out.Body.Notes, CourseNoteView{
				LessonID: n.LessonID.String(), Body: n.Body, UpdatedAt: n.UpdatedAt,
			})
		}
		out.Body.Highlights = make([]CourseHighlightView, 0, len(highlights))
		for _, h := range highlights {
			out.Body.Highlights = append(out.Body.Highlights, CourseHighlightView{
				ID:        h.ID.String(),
				LessonID:  h.LessonID.String(),
				Quote:     h.Quote,
				Note:      h.Note,
				Start:     h.Start,
				End:       h.End,
				CreatedAt: h.CreatedAt,
			})
		}
		return out, nil
	})
}

// learnError maps the learn package's sentinels onto status codes. This is the
// only place that translation happens; the domain never imports net/http.
func learnError(err error) error {
	switch {
	case errors.Is(err, learn.ErrLessonNotFound):
		return huma.Error404NotFound("That lesson does not exist.")

	case errors.Is(err, learn.ErrHighlightNotFound):
		return huma.Error404NotFound("That highlight does not exist.")

	case errors.Is(err, learn.ErrInvalidHighlight):
		return huma.Error422UnprocessableEntity("That is not a valid selection.")

	case errors.Is(err, learn.ErrNoteTooLong):
		return huma.Error422UnprocessableEntity("That note is too long.")

	default:
		// Unmapped: the error handler renders it as a 500 with a correlation id and
		// leaks none of it. A new sentinel without a case here is caught by
		// errors_test.go.
		return err
	}
}
