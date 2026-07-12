package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/ebnsina/muallim-api/internal/assign"
	"github.com/ebnsina/muallim-api/internal/auth"
	"github.com/ebnsina/muallim-api/internal/enroll"
	"github.com/ebnsina/muallim-api/internal/platform/blob"
	"github.com/ebnsina/muallim-api/internal/tenant"
)

// Like a quiz, an assignment is addressed under its lesson, and a learner's own
// submission has no id in any path. The access check that guards the lesson
// guards the assignment, with no second rule to keep in step.
//
// A submission id does appear — in the marking queue, and only there. A marker
// works across learners and needs one; a learner never does.

// AssignmentView is an assignment as anybody who may read the lesson sees it.
type AssignmentView struct {
	ID           string `json:"id" format:"uuid"`
	Title        string `json:"title"`
	Instructions string `json:"instructions,omitempty"`

	Points        int `json:"points"`
	PassingPoints int `json:"passing_points"`

	MaxFiles int   `json:"max_files"`
	MaxBytes int64 `json:"max_bytes" doc:"The largest a single file may be."`

	DueAt     *time.Time `json:"due_at,omitempty"`
	AllowLate bool       `json:"allow_late"`
}

// FileView is one uploaded file. The object key is never in it: it is an internal
// address, and a client that had one could ask the object store for it directly.
type FileView struct {
	ID          string    `json:"id" format:"uuid"`
	Filename    string    `json:"filename"`
	ContentType string    `json:"content_type,omitempty"`
	Bytes       int64     `json:"bytes"`
	UploadedAt  time.Time `json:"uploaded_at"`
}

// AssignmentSubmissionView is a learner's own submission.
//
// Named for its domain, because `assess` already has a SubmissionView and the two
// mean different things: one is a quiz attempt, the other is work in a bucket.
type AssignmentSubmissionView struct {
	Status string `json:"status" enum:"draft,submitted,graded"`

	SubmittedAt *time.Time `json:"submitted_at,omitempty"`
	GradedAt    *time.Time `json:"graded_at,omitempty"`

	// Points is absent until a person has marked it. A grade is not a thing to
	// guess at.
	Points   *int   `json:"points,omitempty"`
	Feedback string `json:"feedback,omitempty"`

	Late bool `json:"late"`

	Files []FileView `json:"files"`
}

// GetAssignmentOutput is the assignment and, for a learner, their own submission.
type GetAssignmentOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Assignment AssignmentView `json:"assignment"`

		// Absent when the learner has not started, or is not signed in.
		Submission *AssignmentSubmissionView `json:"submission,omitempty"`
	}
}

// AssignmentOnlyOutput confirms an authoring write.
type AssignmentOnlyOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Assignment AssignmentView `json:"assignment"`
	}
}

// SubmissionOnlyOutput confirms a learner's write.
type SubmissionOnlyOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Submission AssignmentSubmissionView `json:"submission"`
	}
}

// PresignOutput is everything a browser needs to upload one file, and nothing else.
type PresignOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		// UploadURL takes exactly one object of exactly the declared size. The bytes
		// go straight to the object store; they never pass through this API.
		UploadURL string `json:"upload_url" format:"uri"`
		Method    string `json:"method" enum:"PUT"`

		// Headers must be sent verbatim. They are part of what was signed.
		Headers map[string][]string `json:"headers"`

		// Key identifies the object. Send it back to confirm the upload.
		Key string `json:"key"`

		ExpiresAt time.Time `json:"expires_at"`
	}
}

// FileOutput confirms an upload.
type FileOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		File FileView `json:"file"`
	}
}

// DownloadOutput is a short-lived URL for one file.
//
// A URL and not a redirect. A 302 to a signed URL puts the signature in the
// browser's history, in the referrer, and in every proxy log between here and
// there; a JSON body puts it in one response nobody stores.
type DownloadOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		URL       string    `json:"url" format:"uri"`
		ExpiresAt time.Time `json:"expires_at"`
	}
}

// MarkingView is one submission in the marking queue.
type MarkingView struct {
	// ID appears here and nowhere else. A learner reaches their own submission
	// through the lesson, so no id of theirs is ever guessable; a marker works
	// across learners and holds submission:grade.
	ID string `json:"id" format:"uuid"`

	LearnerName  string `json:"learner_name"`
	LearnerEmail string `json:"learner_email" format:"email"`

	Status      string     `json:"status" enum:"submitted,graded"`
	SubmittedAt *time.Time `json:"submitted_at,omitempty"`
	Late        bool       `json:"late"`
	Points      *int       `json:"points,omitempty"`
}

