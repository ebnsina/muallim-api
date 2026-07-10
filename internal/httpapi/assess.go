package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/ebnsina/lms-api/internal/assess"
	"github.com/ebnsina/lms-api/internal/auth"
	"github.com/ebnsina/lms-api/internal/enroll"
	"github.com/ebnsina/lms-api/internal/tenant"
)

// Every quiz endpoint is addressed under its lesson, and every attempt under the
// quiz — by attempt *number*, for the calling learner.
//
// That is a security decision, not a URL-design one. There is no attempt id in
// any path, so there is nothing for a client to increment into somebody else's
// result; and because the lesson is named, the access check that guards a lesson
// guards its quiz too, with no second rule to keep in step with the first.

// QuizView is a quiz as the person taking it sees it.
type QuizView struct {
	ID          string `json:"id" format:"uuid"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`

	TimeLimitSeconds int `json:"time_limit_seconds" doc:"Zero means no limit."`
	MaxAttempts      int `json:"max_attempts" doc:"Zero means unlimited."`
	PassingPercent   int `json:"passing_percent" minimum:"0" maximum:"100"`
	TotalPoints      int `json:"total_points"`

	Questions []QuestionView `json:"questions"`
}

// QuestionView is one question, with the answer removed.
//
// There is no `is_correct`, no `accepted`, and no explanation. The domain hands
// this layer a type that has nowhere to put them.
type QuestionView struct {
	ID       string `json:"id" format:"uuid"`
	Type     string `json:"type" enum:"true_false,single_choice,multiple_choice,fill_blanks,short_answer,ordering,matching,open_ended"`
	Prompt   string `json:"prompt"`
	Points   int    `json:"points"`
	Position int    `json:"position"`

	Options []OptionView `json:"options,omitempty"`

	// Matches are a matching question's right-hand sides, under identifiers of
	// their own. Which option each belongs to is the answer.
	Matches []OptionView `json:"matches,omitempty"`

	// Blanks is how many holes a fill-in-the-blanks prompt has.
	Blanks int `json:"blanks,omitempty"`
}

// OptionView is a choice, an item to order, or a match. No verdict attached.
type OptionView struct {
	ID      string `json:"id" format:"uuid"`
	Content string `json:"content"`
}

// AttemptView is the state of one run at a quiz.
type AttemptView struct {
	Number int    `json:"number"`
	Status string `json:"status" enum:"in_progress,grading,awaiting_review,graded"`

	StartedAt   time.Time  `json:"started_at"`
	SubmittedAt *time.Time `json:"submitted_at,omitempty"`
	GradedAt    *time.Time `json:"graded_at,omitempty"`

	// ExpiresAt is this attempt's own deadline, frozen when it started.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`

	Points    int `json:"points"`
	MaxPoints int `json:"max_points"`
	Percent   int `json:"percent" minimum:"0" maximum:"100"`

	// Passed is absent until every question has a grade. An essay awaiting an
	// instructor leaves it absent, rather than guessing.
	Passed *bool `json:"passed,omitempty"`
}

// ReviewItemView is one question of a finished attempt, with its verdict.
//
// It says what the learner answered and whether it was right — never what the
// right answer was. A quiz that allows a second attempt would otherwise hand out
// the key with the first result.
type ReviewItemView struct {
	QuestionID string `json:"question_id" format:"uuid"`
	Prompt     string `json:"prompt"`
	Type       string `json:"type"`
	Position   int    `json:"position"`

	Response assess.Response `json:"response"`

	Graded    bool `json:"graded" doc:"False while an essay waits for an instructor."`
	Correct   bool `json:"correct"`
	Points    int  `json:"points"`
	MaxPoints int  `json:"max_points"`

	// Explanation is the author's note, released once the attempt has been graded.
	Explanation string `json:"explanation,omitempty"`

	// Feedback is what an instructor wrote about this particular answer.
	Feedback string `json:"feedback,omitempty"`
}

// GetQuizOutput is a quiz and the caller's attempts at it.
type GetQuizOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Quiz     QuizView      `json:"quiz"`
		Attempts []AttemptView `json:"attempts"`
	}
}

