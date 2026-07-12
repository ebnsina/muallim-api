package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/ebnsina/muallim-api/internal/auth"
	"github.com/ebnsina/muallim-api/internal/learn"
)

// AnswerView is one reply in a lesson's discussion. Mine tells the client whose
// post it is, so it can offer a delete without the caller's id leaving the server.
type AnswerView struct {
	ID           string    `json:"id" format:"uuid"`
	Body         string    `json:"body"`
	AuthorName   string    `json:"author_name"`
	ByInstructor bool      `json:"by_instructor"`
	Mine         bool      `json:"mine"`
	CreatedAt    time.Time `json:"created_at"`
}

// ThreadView is one thread: a question and its answers.
type ThreadView struct {
	ID         string       `json:"id" format:"uuid"`
	Body       string       `json:"body"`
	AuthorName string       `json:"author_name"`
	Mine       bool         `json:"mine"`
	CreatedAt  time.Time    `json:"created_at"`
	Answers    []AnswerView `json:"answers"`
}

// ThreadsOutput is a lesson's discussion. It follows lesson access, so it is
// private and never shared by a cache.
type ThreadsOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Questions []ThreadView `json:"questions"`
	}
}

// ThreadOutput carries one posted question.
type ThreadOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Question ThreadView `json:"question"`
	}
}

// AnswerOutput carries one posted answer.
type AnswerOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Answer AnswerView `json:"answer"`
	}
}

func answerView(a learn.Answer, viewer uuid.UUID) AnswerView {
	return AnswerView{
		ID:           a.ID.String(),
		Body:         a.Body,
		AuthorName:   a.AuthorName,
		ByInstructor: a.ByInstructor,
		Mine:         a.AuthorID != uuid.Nil && a.AuthorID == viewer,
		CreatedAt:    a.CreatedAt,
	}
}

func threadView(q learn.Question, viewer uuid.UUID) ThreadView {
	answers := make([]AnswerView, 0, len(q.Answers))
	for _, a := range q.Answers {
		answers = append(answers, answerView(a, viewer))
	}
	return ThreadView{
		ID:         q.ID.String(),
		Body:       q.Body,
		AuthorName: q.AuthorName,
		Mine:       q.AuthorID != uuid.Nil && q.AuthorID == viewer,
		CreatedAt:  q.CreatedAt,
		Answers:    answers,
	}
}

// participantFrom builds the discussion identity from the session. CanModerate
// maps to course:write — an authorisation decision, never a request parameter.
func participantFrom(p auth.Principal) learn.Participant {
	return learn.Participant{UserID: p.UserID, CanModerate: p.Can(auth.PermCourseWrite)}
}

func registerQA(api huma.API, svc *learn.Service) {
	huma.Register(api, huma.Operation{
		OperationID: "list-lesson-questions",
		Method:      http.MethodGet,
		Path:        "/v1/lessons/{id}/questions",
		Summary:     "A lesson's questions and answers",
		Description: "The discussion on a lesson, newest question first, each with its answers oldest first. " +
			"Visible to whoever may read the lesson.",
		Tags:     []string{"Learn"},
		Security: []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID    uuid.UUID `path:"id" format:"uuid"`
		Limit int       `query:"limit" minimum:"1" maximum:"50" default:"50"`
	}) (*ThreadsOutput, error) {
		p, err := requirePrincipal(ctx)
		if err != nil {
			return nil, err
		}

		questions, err := svc.Questions(ctx, p.TenantID, in.ID, participantFrom(p), in.Limit)
		if err != nil {
			return nil, learnError(err)
		}

		out := &ThreadsOutput{CacheControl: "private, no-store"}
		out.Body.Questions = make([]ThreadView, 0, len(questions))
		for _, q := range questions {
			out.Body.Questions = append(out.Body.Questions, threadView(q, p.UserID))
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "ask-lesson-question",
		Method:        http.MethodPost,
		Path:          "/v1/lessons/{id}/questions",
		Summary:       "Ask a question on a lesson",
		Description:   "Requires that you may read the lesson. A lesson you may not read answers 404, not 403.",
		Tags:          []string{"Learn"},
		DefaultStatus: http.StatusCreated,
		Security:      []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID   uuid.UUID `path:"id" format:"uuid"`
		Body struct {
			Body string `json:"body" maxLength:"5000"`
		}
	}) (*ThreadOutput, error) {
		p, err := requirePrincipal(ctx)
		if err != nil {
			return nil, err
		}

		question, err := svc.Ask(ctx, p.TenantID, in.ID, participantFrom(p), in.Body.Body)
		if err != nil {
			return nil, learnError(err)
		}

		out := &ThreadOutput{CacheControl: "private, no-store"}
		out.Body.Question = threadView(question, p.UserID)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "answer-lesson-question",
		Method:        http.MethodPost,
		Path:          "/v1/lesson-questions/{id}/answers",
		Summary:       "Answer a question",
		Description:   "Requires that you may read the question's lesson. An instructor's answer is badged as one.",
		Tags:          []string{"Learn"},
		DefaultStatus: http.StatusCreated,
		Security:      []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID   uuid.UUID `path:"id" format:"uuid"`
		Body struct {
			Body string `json:"body" maxLength:"5000"`
		}
	}) (*AnswerOutput, error) {
		p, err := requirePrincipal(ctx)
		if err != nil {
			return nil, err
		}

		answer, err := svc.Answer(ctx, p.TenantID, in.ID, participantFrom(p), in.Body.Body)
		if err != nil {
			return nil, learnError(err)
		}

		out := &AnswerOutput{CacheControl: "private, no-store"}
		out.Body.Answer = answerView(answer, p.UserID)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "delete-lesson-question",
		Method:        http.MethodDelete,
		Path:          "/v1/lesson-questions/{id}",
		Summary:       "Delete a question",
		Description:   "Yours to delete, or anyone's if you can author the course. Removing a question removes its answers.",
		Tags:          []string{"Learn"},
		DefaultStatus: http.StatusNoContent,
		Security:      []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID uuid.UUID `path:"id" format:"uuid"`
	}) (*struct{}, error) {
		p, err := requirePrincipal(ctx)
		if err != nil {
			return nil, err
		}
		if err := svc.DeleteQuestion(ctx, p.TenantID, in.ID, participantFrom(p)); err != nil {
			return nil, learnError(err)
		}
		return nil, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "delete-lesson-answer",
		Method:        http.MethodDelete,
		Path:          "/v1/lesson-answers/{id}",
		Summary:       "Delete an answer",
		Description:   "Yours to delete, or anyone's if you can author the course.",
		Tags:          []string{"Learn"},
		DefaultStatus: http.StatusNoContent,
		Security:      []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID uuid.UUID `path:"id" format:"uuid"`
	}) (*struct{}, error) {
		p, err := requirePrincipal(ctx)
		if err != nil {
			return nil, err
		}
		if err := svc.DeleteAnswer(ctx, p.TenantID, in.ID, participantFrom(p)); err != nil {
			return nil, learnError(err)
		}
		return nil, nil
	})
}