// ListMarkingOutput is a marking queue.
type ListMarkingOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Submissions []MarkingView `json:"submissions"`
	}
}

// MarkedSubmissionOutput is one submission, with its files, for its marker.
type MarkedSubmissionOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Assignment AssignmentView           `json:"assignment"`
		Submission AssignmentSubmissionView `json:"submission"`

		LearnerID string `json:"learner_id" format:"uuid"`
	}
}

// DeadlineView is one piece of work a learner still owes.
//
// `overdue` is computed here and sent, rather than left to a client's clock: a
// browser's idea of "now" is whatever its owner set it to, and a deadline is not
// a thing to let the reader decide about.
type DeadlineView struct {
	AssignmentID string `json:"assignment_id" format:"uuid"`
	LessonID     string `json:"lesson_id" format:"uuid"`
	Title        string `json:"title"`

	CourseSlug  string `json:"course_slug"`
	CourseTitle string `json:"course_title"`

	DueAt     time.Time `json:"due_at"`
	Overdue   bool      `json:"overdue"`
	AllowLate bool      `json:"allow_late"`
}

// ListDeadlinesOutput is what a learner owes, soonest first.
type ListDeadlinesOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Deadlines []DeadlineView `json:"deadlines"`
	}
}

// DeadlineWindow is how far back and forward the learner's list looks.
//
// Back a week, because a deadline missed on Monday is the one they most need on
// Tuesday. Forward six weeks, because a date further off than that is not a thing
// anybody does anything about today.
const (
	deadlinesLookBack   = 7 * 24 * time.Hour
	deadlinesLookAhead  = 42 * 24 * time.Hour
	deadlinesCacheEntry = "private, no-store"
)