// AttemptOutput is one attempt.
type AttemptOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Attempt AttemptView `json:"attempt"`
	}
}

// ReviewOutput is a finished attempt, question by question.
type ReviewOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Attempt AttemptView      `json:"attempt"`
		Items   []ReviewItemView `json:"items"`
	}
}

// QuizOnlyOutput confirms an authoring write.
type QuizOnlyOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Quiz QuizSettingsView `json:"quiz"`
	}
}

// QuizSettingsView is a quiz without its questions.
type QuizSettingsView struct {
	ID               string `json:"id" format:"uuid"`
	LessonID         string `json:"lesson_id" format:"uuid"`
	Title            string `json:"title"`
	Description      string `json:"description,omitempty"`
	TimeLimitSeconds int    `json:"time_limit_seconds"`
	MaxAttempts      int    `json:"max_attempts"`
	PassingPercent   int    `json:"passing_percent"`
}

// QuestionOutput confirms a question was written.
type QuestionOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Question AuthoredQuestionView `json:"question"`
	}
}

// AuthoredQuestionView is a question as its author sees it — answers included.
//
// This type is returned only from endpoints that require course:write. It is the
// only place in this API where a correct answer appears.
type AuthoredQuestionView struct {
	ID       string `json:"id" format:"uuid"`
	Type     string `json:"type"`
	Prompt   string `json:"prompt"`
	Points   int    `json:"points"`
	Position int    `json:"position"`

	Explanation   string     `json:"explanation,omitempty"`
	CaseSensitive bool       `json:"case_sensitive"`
	Accepted      [][]string `json:"accepted,omitempty"`

	Options []AuthoredOptionView `json:"options,omitempty"`
}

// AuthoredOptionView carries the answer, and goes only to authors.
type AuthoredOptionView struct {
	ID           string `json:"id" format:"uuid"`
	Content      string `json:"content"`
	Position     int    `json:"position"`
	IsCorrect    bool   `json:"is_correct"`
	MatchID      string `json:"match_id,omitempty" format:"uuid"`
	MatchContent string `json:"match_content,omitempty"`
}

// AuthoredQuizOutput is the whole quiz, answers and all.
type AuthoredQuizOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Quiz      QuizSettingsView       `json:"quiz"`
		Questions []AuthoredQuestionView `json:"questions"`
	}
}

