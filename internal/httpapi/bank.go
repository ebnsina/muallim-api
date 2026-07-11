package httpapi

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/ebnsina/lms-api/internal/assess"
)

// BankQuestionView is one reusable question as the bank list shows it.
type BankQuestionView struct {
	ID       string `json:"id" format:"uuid"`
	Type     string `json:"type"`
	Prompt   string `json:"prompt"`
	Points   int    `json:"points"`
	Category string `json:"category"`
}

// ListBankOutput is a keyset page of the bank. Private to authors, not cached.
type ListBankOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Questions  []BankQuestionView `json:"questions"`
		Categories []string           `json:"categories"`
		NextCursor string             `json:"next_cursor,omitempty"`
	}
}

// BankQuestionOutput confirms a question was saved to the bank.
type BankQuestionOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Question BankQuestionView `json:"question"`
	}
}

func bankQuestionView(b assess.BankQuestion) BankQuestionView {
	return BankQuestionView{ID: b.ID.String(), Type: b.Type, Prompt: b.Prompt, Points: b.Points, Category: b.Category}
}

func registerBank(api huma.API, svc *assess.Service) {
	huma.Register(api, huma.Operation{
		OperationID: "list-bank-questions",
		Method:      http.MethodGet,
		Path:        "/v1/quiz-bank",
		Summary:     "The reusable question bank",
		Description: "Questions saved for reuse, newest first, optionally filtered by category. Requires course:write.",
		Tags:        []string{"Assessment"},
		Security:    []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		Category string `query:"category" maxLength:"100"`
		Cursor   string `query:"cursor" maxLength:"128"`
		Limit    int    `query:"limit" minimum:"1" maximum:"50" default:"20"`
	}) (*ListBankOutput, error) {
		p, _, err := quizAuthor(ctx)
		if err != nil {
			return nil, err
		}

		page, err := svc.BankQuestions(ctx, p.TenantID, in.Category, in.Cursor, in.Limit)
		if err != nil {
			return nil, assessError(err)
		}
		cats, err := svc.BankCategories(ctx, p.TenantID)
		if err != nil {
			return nil, assessError(err)
		}

		out := &ListBankOutput{CacheControl: "private, no-store"}
		out.Body.Questions = make([]BankQuestionView, 0, len(page.Questions))
		for _, q := range page.Questions {
			out.Body.Questions = append(out.Body.Questions, bankQuestionView(q))
		}
		out.Body.Categories = cats
		out.Body.NextCursor = page.NextCursor
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "save-question-to-bank",
		Method:        http.MethodPost,
		Path:          "/v1/questions/{id}/save-to-bank",
		Summary:       "Save a quiz question to the bank",
		Description:   "Copies an existing question into the reusable bank under an optional category.",
		Tags:          []string{"Assessment"},
		DefaultStatus: http.StatusCreated,
		Security:      []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID   uuid.UUID `path:"id" format:"uuid"`
		Body struct {
			Category string `json:"category,omitempty" maxLength:"100"`
		}
	}) (*BankQuestionOutput, error) {
		p, author, err := quizAuthor(ctx)
		if err != nil {
			return nil, err
		}
		saved, err := svc.SaveQuestionToBank(ctx, p.TenantID, in.ID, in.Body.Category, author)
		if err != nil {
			return nil, assessError(err)
		}
		out := &BankQuestionOutput{CacheControl: "private, no-store"}
		out.Body.Question = bankQuestionView(saved)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "add-bank-question-to-quiz",
		Method:        http.MethodPost,
		Path:          "/v1/lessons/{id}/quiz/questions/from-bank",
		Summary:       "Add a bank question to a lesson's quiz",
		Description:   "Copies a bank question into the quiz. The copy is independent — editing it does not touch the bank.",
		Tags:          []string{"Assessment"},
		DefaultStatus: http.StatusCreated,
		Security:      []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID   uuid.UUID `path:"id" format:"uuid"`
		Body struct {
			BankQuestionID uuid.UUID `json:"bank_question_id" format:"uuid"`
		}
	}) (*QuestionOutput, error) {
		p, author, err := quizAuthor(ctx)
		if err != nil {
			return nil, err
		}
		created, err := svc.AddBankQuestionToQuiz(ctx, p.TenantID, in.ID, in.Body.BankQuestionID, author)
		if err != nil {
			return nil, assessError(err)
		}
		out := &QuestionOutput{CacheControl: lessonCacheControl}
		out.Body.Question = authoredQuestionView(created)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "delete-bank-question",
		Method:        http.MethodDelete,
		Path:          "/v1/quiz-bank/{id}",
		Summary:       "Remove a question from the bank",
		Tags:          []string{"Assessment"},
		DefaultStatus: http.StatusNoContent,
		Security:      []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID uuid.UUID `path:"id" format:"uuid"`
	}) (*struct{}, error) {
		p, _, err := quizAuthor(ctx)
		if err != nil {
			return nil, err
		}
		if err := svc.DeleteBankQuestion(ctx, p.TenantID, in.ID); err != nil {
			return nil, assessError(err)
		}
		return nil, nil
	})
}