func registerAssignments(api huma.API, svc *assign.Service, learning *enroll.Service) {
	huma.Register(api, huma.Operation{
		OperationID: "list-my-deadlines",
		Method:      http.MethodGet,
		Path:        "/v1/me/deadlines",
		Summary:     "What I still owe, and when",
		Description: "Unsubmitted assignments with a due date, across the courses I am " +
			"enrolled on, from a week ago to six weeks out. One query, soonest first.",
		Tags:     []string{"Learning"},
		Security: []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		Limit int `query:"limit" minimum:"1" maximum:"20" default:"10"`
	}) (*ListDeadlinesOutput, error) {
		p, err := requirePrincipal(ctx)
		if err != nil {
			return nil, err
		}

		now := time.Now()
		deadlines, err := svc.Deadlines(ctx, p.TenantID, p.UserID,
			now.Add(-deadlinesLookBack), now.Add(deadlinesLookAhead), in.Limit)
		if err != nil {
			return nil, assignError(err)
		}

		out := &ListDeadlinesOutput{CacheControl: deadlinesCacheEntry}
		out.Body.Deadlines = make([]DeadlineView, 0, len(deadlines))
		for _, d := range deadlines {
			out.Body.Deadlines = append(out.Body.Deadlines, DeadlineView{
				AssignmentID: d.AssignmentID.String(),
				LessonID:     d.LessonID.String(),
				Title:        d.Title,
				CourseSlug:   d.CourseSlug,
				CourseTitle:  d.CourseTitle,
				DueAt:        d.DueAt,
				Overdue:      d.Overdue(now),
				AllowLate:    d.AllowLate,
			})
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-assignment",
		Method:      http.MethodGet,
		Path:        "/v1/lessons/{id}/assignment",
		Summary:     "Read a lesson's assignment",
		Description: "Whoever may read the lesson may read its assignment; a reader who may not receives " +
			"404. An enrolled learner also gets their own submission.",
		Tags: []string{"Assignments"},
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*GetAssignmentOutput, error) {
		lessonID, err := readableLesson(ctx, learning, in.ID)
		if err != nil {
			return nil, err
		}

		out := &GetAssignmentOutput{CacheControl: lessonCacheControl}

		principal, signedIn := principalFrom(ctx)
		if !signedIn {
			assignment, err := svc.Assignment(ctx, tenant.ID(ctx), lessonID)
			if err != nil {
				return nil, assignError(err)
			}
			out.Body.Assignment = assignmentView(assignment)
			return out, nil
		}

		assignment, submission, err := svc.MySubmission(ctx, principal.TenantID, lessonID, principal.UserID)
		if err != nil {
			return nil, assignError(err)
		}

		out.Body.Assignment = assignmentView(assignment)
		if submission.ID != uuid.Nil {
			view := submissionView(submission)
			out.Body.Submission = &view
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "presign-upload",
		Method:        http.MethodPost,
		Path:          "/v1/lessons/{id}/assignment/uploads",
		Summary:       "Ask for a URL to upload one file to",
		DefaultStatus: http.StatusCreated,
		Description: "Returns a URL that accepts exactly one object of exactly the declared size, for " +
			"fifteen minutes. The bytes go straight to the object store and never pass through this " +
			"API. Nothing is recorded until you confirm the upload.",
		Tags:     []string{"Assignments"},
		Security: []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id" format:"uuid"`
		Body struct {
			Filename string `json:"filename" minLength:"1" maxLength:"512"`
			Bytes    int64  `json:"bytes" minimum:"1" maximum:"1073741824"`
		}
	}) (*PresignOutput, error) {
		p, lessonID, err := enrolledLearner(ctx, learning, in.ID)
		if err != nil {
			return nil, err
		}

		upload, key, err := svc.PresignUpload(ctx, p.TenantID, lessonID, p.UserID, in.Body.Filename, in.Body.Bytes)
		if err != nil {
			return nil, assignError(err)
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
		OperationID:   "confirm-upload",
		Method:        http.MethodPost,
		Path:          "/v1/lessons/{id}/assignment/files",
		Summary:       "Confirm a file you uploaded",
		DefaultStatus: http.StatusCreated,
		Description: "The object store is asked what is really at that key before anything is recorded. " +
			"A key that is not yours, or that nothing was uploaded to, is refused.",
		Tags:     []string{"Assignments"},
		Security: []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id" format:"uuid"`
		Body struct {
			Key      string `json:"key" minLength:"1" maxLength:"1024"`
			Filename string `json:"filename" minLength:"1" maxLength:"512"`
		}
	}) (*FileOutput, error) {
		p, lessonID, err := enrolledLearner(ctx, learning, in.ID)
		if err != nil {
			return nil, err
		}

		file, err := svc.AttachFile(ctx, p.TenantID, lessonID, p.UserID, in.Body.Key, in.Body.Filename)
		if err != nil {
			return nil, assignError(err)
		}

		out := &FileOutput{CacheControl: lessonCacheControl}
		out.Body.File = fileView(file)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "remove-upload",
		Method:        http.MethodDelete,
		Path:          "/v1/assignment-files/{id}",
		Summary:       "Remove a file from your draft",
		DefaultStatus: http.StatusNoContent,
		Description:   "Only from a draft, and only your own. The object is deleted by a background job.",
		Tags:          []string{"Assignments"},
		Security:      []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*struct{}, error) {
		p, err := requirePrincipal(ctx)
		if err != nil {
			return nil, err
		}
		fileID, err := parseUUID(in.ID, "file")
		if err != nil {
			return nil, err
		}

		if err := svc.RemoveFile(ctx, p.TenantID, fileID, p.UserID); err != nil {
			return nil, assignError(err)
		}
		return &struct{}{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "download-assignment-file",
		Method:      http.MethodGet,
		Path:        "/v1/assignment-files/{id}/download",
		Summary:     "Get a short-lived URL for a file",
		Description: "Readable by the learner who uploaded it, and by anybody who may mark it. The URL " +
			"lives five minutes and always downloads as an attachment, never inline.",
		Tags:     []string{"Assignments"},
		Security: []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*DownloadOutput, error) {
		p, err := requirePrincipal(ctx)
		if err != nil {
			return nil, err
		}
		fileID, err := parseUUID(in.ID, "file")
		if err != nil {
			return nil, err
		}

		url, err := svc.DownloadURL(ctx, p.TenantID, fileID, p.UserID, p.Can(auth.PermSubmissionGrade))
		if err != nil {
			return nil, assignError(err)
		}

		out := &DownloadOutput{CacheControl: "private, no-store"}
		out.Body.URL = url
		out.Body.ExpiresAt = time.Now().Add(assign.DownloadTTL)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "submit-assignment",
		Method:      http.MethodPost,
		Path:        "/v1/lessons/{id}/assignment/submit",
		Summary:     "Hand your work in",
		Description: "Freezes the files. Lateness is recorded against the deadline as it stands now, so " +
			"moving the deadline afterwards changes nobody's standing.",
		Tags:     []string{"Assignments"},
		Security: []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*SubmissionOnlyOutput, error) {
		p, lessonID, err := enrolledLearner(ctx, learning, in.ID)
		if err != nil {
			return nil, err
		}

		submission, err := svc.Submit(ctx, p.TenantID, lessonID, p.UserID, assignAuthor(ctx, p))
		if err != nil {
			return nil, assignError(err)
		}

		out := &SubmissionOnlyOutput{CacheControl: lessonCacheControl}
		out.Body.Submission = submissionView(submission)
		return out, nil
	})

	registerAssignmentAuthoring(api, svc)
	registerAssignmentMarking(api, svc)
}

func registerAssignmentAuthoring(api huma.API, svc *assign.Service) {
	huma.Register(api, huma.Operation{
		OperationID:   "create-assignment",
		Method:        http.MethodPost,
		Path:          "/v1/lessons/{id}/assignment",
		Summary:       "Attach an assignment to a lesson",
		DefaultStatus: http.StatusCreated,
		Description:   "A lesson has at most one.",
		Tags:          []string{"Authoring"},
		Security:      []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id" format:"uuid"`
		Body struct {
			Title         string     `json:"title" minLength:"1" maxLength:"200"`
			Instructions  string     `json:"instructions,omitempty" maxLength:"8000"`
			Points        int        `json:"points,omitempty" minimum:"0" maximum:"1000" default:"100"`
			PassingPoints int        `json:"passing_points,omitempty" minimum:"0" maximum:"1000"`
			MaxFiles      int        `json:"max_files,omitempty" minimum:"1" maximum:"20" default:"3"`
			MaxBytes      int64      `json:"max_bytes,omitempty" minimum:"1" maximum:"1073741824" default:"26214400"`
			DueAt         *time.Time `json:"due_at,omitempty"`
			AllowLate     bool       `json:"allow_late,omitempty" default:"true"`
		}
	}) (*AssignmentOnlyOutput, error) {
		p, author, err := assignmentAuthor(ctx)
		if err != nil {
			return nil, err
		}
		lessonID, err := parseUUID(in.ID, "lesson")
		if err != nil {
			return nil, err
		}

		assignment, err := svc.CreateAssignment(ctx, p.TenantID, lessonID, assign.NewAssignment{
			Title: in.Body.Title, Instructions: in.Body.Instructions,
			Points: in.Body.Points, PassingPoints: in.Body.PassingPoints,
			MaxFiles: in.Body.MaxFiles, MaxBytes: in.Body.MaxBytes,
			DueAt: in.Body.DueAt, AllowLate: in.Body.AllowLate,
		}, author)
		if err != nil {
			return nil, assignError(err)
		}

		out := &AssignmentOnlyOutput{CacheControl: lessonCacheControl}
		out.Body.Assignment = assignmentView(assignment)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "update-assignment",
		Method:      http.MethodPatch,
		Path:        "/v1/lessons/{id}/assignment",
		Summary:     "Change an assignment",
		Description: "An omitted field is left alone. The patch is checked against the assignment it " +
			"produces, so lowering the points below the pass mark is refused however many requests it takes.",
		Tags:     []string{"Authoring"},
		Security: []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id" format:"uuid"`
		Body struct {
			Title         *string `json:"title,omitempty" minLength:"1" maxLength:"200"`
			Instructions  *string `json:"instructions,omitempty" maxLength:"8000"`
			Points        *int    `json:"points,omitempty" minimum:"0" maximum:"1000"`
			PassingPoints *int    `json:"passing_points,omitempty" minimum:"0" maximum:"1000"`
			MaxFiles      *int    `json:"max_files,omitempty" minimum:"1" maximum:"20"`
			MaxBytes      *int64  `json:"max_bytes,omitempty" minimum:"1" maximum:"1073741824"`
			AllowLate     *bool   `json:"allow_late,omitempty"`

			// `null` erases the deadline; an absent field leaves it where it is.
			DueAt Optional[time.Time] `json:"due_at,omitempty" doc:"An instant, or null to remove the deadline."`
		}
	}) (*AssignmentOnlyOutput, error) {
		p, author, err := assignmentAuthor(ctx)
		if err != nil {
			return nil, err
		}
		lessonID, err := parseUUID(in.ID, "lesson")
		if err != nil {
			return nil, err
		}

		patch := assign.AssignmentPatch{
			Title: in.Body.Title, Instructions: in.Body.Instructions,
			Points: in.Body.Points, PassingPoints: in.Body.PassingPoints,
			MaxFiles: in.Body.MaxFiles, MaxBytes: in.Body.MaxBytes,
			AllowLate: in.Body.AllowLate,
		}
		if in.Body.DueAt.Sent {
			if in.Body.DueAt.Null {
				patch.ClearDueAt = true
			} else {
				patch.DueAt = &in.Body.DueAt.Value
			}
		}

		assignment, err := svc.EditAssignment(ctx, p.TenantID, lessonID, patch, author)
		if err != nil {
			return nil, assignError(err)
		}

		out := &AssignmentOnlyOutput{CacheControl: lessonCacheControl}
		out.Body.Assignment = assignmentView(assignment)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "delete-assignment",
		Method:        http.MethodDelete,
		Path:          "/v1/lessons/{id}/assignment",
		Summary:       "Remove a lesson's assignment",
		DefaultStatus: http.StatusNoContent,
		Description:   "Deletes every submission, and queues every uploaded file for deletion.",
		Tags:          []string{"Authoring"},
		Security:      []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*struct{}, error) {
		p, author, err := assignmentAuthor(ctx)
		if err != nil {
			return nil, err
		}
		lessonID, err := parseUUID(in.ID, "lesson")
		if err != nil {
			return nil, err
		}

		if err := svc.RemoveAssignment(ctx, p.TenantID, lessonID, author); err != nil {
			return nil, assignError(err)
		}
		return &struct{}{}, nil
	})
}