func registerAssessment(api huma.API, svc *assess.Service, learning *enroll.Service) {
	huma.Register(api, huma.Operation{
		OperationID: "get-quiz",
		Method:      http.MethodGet,
		Path:        "/v1/lessons/{id}/quiz",
		Summary:     "Read a lesson's quiz",
		Description: "The questions, with no answers in them. Whoever may read the lesson may read its " +
			"quiz; a reader who may not receives 404. A learner also gets their own attempts.",
		Tags: []string{"Assessment"},
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*GetQuizOutput, error) {
		lessonID, err := readableLesson(ctx, learning, in.ID)
		if err != nil {
			return nil, err
		}

		// An anonymous reader of a preview lesson has no attempts and cannot have.
		var userID uuid.UUID
		if p, ok := principalFrom(ctx); ok {
			userID = p.UserID
		}

		quiz, attempts, err := svc.LearnerView(ctx, tenant.ID(ctx), lessonID, userID)
		if err != nil {
			return nil, assessError(err)
		}

		out := &GetQuizOutput{CacheControl: lessonCacheControl}
		out.Body.Quiz = quizView(quiz)
		out.Body.Attempts = attemptViews(attempts)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "start-attempt",
		Method:        http.MethodPost,
		Path:          "/v1/lessons/{id}/quiz/attempts",
		Summary:       "Start an attempt, or resume the open one",
		DefaultStatus: http.StatusCreated,
		Description: "Requires a live enrolment: taking a quiz is studying the course. Idempotent — a " +
			"learner who reloads the page resumes their attempt rather than burning another.",
		Tags:     []string{"Assessment"},
		Security: []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*AttemptOutput, error) {
		p, lessonID, err := enrolledLearner(ctx, learning, in.ID)
		if err != nil {
			return nil, err
		}

		attempt, err := svc.StartAttempt(ctx, p.TenantID, lessonID, p.UserID, authorFrom(ctx, p))
		if err != nil {
			return nil, assessError(err)
		}

		out := &AttemptOutput{CacheControl: lessonCacheControl}
		out.Body.Attempt = attemptView(attempt)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "answer-question",
		Method:      http.MethodPut,
		Path:        "/v1/lessons/{id}/quiz/attempts/current/answers/{question_id}",
		Summary:     "Answer one question of the open attempt",
		Description: "Replaces any previous answer to that question. Refused once the attempt has been " +
			"submitted, or once its time limit has passed.",
		Tags:     []string{"Assessment"},
		Security: []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID         string `path:"id" format:"uuid"`
		QuestionID string `path:"question_id" format:"uuid"`
		Body       struct {
			Response assess.Response `json:"response"`
		}
	}) (*struct{}, error) {
		p, lessonID, err := enrolledLearner(ctx, learning, in.ID)
		if err != nil {
			return nil, err
		}
		questionID, err := parseUUID(in.QuestionID, "question")
		if err != nil {
			return nil, err
		}

		if err := svc.SaveAnswer(ctx, p.TenantID, lessonID, p.UserID, questionID, in.Body.Response); err != nil {
			return nil, assessError(err)
		}
		return &struct{}{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "submit-attempt",
		Method:      http.MethodPost,
		Path:        "/v1/lessons/{id}/quiz/attempts/current/submit",
		Summary:     "Submit the open attempt for grading",
		Description: "Returns immediately with the attempt in `grading`. A background job grades it and " +
			"the result appears on the attempt; nothing is graded in this request. Poll the attempt, " +
			"or read the review once its status leaves `grading`.",
		Tags:     []string{"Assessment"},
		Security: []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*AttemptOutput, error) {
		p, lessonID, err := enrolledLearner(ctx, learning, in.ID)
		if err != nil {
			return nil, err
		}

		attempt, err := svc.SubmitAttempt(ctx, p.TenantID, lessonID, p.UserID, authorFrom(ctx, p))
		if err != nil {
			return nil, assessError(err)
		}

		out := &AttemptOutput{CacheControl: lessonCacheControl}
		out.Body.Attempt = attemptView(attempt)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "review-attempt",
		Method:      http.MethodGet,
		Path:        "/v1/lessons/{id}/quiz/attempts/{number}",
		Summary:     "Read one of your own attempts",
		Description: "Question by question: what you answered, whether it was right, and — once the " +
			"attempt is graded — the author's explanation. Never the correct answer itself.",
		Tags:     []string{"Assessment"},
		Security: []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID     string `path:"id" format:"uuid"`
		Number int    `path:"number" minimum:"1"`
	}) (*ReviewOutput, error) {
		p, err := requirePrincipal(ctx)
		if err != nil {
			return nil, err
		}
		lessonID, err := readableLesson(ctx, learning, in.ID)
		if err != nil {
			return nil, err
		}

		review, err := svc.Review(ctx, p.TenantID, lessonID, p.UserID, in.Number)
		if err != nil {
			return nil, assessError(err)
		}

		out := &ReviewOutput{CacheControl: lessonCacheControl}
		out.Body.Attempt = attemptView(review.Attempt)
		out.Body.Items = make([]ReviewItemView, 0, len(review.Items))
		for _, item := range review.Items {
			out.Body.Items = append(out.Body.Items, ReviewItemView{
				QuestionID: item.QuestionID.String(), Prompt: item.Prompt, Type: item.Type,
				Position: item.Position, Response: item.Response,
				Graded: item.Graded, Correct: item.Correct,
				Points: item.Points, MaxPoints: item.MaxPoints,
				Explanation: item.Explanation, Feedback: item.Feedback,
			})
		}
		return out, nil
	})

	registerQuizAuthoring(api, svc)
}

