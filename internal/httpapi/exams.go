package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ebnsina/muallim-api/internal/auth"
	"github.com/ebnsina/muallim-api/internal/exams"
)

// GradeBandView is one row of a grading scale.
type GradeBandView struct {
	Letter     string  `json:"letter"`
	MinPercent float64 `json:"min_percent"`
	GPAPoint   float64 `json:"gpa_point"`
	IsPass     bool    `json:"is_pass"`
}

// GradingScaleView is a named table of bands.
type GradingScaleView struct {
	ID        string          `json:"id" format:"uuid"`
	Name      string          `json:"name"`
	IsDefault bool            `json:"is_default"`
	Bands     []GradeBandView `json:"bands"`
}

// ExamView is a scheduled assessment.
type ExamView struct {
	ID           string `json:"id" format:"uuid"`
	Name         string `json:"name"`
	TermID       string `json:"term_id,omitempty" format:"uuid"`
	GradeLevelID string `json:"grade_level_id,omitempty" format:"uuid"`
	ScaleID      string `json:"scale_id" format:"uuid"`
	HeldOn       string `json:"held_on,omitempty" format:"date"`
	Status       string `json:"status" enum:"draft,published"`
}

// MarkInput is one subject's score for one student.
type MarkInput struct {
	StudentID string  `json:"student_id" format:"uuid"`
	SubjectID string  `json:"subject_id" format:"uuid"`
	FullMarks float64 `json:"full_marks" minimum:"1"`
	Obtained  float64 `json:"obtained" minimum:"0"`
}

// SubjectResultView is one graded subject on a report card.
type SubjectResultView struct {
	SubjectID   string  `json:"subject_id" format:"uuid"`
	SubjectName string  `json:"subject_name"`
	FullMarks   float64 `json:"full_marks"`
	Obtained    float64 `json:"obtained"`
	Percent     float64 `json:"percent"`
	Letter      string  `json:"letter"`
	GPAPoint    float64 `json:"gpa_point"`
	IsPass      bool    `json:"is_pass"`
}

// ReportCardView is a student's whole result for an exam.
type ReportCardView struct {
	StudentID      string              `json:"student_id" format:"uuid"`
	Subjects       []SubjectResultView `json:"subjects"`
	TotalObtained  float64             `json:"total_obtained"`
	TotalFull      float64             `json:"total_full"`
	AveragePercent float64             `json:"average_percent"`
	GPA            float64             `json:"gpa"`
	OverallLetter  string              `json:"overall_letter"`
	Passed         bool                `json:"passed"`
}

func gradingScaleView(s exams.GradingScale) GradingScaleView {
	v := GradingScaleView{ID: s.ID.String(), Name: s.Name, IsDefault: s.IsDefault}
	v.Bands = make([]GradeBandView, 0, len(s.Bands))
	for _, b := range s.Bands {
		v.Bands = append(v.Bands, GradeBandView{Letter: b.Letter, MinPercent: b.MinPercent, GPAPoint: b.GPAPoint, IsPass: b.IsPass})
	}
	return v
}

func examView(e exams.Exam) ExamView {
	v := ExamView{
		ID: e.ID.String(), Name: e.Name, ScaleID: e.ScaleID.String(), Status: e.Status,
		TermID: uuidPtrString(e.TermID), GradeLevelID: uuidPtrString(e.GradeLevelID),
	}
	if e.HeldOn != nil {
		v.HeldOn = e.HeldOn.Format(dateLayout)
	}
	return v
}