func registerAssignmentMarking(api huma.API, svc *assign.Service) {
	huma.Register(api, huma.Operation{
		OperationID: "list-assignment-submissions",
		Method:      http.MethodGet,
		Path:        "/v1/lessons/{id}/assignment/submissions",
		Summary:     "List what has been handed in",
		Description: "Drafts are never here: a marker has no business reading work nobody has finished. " +
			"Requires submission:grade.",
		Tags:     []string{"Marking"},
		Security: []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID       string `path:"id" format:"uuid"`
		Awaiting bool   `query:"awaiting" doc:"Only what is still waiting for a person."`
		Limit    int    `query:"limit" minimum:"1" maximum:"50" default:"50"`
	}) (*ListMarkingOutput, error) {
		p, _, err := marker(ctx)
		if err != nil {
			return nil, err
		}
		lessonID, err := parseUUID(in.ID, "lesson")
		if err != nil {
			return nil, err
		}

		markings, err := svc.Submissions(ctx, p.TenantID, lessonID, in.Awaiting, in.Limit)
		if err != nil {
			return nil, assignError(err)
		}

		out := &ListMarkingOutput{CacheControl: lessonCacheControl}
		out.Body.Submissions = make([]MarkingView, 0, len(markings))
		for _, m := range markings {
			out.Body.Submissions = append(out.Body.Submissions, MarkingView{
				ID:           m.Submission.ID.String(),
				LearnerName:  m.LearnerName,
				LearnerEmail: m.LearnerEmail,
				Status:       m.Submission.Status,
				SubmittedAt:  m.Submission.SubmittedAt,
				Late:         m.Submission.Late,
				Points:       m.Submission.Points,
			})
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "read-assignment-submission",
		Method:      http.MethodGet,
		Path:        "/v1/assignment-submissions/{id}",
		Summary:     "Read one submission for marking",
		Description: "Its files, and what it is worth. Requires submission:grade.",
		Tags:        []string{"Marking"},
		Security:    []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*MarkedSubmissionOutput, error) {
		p, _, err := marker(ctx)
		if err != nil {
			return nil, err
		}
		submissionID, err := parseUUID(in.ID, "submission")
		if err != nil {
			return nil, err
		}

		assignment, submission, err := svc.Submission(ctx, p.TenantID, submissionID)
		if err != nil {
			return nil, assignError(err)
		}

		out := &MarkedSubmissionOutput{CacheControl: lessonCacheControl}
		out.Body.Assignment = assignmentView(assignment)
		out.Body.Submission = submissionView(submission)
		out.Body.LearnerID = submission.UserID.String()
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "mark-assignment",
		Method:      http.MethodPut,
		Path:        "/v1/assignment-submissions/{id}/mark",
		Summary:     "Mark a submission",
		Description: "Only what has been handed in. Clearing the pass mark completes the lesson, in the " +
			"transaction that recorded the grade.",
		Tags:     []string{"Marking"},
		Security: []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id" format:"uuid"`
		Body struct {
			Points   int    `json:"points" minimum:"0" maximum:"1000"`
			Feedback string `json:"feedback,omitempty" maxLength:"8000"`
		}
	}) (*SubmissionOnlyOutput, error) {
		p, author, err := marker(ctx)
		if err != nil {
			return nil, err
		}
		submissionID, err := parseUUID(in.ID, "submission")
		if err != nil {
			return nil, err
		}

		marked, err := svc.Mark(ctx, p.TenantID, submissionID, in.Body.Points, in.Body.Feedback,
			assign.Author{UserID: author.UserID, IP: author.IP, UserAgent: author.UserAgent})
		if err != nil {
			return nil, assignError(err)
		}

		out := &SubmissionOnlyOutput{CacheControl: lessonCacheControl}
		out.Body.Submission = submissionView(marked)
		return out, nil
	})
}