func registerQuizAuthoring(api huma.API, svc *assess.Service) {
	huma.Register(api, huma.Operation{
		OperationID:   "create-quiz",
		Method:        http.MethodPost,
		Path:          "/v1/lessons/{id}/quiz",
		Summary:       "Attach a quiz to a lesson",
		DefaultStatus: http.StatusCreated,
		Description:   "A lesson has at most one quiz.",
		Tags:          []string{"Authoring"},
		Security:      []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id" format:"uuid"`
		Body struct {
			Title            string `json:"title" minLength:"1" maxLength:"200"`
			Description      string `json:"description,omitempty" maxLength:"2000"`
			TimeLimitSeconds int    `json:"time_limit_seconds,omitempty" minimum:"0"`
			MaxAttempts      int    `json:"max_attempts,omitempty" minimum:"0"`
			PassingPercent   int    `json:"passing_percent,omitempty" minimum:"0" maximum:"100"`
		}
	}) (*QuizOnlyOutput, error) {
		p, author, err := quizAuthor(ctx)
		if err != nil {
			return nil, err
		}
		lessonID, err := parseUUID(in.ID, "lesson")
		if err != nil {
			return nil, err
		}

		quiz, err := svc.CreateQuiz(ctx, p.TenantID, lessonID, assess.NewQuiz{
			Title: in.Body.Title, Description: in.Body.Description,
			TimeLimitSeconds: in.Body.TimeLimitSeconds,
			MaxAttempts:      in.Body.MaxAttempts,
			PassingPercent:   in.Body.PassingPercent,
		}, author)
		if err != nil {
			return nil, assessError(err)
		}

		out := &QuizOnlyOutput{CacheControl: lessonCacheControl}
		out.Body.Quiz = quizSettingsView(quiz)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-authored-quiz",
		Method:      http.MethodGet,
		Path:        "/v1/lessons/{id}/quiz/authoring",
		Summary:     "Read a quiz with its answers",
		Description: "The only endpoint in this API that returns a correct answer. Requires course:write.",
		Tags:        []string{"Authoring"},
		Security:    []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*AuthoredQuizOutput, error) {
		p, _, err := quizAuthor(ctx)
		if err != nil {
			return nil, err
		}
		lessonID, err := parseUUID(in.ID, "lesson")
		if err != nil {
			return nil, err
		}

		quiz, questions, err := svc.AuthoredQuiz(ctx, p.TenantID, lessonID)
		if err != nil {
			return nil, assessError(err)
		}

		out := &AuthoredQuizOutput{CacheControl: lessonCacheControl}
		out.Body.Quiz = quizSettingsView(quiz)
		out.Body.Questions = make([]AuthoredQuestionView, 0, len(questions))
		for _, q := range questions {
			out.Body.Questions = append(out.Body.Questions, authoredQuestionView(q))
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "update-quiz",
		Method:      http.MethodPatch,
		Path:        "/v1/lessons/{id}/quiz",
		Summary:     "Change a quiz's settings",
		Description: "An omitted field is left alone. Changing the time limit does not shorten an attempt " +
			"already under way: its deadline was frozen when it started.",
		Tags:     []string{"Authoring"},
		Security: []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id" format:"uuid"`
		Body struct {
			Title            *string `json:"title,omitempty" minLength:"1" maxLength:"200"`
			Description      *string `json:"description,omitempty" maxLength:"2000"`
			TimeLimitSeconds *int    `json:"time_limit_seconds,omitempty" minimum:"0"`
			MaxAttempts      *int    `json:"max_attempts,omitempty" minimum:"0"`
			PassingPercent   *int    `json:"passing_percent,omitempty" minimum:"0" maximum:"100"`
		}
	}) (*QuizOnlyOutput, error) {
		p, author, err := quizAuthor(ctx)
		if err != nil {
			return nil, err
		}
		lessonID, err := parseUUID(in.ID, "lesson")
		if err != nil {
			return nil, err
		}

		quiz, err := svc.EditQuiz(ctx, p.TenantID, lessonID, assess.QuizPatch{
			Title: in.Body.Title, Description: in.Body.Description,
			TimeLimitSeconds: in.Body.TimeLimitSeconds,
			MaxAttempts:      in.Body.MaxAttempts,
			PassingPercent:   in.Body.PassingPercent,
		}, author)
		if err != nil {
			return nil, assessError(err)
		}

		out := &QuizOnlyOutput{CacheControl: lessonCacheControl}
		out.Body.Quiz = quizSettingsView(quiz)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "delete-quiz",
		Method:        http.MethodDelete,
		Path:          "/v1/lessons/{id}/quiz",
		Summary:       "Remove a lesson's quiz",
		Description:   "Deletes every question and every attempt at it.",
		DefaultStatus: http.StatusNoContent,
		Tags:          []string{"Authoring"},
		Security:      []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*struct{}, error) {
		p, author, err := quizAuthor(ctx)
		if err != nil {
			return nil, err
		}
		lessonID, err := parseUUID(in.ID, "lesson")
		if err != nil {
			return nil, err
		}

		if err := svc.RemoveQuiz(ctx, p.TenantID, lessonID, author); err != nil {
			return nil, assessError(err)
		}
		return &struct{}{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "add-question",
		Method:        http.MethodPost,
		Path:          "/v1/lessons/{id}/quiz/questions",
		Summary:       "Append a question to a quiz",
		DefaultStatus: http.StatusCreated,
		Description: "A question nobody could answer correctly is refused: single choice with no correct " +
			"option or two, multiple choice where every option is correct, an ordering question with a " +
			"correct item.",
		Tags:     []string{"Authoring"},
		Security: []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id" format:"uuid"`
		Body struct {
			Type   string `json:"type" enum:"true_false,single_choice,multiple_choice,fill_blanks,short_answer,ordering,matching,open_ended"`
			Prompt string `json:"prompt" minLength:"1" maxLength:"4000"`
			Points int    `json:"points,omitempty" minimum:"0" maximum:"1000" default:"1"`

			Explanation   string `json:"explanation,omitempty" maxLength:"4000" doc:"Shown to the learner once their attempt is graded."`
			CaseSensitive bool   `json:"case_sensitive,omitempty"`

			Accepted [][]string `json:"accepted,omitempty" doc:"One array of acceptable spellings per blank, in order."`

			Options []struct {
				Content      string `json:"content" minLength:"1" maxLength:"1000"`
				IsCorrect    bool   `json:"is_correct,omitempty"`
				MatchContent string `json:"match_content,omitempty" maxLength:"1000"`
			} `json:"options,omitempty"`
		}
	}) (*QuestionOutput, error) {
		p, author, err := quizAuthor(ctx)
		if err != nil {
			return nil, err
		}
		lessonID, err := parseUUID(in.ID, "lesson")
		if err != nil {
			return nil, err
		}

		n := assess.NewQuestion{
			Type: in.Body.Type, Prompt: in.Body.Prompt, Points: in.Body.Points,
			Explanation: in.Body.Explanation, CaseSensitive: in.Body.CaseSensitive,
			Accepted: in.Body.Accepted,
		}
		for _, o := range in.Body.Options {
			n.Options = append(n.Options, assess.NewOption{
				Content: o.Content, IsCorrect: o.IsCorrect, MatchContent: o.MatchContent,
			})
		}

		question, err := svc.AddQuestion(ctx, p.TenantID, lessonID, n, author)
		if err != nil {
			return nil, assessError(err)
		}

		out := &QuestionOutput{CacheControl: lessonCacheControl}
		out.Body.Question = authoredQuestionView(question)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "delete-question",
		Method:        http.MethodDelete,
		Path:          "/v1/questions/{id}",
		Summary:       "Remove a question",
		DefaultStatus: http.StatusNoContent,
		Tags:          []string{"Authoring"},
		Security:      []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*struct{}, error) {
		p, author, err := quizAuthor(ctx)
		if err != nil {
			return nil, err
		}
		questionID, err := parseUUID(in.ID, "question")
		if err != nil {
			return nil, err
		}

		if err := svc.RemoveQuestion(ctx, p.TenantID, questionID, author); err != nil {
			return nil, assessError(err)
		}
		return &struct{}{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "reorder-questions",
		Method:        http.MethodPut,
		Path:          "/v1/lessons/{id}/quiz/questions/order",
		Summary:       "Reorder a quiz's questions",
		DefaultStatus: http.StatusNoContent,
		Description:   "The list must name every question exactly once, or the order is refused rather than half-applied.",
		Tags:          []string{"Authoring"},
		Security:      []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id" format:"uuid"`
		Body struct {
			Questions []string `json:"questions" minItems:"1" maxItems:"500"`
		}
	}) (*struct{}, error) {
		p, author, err := quizAuthor(ctx)
		if err != nil {
			return nil, err
		}
		lessonID, err := parseUUID(in.ID, "lesson")
		if err != nil {
			return nil, err
		}

		order := make([]uuid.UUID, 0, len(in.Body.Questions))
		for _, raw := range in.Body.Questions {
			id, err := parseUUID(raw, "question")
			if err != nil {
				return nil, err
			}
			order = append(order, id)
		}

		if err := svc.ReorderQuestions(ctx, p.TenantID, lessonID, order, author); err != nil {
			return nil, assessError(err)
		}
		return &struct{}{}, nil
	})
}