func registerExams(api huma.API, svc *exams.Service) {
	admin := []map[string][]string{{"bearer": {}}}

	huma.Register(api, huma.Operation{
		OperationID: "list-exam-scales",
		Method:      http.MethodGet,
		Path:        "/v1/exam-scales",
		Summary:     "The workspace's grading scales",
		Tags:        []string{"Exams"},
		Security:    admin,
	}, func(ctx context.Context, _ *struct{}) (*struct {
		Body struct {
			Scales []GradingScaleView `json:"scales"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		scales, err := svc.Scales(ctx, p.TenantID)
		if err != nil {
			return nil, examsError(err)
		}
		out := &struct {
			Body struct {
				Scales []GradingScaleView `json:"scales"`
			}
		}{}
		out.Body.Scales = make([]GradingScaleView, 0, len(scales))
		for _, s := range scales {
			out.Body.Scales = append(out.Body.Scales, gradingScaleView(s))
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "ensure-default-exam-scale",
		Method:      http.MethodPost,
		Path:        "/v1/exam-scales/default",
		Summary:     "Get or seed the default grading scale (Bangladesh GPA-5)",
		Description: "Idempotent: returns the workspace's default scale, creating the " +
			"Bangladesh board GPA-5 scale the first time it is asked for.",
		Tags:     []string{"Exams"},
		Security: admin,
	}, func(ctx context.Context, _ *struct{}) (*struct {
		Body struct {
			Scale GradingScaleView `json:"scale"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		scale, err := svc.DefaultScale(ctx, p.TenantID, exams.Author{UserID: p.UserID})
		if err != nil {
			return nil, examsError(err)
		}
		out := &struct {
			Body struct {
				Scale GradingScaleView `json:"scale"`
			}
		}{}
		out.Body.Scale = gradingScaleView(scale)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "create-exam-scale",
		Method:        http.MethodPost,
		Path:          "/v1/exam-scales",
		Summary:       "Create a custom grading scale",
		DefaultStatus: http.StatusCreated,
		Tags:          []string{"Exams"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		Body struct {
			Name      string          `json:"name" minLength:"1" maxLength:"120"`
			IsDefault bool            `json:"is_default,omitempty"`
			Bands     []GradeBandView `json:"bands" minItems:"1" maxItems:"30"`
		}
	}) (*struct {
		Body struct {
			Scale GradingScaleView `json:"scale"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		bands := make([]exams.Band, 0, len(in.Body.Bands))
		for _, b := range in.Body.Bands {
			bands = append(bands, exams.Band{Letter: b.Letter, MinPercent: b.MinPercent, GPAPoint: b.GPAPoint, IsPass: b.IsPass})
		}
		scale, err := svc.CreateScale(ctx, p.TenantID, exams.NewScale{
			Name: in.Body.Name, IsDefault: in.Body.IsDefault, Bands: bands,
		}, exams.Author{UserID: p.UserID})
		if err != nil {
			return nil, examsError(err)
		}
		out := &struct {
			Body struct {
				Scale GradingScaleView `json:"scale"`
			}
		}{}
		out.Body.Scale = gradingScaleView(scale)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-exams",
		Method:      http.MethodGet,
		Path:        "/v1/exams",
		Summary:     "The exam calendar, newest first",
		Description: "Keyset-paginated: pass the next_cursor from the previous page.",
		Tags:        []string{"Exams"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		Limit  int    `query:"limit" minimum:"1" maximum:"100" default:"50"`
		Cursor string `query:"cursor"`
	}) (*struct {
		Body struct {
			Exams      []ExamView `json:"exams"`
			NextCursor string     `json:"next_cursor,omitempty"`
			HasMore    bool       `json:"has_more"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		page, err := svc.Exams(ctx, p.TenantID, exams.PageParams{Limit: in.Limit, Cursor: in.Cursor})
		if err != nil {
			return nil, examsError(err)
		}
		out := &struct {
			Body struct {
				Exams      []ExamView `json:"exams"`
				NextCursor string     `json:"next_cursor,omitempty"`
				HasMore    bool       `json:"has_more"`
			}
		}{}
		out.Body.Exams = make([]ExamView, 0, len(page.Exams))
		for _, e := range page.Exams {
			out.Body.Exams = append(out.Body.Exams, examView(e))
		}
		out.Body.NextCursor = page.NextCursor
		out.Body.HasMore = page.HasMore
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "create-exam",
		Method:        http.MethodPost,
		Path:          "/v1/exams",
		Summary:       "Schedule an exam",
		DefaultStatus: http.StatusCreated,
		Tags:          []string{"Exams"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		Body struct {
			Name         string `json:"name" minLength:"1" maxLength:"160"`
			TermID       string `json:"term_id,omitempty" format:"uuid"`
			GradeLevelID string `json:"grade_level_id,omitempty" format:"uuid"`
			ScaleID      string `json:"scale_id" format:"uuid"`
			HeldOn       string `json:"held_on,omitempty" format:"date"`
		}
	}) (*struct {
		Body struct {
			Exam ExamView `json:"exam"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		scaleID, err := parseUUID(in.Body.ScaleID, "grading scale")
		if err != nil {
			return nil, err
		}
		term, err := optionalUUIDPtr(in.Body.TermID, "term")
		if err != nil {
			return nil, err
		}
		grade, err := optionalUUIDPtr(in.Body.GradeLevelID, "grade level")
		if err != nil {
			return nil, err
		}
		held, err := optionalDate(in.Body.HeldOn)
		if err != nil {
			return nil, err
		}
		exam, err := svc.CreateExam(ctx, p.TenantID, exams.NewExam{
			Name: in.Body.Name, TermID: term, GradeLevelID: grade, ScaleID: scaleID, HeldOn: held,
		}, exams.Author{UserID: p.UserID})
		if err != nil {
			return nil, examsError(err)
		}
		out := &struct {
			Body struct {
				Exam ExamView `json:"exam"`
			}
		}{}
		out.Body.Exam = examView(exam)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "publish-exam",
		Method:      http.MethodPost,
		Path:        "/v1/exams/{id}/publish",
		Summary:     "Publish an exam, fixing its results",
		Tags:        []string{"Exams"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*struct {
		Body struct {
			Exam ExamView `json:"exam"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		examID, err := parseUUID(in.ID, "exam")
		if err != nil {
			return nil, err
		}
		exam, err := svc.PublishExam(ctx, p.TenantID, examID, exams.Author{UserID: p.UserID})
		if err != nil {
			return nil, examsError(err)
		}
		out := &struct {
			Body struct {
				Exam ExamView `json:"exam"`
			}
		}{}
		out.Body.Exam = examView(exam)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "delete-exam",
		Method:        http.MethodDelete,
		Path:          "/v1/exams/{id}",
		Summary:       "Remove an exam and its marks",
		DefaultStatus: http.StatusNoContent,
		Tags:          []string{"Exams"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*struct{}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		examID, err := parseUUID(in.ID, "exam")
		if err != nil {
			return nil, err
		}
		if err := svc.DeleteExam(ctx, p.TenantID, examID); err != nil {
			return nil, examsError(err)
		}
		return &struct{}{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "enter-marks",
		Method:      http.MethodPost,
		Path:        "/v1/exams/{id}/marks",
		Summary:     "Enter a batch of subject marks",
		Description: "Upserts one row per (student, subject). Only while the exam is a draft.",
		Tags:        []string{"Exams"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id" format:"uuid"`
		Body struct {
			Marks []MarkInput `json:"marks" minItems:"1" maxItems:"2000"`
		}
	}) (*struct {
		Body struct {
			Entered int `json:"entered"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		examID, err := parseUUID(in.ID, "exam")
		if err != nil {
			return nil, err
		}
		marks := make([]exams.Mark, 0, len(in.Body.Marks))
		for _, m := range in.Body.Marks {
			studentID, err := parseUUID(m.StudentID, "student")
			if err != nil {
				return nil, err
			}
			subjectID, err := parseUUID(m.SubjectID, "subject")
			if err != nil {
				return nil, err
			}
			marks = append(marks, exams.Mark{
				StudentID: studentID, SubjectID: subjectID, FullMarks: m.FullMarks, Obtained: m.Obtained,
			})
		}
		entered, err := svc.EnterMarks(ctx, p.TenantID, examID, marks, exams.Author{UserID: p.UserID})
		if err != nil {
			return nil, examsError(err)
		}
		out := &struct {
			Body struct {
				Entered int `json:"entered"`
			}
		}{}
		out.Body.Entered = entered
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "student-report-card",
		Method:      http.MethodGet,
		Path:        "/v1/exams/{id}/students/{student_id}/report-card",
		Summary:     "A student's report card for an exam",
		Description: "Computed from the student's marks against the exam's scale — total, " +
			"average, GPA, and pass/fail.",
		Tags:     []string{"Exams"},
		Security: admin,
	}, func(ctx context.Context, in *struct {
		ID        string `path:"id" format:"uuid"`
		StudentID string `path:"student_id" format:"uuid"`
	}) (*struct {
		Body struct {
			ReportCard ReportCardView `json:"report_card"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		examID, err := parseUUID(in.ID, "exam")
		if err != nil {
			return nil, err
		}
		studentID, err := parseUUID(in.StudentID, "student")
		if err != nil {
			return nil, err
		}
		rc, err := svc.ReportCard(ctx, p.TenantID, examID, studentID)
		if err != nil {
			return nil, examsError(err)
		}
		out := &struct {
			Body struct {
				ReportCard ReportCardView `json:"report_card"`
			}
		}{}
		out.Body.ReportCard = reportCardView(rc)
		return out, nil
	})
}

func reportCardView(rc exams.ReportCard) ReportCardView {
	v := ReportCardView{
		StudentID: rc.StudentID.String(), TotalObtained: rc.TotalObtained, TotalFull: rc.TotalFull,
		AveragePercent: rc.AveragePercent, GPA: rc.GPA, OverallLetter: rc.OverallLetter, Passed: rc.Passed,
	}
	v.Subjects = make([]SubjectResultView, 0, len(rc.Subjects))
	for _, s := range rc.Subjects {
		v.Subjects = append(v.Subjects, SubjectResultView{
			SubjectID: s.SubjectID.String(), SubjectName: s.SubjectName, FullMarks: s.FullMarks,
			Obtained: s.Obtained, Percent: s.Percent, Letter: s.Letter, GPAPoint: s.GPAPoint, IsPass: s.IsPass,
		})
	}
	return v
}

// optionalDate parses an optional calendar date: empty leaves it nil.
func optionalDate(raw string) (*time.Time, error) {
	if raw == "" {
		return nil, nil
	}
	t, err := time.Parse(dateLayout, raw)
	if err != nil {
		return nil, huma.Error422UnprocessableEntity("held_on must be a calendar date, YYYY-MM-DD.")
	}
	return &t, nil
}

// examsError maps the exams package's sentinels onto status codes.
func examsError(err error) error {
	switch {
	case errors.Is(err, exams.ErrNotFound):
		return huma.Error404NotFound("Not found.")
	case errors.Is(err, exams.ErrExamPublished):
		return huma.Error409Conflict("A published exam's marks cannot be changed.")
	case errors.Is(err, exams.ErrNoScale):
		return huma.Error422UnprocessableEntity("This exam's grading scale has no bands.")
	case errors.Is(err, exams.ErrUngraded):
		return huma.Error422UnprocessableEntity(err.Error())
	case errors.Is(err, exams.ErrInvalidPage):
		return huma.Error422UnprocessableEntity("That page cursor is not valid.")
	case errors.Is(err, exams.ErrInvalidScale),
		errors.Is(err, exams.ErrInvalidExam),
		errors.Is(err, exams.ErrInvalidMark):
		return huma.Error422UnprocessableEntity(err.Error())
	default:
		return err
	}
}