// assignmentAuthor authorises an authoring write and packages the audit detail.
func assignmentAuthor(ctx context.Context) (auth.Principal, assign.Author, error) {
	p, err := requirePermission(ctx, auth.PermCourseWrite)
	if err != nil {
		return auth.Principal{}, assign.Author{}, err
	}
	rc := requestContextFrom(ctx)
	return p, assign.Author{UserID: p.UserID, IP: rc.IP, UserAgent: rc.UserAgent}, nil
}

func assignAuthor(ctx context.Context, p auth.Principal) assign.Author {
	rc := requestContextFrom(ctx)
	return assign.Author{UserID: p.UserID, IP: rc.IP, UserAgent: rc.UserAgent}
}

func assignmentView(a assign.Assignment) AssignmentView {
	return AssignmentView{
		ID: a.ID.String(), Title: a.Title, Instructions: a.Instructions,
		Points: a.Points, PassingPoints: a.PassingPoints,
		MaxFiles: a.MaxFiles, MaxBytes: a.MaxBytes,
		DueAt: a.DueAt, AllowLate: a.AllowLate,
	}
}

func fileView(f assign.File) FileView {
	return FileView{
		ID: f.ID.String(), Filename: f.Filename, ContentType: f.ContentType,
		Bytes: f.Bytes, UploadedAt: f.UploadedAt,
	}
}