// readableLesson resolves a lesson id and refuses anyone who may not read it.
//
// The check is `enroll`'s, not a second one written here: whoever may read the
// lesson may read its quiz, and there is no rule to keep in step. A reader who
// may not receives whatever the lesson itself would give them — 404 for a draft,
// 403 for a dripped lesson they are enrolled in.
func readableLesson(ctx context.Context, learning *enroll.Service, raw string) (uuid.UUID, error) {
	lessonID, err := parseUUID(raw, "lesson")
	if err != nil {
		return uuid.Nil, err
	}

	reader := enroll.Reader{}
	if p, ok := principalFrom(ctx); ok {
		reader.UserID = p.UserID
		reader.CanAuthor = p.Can(auth.PermCourseWrite)
	}

	if _, _, err := learning.Lesson(ctx, tenant.ID(ctx), lessonID, reader); err != nil {
		return uuid.Nil, enrolError(err)
	}
	return lessonID, nil
}

// enrolledLearner refuses anyone who is not enrolled.
//
// Reading a free preview is not studying a course, so it is not taking its quiz
// either — the same rule that governs marking a lesson complete. An author is not
// exempt: an attempt belongs to a learner, and an author who wants one enrols.
func enrolledLearner(ctx context.Context, learning *enroll.Service, raw string) (auth.Principal, uuid.UUID, error) {
	p, err := requirePrincipal(ctx)
	if err != nil {
		return auth.Principal{}, uuid.Nil, err
	}

	lessonID, err := parseUUID(raw, "lesson")
	if err != nil {
		return auth.Principal{}, uuid.Nil, err
	}

	// CanAuthor deliberately left false: an author reading their own draft has no
	// enrolment, and an attempt without one is a score in nobody's gradebook.
	_, access, err := learning.Lesson(ctx, p.TenantID, lessonID, enroll.Reader{UserID: p.UserID})
	if err != nil {
		return auth.Principal{}, uuid.Nil, enrolError(err)
	}
	if access != enroll.AccessEnrolled {
		return auth.Principal{}, uuid.Nil, huma.Error403Forbidden("Enrol in this course to take its quiz.")
	}
	return p, lessonID, nil
}

