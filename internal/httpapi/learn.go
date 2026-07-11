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

// learnError maps the learn package's sentinels onto status codes. This is the
// only place that translation happens; the domain never imports net/http.
func learnError(err error) error {
	switch {
	case errors.Is(err, learn.ErrLessonNotFound):
		return huma.Error404NotFound("That lesson does not exist.")

	case errors.Is(err, learn.ErrNoteTooLong):
		return huma.Error422UnprocessableEntity("That note is too long.")

	default:
		// Unmapped: the error handler renders it as a 500 with a correlation id and
		// leaks none of it. A new sentinel without a case here is caught by
		// errors_test.go.
		return err
	}
}