func submissionView(s assign.Submission) AssignmentSubmissionView {
	view := AssignmentSubmissionView{
		Status: s.Status, SubmittedAt: s.SubmittedAt, GradedAt: s.GradedAt,
		Points: s.Points, Feedback: s.Feedback, Late: s.Late,
		Files: make([]FileView, 0, len(s.Files)),
	}
	for _, file := range s.Files {
		view.Files = append(view.Files, fileView(file))
	}
	return view
}

// assignError maps the assign package's sentinels onto status codes. This is the
// only place that translation happens; the domain never imports net/http.
func assignError(err error) error {
	switch {
	case errors.Is(err, assign.ErrNotFound):
		return huma.Error404NotFound("Not found.")

	case errors.Is(err, assign.ErrNotYours):
		// A key belonging to somebody else is a key that does not exist, as far as
		// whoever asked is concerned. The difference between the two is precisely
		// the fact a probe would be after.
		return huma.Error404NotFound("Not found.")

	case errors.Is(err, assign.ErrAssignmentExists):
		return huma.Error409Conflict("That lesson already has an assignment.")

	case errors.Is(err, assign.ErrAlreadySubmitted):
		return huma.Error409Conflict("You have already handed that in.")

	case errors.Is(err, assign.ErrNothingToSubmit):
		return huma.Error409Conflict("Attach at least one file before handing in.")

	case errors.Is(err, assign.ErrNotSubmitted):
		return huma.Error409Conflict("That work has not been handed in yet.")

	case errors.Is(err, assign.ErrTooManyFiles):
		return huma.Error409Conflict(err.Error())

	case errors.Is(err, assign.ErrPastDue):
		return huma.Error409Conflict("The deadline for that assignment has passed.")

	case errors.Is(err, assign.ErrUploadMissing):
		// 409: the request was understood, and the world is not in the state it
		// describes. Nothing was uploaded to that key, so there is nothing to record.
		return huma.Error409Conflict("Nothing was uploaded to that key.")

	case errors.Is(err, assign.ErrInvalidAssignment),
		errors.Is(err, assign.ErrInvalidFile),
		errors.Is(err, assign.ErrInvalidGrade):
		return huma.Error422UnprocessableEntity(err.Error())

	case errors.Is(err, blob.ErrNotConfigured):
		// 503, not 500. Nothing is broken; this deployment has no bucket, and no
		// retry of this request will change that until somebody configures one.
		return huma.Error503ServiceUnavailable("This workspace cannot accept uploads yet.")

	default:
		// The wrapped cause is logged with a correlation id; the client learns no more.
		return err
	}
}