// quizAuthor authorises a quiz-authoring write and packages the audit detail.
func quizAuthor(ctx context.Context) (auth.Principal, assess.Author, error) {
	p, err := requirePermission(ctx, auth.PermCourseWrite)
	if err != nil {
		return auth.Principal{}, assess.Author{}, err
	}
	rc := requestContextFrom(ctx)
	return p, assess.Author{UserID: p.UserID, IP: rc.IP, UserAgent: rc.UserAgent}, nil
}

func authorFrom(ctx context.Context, p auth.Principal) assess.Author {
	rc := requestContextFrom(ctx)
	return assess.Author{UserID: p.UserID, IP: rc.IP, UserAgent: rc.UserAgent}
}

func quizView(q assess.LearnerQuiz) QuizView {
	view := QuizView{
		ID: q.ID.String(), Title: q.Title, Description: q.Description,
		TimeLimitSeconds: q.TimeLimitSeconds, MaxAttempts: q.MaxAttempts,
		PassingPercent: q.PassingPercent, TotalPoints: q.TotalPoints,
		Questions: make([]QuestionView, 0, len(q.Questions)),
	}

	for _, question := range q.Questions {
		item := QuestionView{
			ID: question.ID.String(), Type: question.Type, Prompt: question.Prompt,
			Points: question.Points, Position: question.Position, Blanks: question.Blanks,
		}
		for _, o := range question.Options {
			item.Options = append(item.Options, OptionView{ID: o.ID.String(), Content: o.Content})
		}
		for _, m := range question.Matches {
			item.Matches = append(item.Matches, OptionView{ID: m.ID.String(), Content: m.Content})
		}
		view.Questions = append(view.Questions, item)
	}
	return view
}

func quizSettingsView(q assess.Quiz) QuizSettingsView {
	return QuizSettingsView{
		ID: q.ID.String(), LessonID: q.LessonID.String(),
		Title: q.Title, Description: q.Description,
		TimeLimitSeconds: q.TimeLimitSeconds, MaxAttempts: q.MaxAttempts,
		PassingPercent: q.PassingPercent,
	}
}

func authoredQuestionView(q assess.Question) AuthoredQuestionView {
	view := AuthoredQuestionView{
		ID: q.ID.String(), Type: q.Type, Prompt: q.Prompt, Points: q.Points,
		Position: q.Position, Explanation: q.Explanation, CaseSensitive: q.CaseSensitive,
		Accepted: q.Accepted,
	}
	for _, o := range q.Options {
		option := AuthoredOptionView{
			ID: o.ID.String(), Content: o.Content, Position: o.Position, IsCorrect: o.IsCorrect,
		}
		if o.MatchContent != "" {
			option.MatchID, option.MatchContent = o.MatchID.String(), o.MatchContent
		}
		view.Options = append(view.Options, option)
	}
	return view
}

func attemptView(a assess.Attempt) AttemptView {
	return AttemptView{
		Number: a.Number, Status: a.Status,
		StartedAt: a.StartedAt, SubmittedAt: a.SubmittedAt, GradedAt: a.GradedAt,
		ExpiresAt: a.ExpiresAt,
		Points:    a.Points, MaxPoints: a.MaxPoints, Percent: a.Percent(),
		Passed: a.Passed,
	}
}

func attemptViews(attempts []assess.Attempt) []AttemptView {
	views := make([]AttemptView, 0, len(attempts))
	for _, a := range attempts {
		views = append(views, attemptView(a))
	}
	return views
}

// assessError maps the assess package's sentinels onto status codes. This is the
// only place that translation happens; the domain never imports net/http.
func assessError(err error) error {
	switch {
	case errors.Is(err, assess.ErrNotFound):
		// Also the answer for an attempt belonging to somebody else, and for one that
		// is no longer open. A client learns that there is nothing there for them,
		// which is all any of those three mean.
		return huma.Error404NotFound("Not found.")

	case errors.Is(err, assess.ErrNotYourAttempt):
		return huma.Error404NotFound("Not found.")

	case errors.Is(err, assess.ErrQuizExists):
		return huma.Error409Conflict("That lesson already has a quiz.")

	case errors.Is(err, assess.ErrAttemptsExhausted):
		return huma.Error409Conflict("You have used every attempt at this quiz.")

	case errors.Is(err, assess.ErrAttemptClosed), errors.Is(err, assess.ErrAttemptInProgress):
		return huma.Error409Conflict("That attempt has already been submitted.")

	case errors.Is(err, assess.ErrAttemptExpired):
		return huma.Error409Conflict("The time limit for that attempt has passed.")

	case errors.Is(err, assess.ErrEmptyQuiz):
		return huma.Error409Conflict("That quiz has no questions yet.")

	case errors.Is(err, assess.ErrInvalidQuiz),
		errors.Is(err, assess.ErrInvalidQuestion),
		errors.Is(err, assess.ErrIncompleteOrder):
		return huma.Error422UnprocessableEntity(err.Error())

	default:
		// The wrapped cause is logged with a correlation id; the client learns no more.
		return err
	}
}
