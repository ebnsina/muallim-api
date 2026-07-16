package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/ebnsina/muallim-api/internal/auth"
	"github.com/ebnsina/muallim-api/internal/grade"
)

// NewBand is one band of a scale being created.
//
// A named type, not an anonymous struct in the request body. Huma derives a schema
// name from the field's element type, and an anonymous one lands on "Item" — which
// a quiz option already answers to. The collision panics at registration, which is
// the good version of that bug.
type NewBand struct {
	Label  string `json:"label" minLength:"1" maxLength:"40"`
	Min    int    `json:"min_percent" minimum:"0" maximum:"100"`
	IsPass bool   `json:"is_pass,omitempty"`
}

// BandView is one step of a grading scale.
type BandView struct {
	Label  string `json:"label"`
	Min    int    `json:"min_percent" doc:"Everything at or above this, and below the band over it."`
	IsPass bool   `json:"is_pass"`
}

// ScaleView is a grading scale. The built-in default has no id: it is not a row,
// and there is nothing to address.
type ScaleView struct {
	ID      string     `json:"id,omitempty" format:"uuid"`
	Name    string     `json:"name"`
	Builtin bool       `json:"builtin" doc:"True for the scale this system ships. It cannot be edited or deleted."`
	Bands   []BandView `json:"bands"`
}

// ItemView is one thing worth marks in a course.
type ItemView struct {
	ID        string `json:"id" format:"uuid"`
	LessonID  string `json:"lesson_id" format:"uuid"`
	Source    string `json:"source" enum:"quiz,assignment"`
	Title     string `json:"title"`
	MaxPoints int    `json:"max_points"`
}

// EntryView is one mark. `max_points` is what the item was worth when it was
// marked, which is not always what it is worth now.
type EntryView struct {
	ItemID    string    `json:"item_id" format:"uuid"`
	Points    int       `json:"points"`
	MaxPoints int       `json:"max_points"`
	GradedAt  time.Time `json:"graded_at"`
}

// ResultView is a course grade.
type ResultView struct {
	// Graded of ItemCount. A learner looking at "72%" wants to know 72% of what.
	Graded    int `json:"graded"`
	ItemCount int `json:"item_count"`

	Points    int `json:"points"`
	MaxPoints int `json:"max_points"`
	Percent   int `json:"percent"`

	// Absent until something has been marked. Nothing graded is not a zero, and an
	// F for work nobody has been asked to hand in is a lie a learner will believe.
	Band *BandView `json:"band,omitempty"`
}

// MyGradesOutput is a learner's own grades in one course.
type MyGradesOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Scale   ScaleView   `json:"scale"`
		Items   []ItemView  `json:"items"`
		Entries []EntryView `json:"entries"`
		Result  ResultView  `json:"result"`
	}
}

// LearnerRowView is one learner's row of a gradebook.
type LearnerRowView struct {
	UserID string `json:"user_id" format:"uuid"`
	Name   string `json:"name"`
	Email  string `json:"email" format:"email"`

	Entries []EntryView `json:"entries"`
	Result  ResultView  `json:"result"`
}

// GradebookOutput is the whole course, for whoever may mark it.
type GradebookOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Scale    ScaleView        `json:"scale"`
		Items    []ItemView       `json:"items"`
		Learners []LearnerRowView `json:"learners"`
	}
}

// ScalesOutput lists a workspace's grading scales, the built-in one first.
type ScalesOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Scales []ScaleView `json:"scales"`
	}
}

// ScaleOutput confirms a write.
type ScaleOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Scale ScaleView `json:"scale"`
	}
}

func registerGrades(api huma.API, svc *grade.Service) {
	huma.Register(api, huma.Operation{
		OperationID: "my-grades",
		Method:      http.MethodGet,
		Path:        "/v1/courses/{slug}/grades",
		Summary:     "Your own grades in a course",
		Description: "Every assessment in the course, and your mark against the ones that have been " +
			"graded. The percentage covers what has been marked, not what has not been attempted.",
		Tags:     []string{"Grades"},
		Security: []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		Slug string `path:"slug" maxLength:"200"`
	}) (*MyGradesOutput, error) {
		p, err := requirePrincipal(ctx)
		if err != nil {
			return nil, err
		}

		// The learner is the caller. No path here takes a user id from a request.
		grades, err := svc.LearnerGrades(ctx, p.TenantID, in.Slug, p.UserID)
		if err != nil {
			return nil, gradeError(err)
		}

		out := &MyGradesOutput{CacheControl: "private, no-store"}
		out.Body.Scale = scaleView(grades.Scale)
		out.Body.Items = itemViews(grades.Items)
		out.Body.Entries = entryViews(grades.Entries)
		out.Body.Result = resultView(grades.Result)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "course-gradebook",
		Method:      http.MethodGet,
		Path:        "/v1/courses/{slug}/gradebook",
		Summary:     "Every learner's grade in a course",
		Description: "Everybody enrolled, marked or not — a gradebook that listed only the learners with " +
			"marks would hide the ones who have handed nothing in. Requires submission:grade.",
		Tags:     []string{"Grades"},
		Security: []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		Slug string `path:"slug" maxLength:"200"`
	}) (*GradebookOutput, error) {
		p, err := requirePermission(ctx, auth.PermSubmissionGrade)
		if err != nil {
			return nil, err
		}

		book, err := svc.CourseGradebook(ctx, p.TenantID, in.Slug)
		if err != nil {
			return nil, gradeError(err)
		}

		out := &GradebookOutput{CacheControl: "private, no-store"}
		out.Body.Scale = scaleView(book.Scale)
		out.Body.Items = itemViews(book.Items)
		out.Body.Learners = make([]LearnerRowView, 0, len(book.Learners))

		for _, row := range book.Learners {
			out.Body.Learners = append(out.Body.Learners, LearnerRowView{
				UserID:  row.Learner.UserID.String(),
				Name:    row.Learner.Name,
				Email:   row.Learner.Email,
				Entries: entryViews(row.Entries),
				Result:  resultView(row.Result),
			})
		}
		return out, nil
	})

	registerScales(api, svc)
}

func registerScales(api huma.API, svc *grade.Service) {
	huma.Register(api, huma.Operation{
		OperationID: "list-grading-scales",
		Method:      http.MethodGet,
		Path:        "/v1/grading-scales",
		Summary:     "The workspace's grading scales",
		Description: "The built-in default first. It is not a row and has no id.",
		Tags:        []string{"Grades"},
		Security:    []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, _ *struct{}) (*ScalesOutput, error) {
		p, err := requirePermission(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}

		scales, err := svc.Scales(ctx, p.TenantID)
		if err != nil {
			return nil, gradeError(err)
		}

		out := &ScalesOutput{CacheControl: "private, no-store"}
		out.Body.Scales = make([]ScaleView, 0, len(scales))
		for _, scale := range scales {
			out.Body.Scales = append(out.Body.Scales, scaleView(scale))
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "create-grading-scale",
		Method:        http.MethodPost,
		Path:          "/v1/grading-scales",
		Summary:       "Add a grading scale",
		DefaultStatus: http.StatusCreated,
		Description: "The bands must cover every percentage from 0 to 100 exactly once, and at least " +
			"one of them must be a pass. A scale that cannot grade every score is refused here rather " +
			"than discovered on a learner's empty gradebook.",
		Tags:     []string{"Grades"},
		Security: []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		Body struct {
			Name  string    `json:"name" minLength:"1" maxLength:"100"`
			Bands []NewBand `json:"bands" minItems:"1" maxItems:"20"`
		}
	}) (*ScaleOutput, error) {
		p, err := requirePermission(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}

		scale := grade.Scale{Name: in.Body.Name, Bands: make([]grade.Band, 0, len(in.Body.Bands))}
		for _, band := range in.Body.Bands {
			scale.Bands = append(scale.Bands, grade.Band{Label: band.Label, Min: band.Min, IsPass: band.IsPass})
		}

		created, err := svc.CreateScale(ctx, p.TenantID, scale)
		if err != nil {
			return nil, gradeError(err)
		}

		out := &ScaleOutput{CacheControl: "private, no-store"}
		out.Body.Scale = scaleView(created)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "delete-grading-scale",
		Method:        http.MethodDelete,
		Path:          "/v1/grading-scales/{id}",
		Summary:       "Remove a grading scale",
		DefaultStatus: http.StatusNoContent,
		Description:   "Courses grading by it fall back to the built-in default. None of them is deleted.",
		Tags:          []string{"Grades"},
		Security:      []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*struct{}, error) {
		p, err := requirePermission(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}
		scaleID, err := parseUUID(in.ID, "scale")
		if err != nil {
			return nil, err
		}

		if err := svc.DeleteScale(ctx, p.TenantID, scaleID); err != nil {
			return nil, gradeError(err)
		}
		return &struct{}{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "set-course-grading-scale",
		Method:      http.MethodPut,
		Path:        "/v1/courses/{slug}/grading-scale",
		Summary:     "Choose how a course grades",
		Description: "`null` puts the course back on the built-in default.",
		Tags:        []string{"Authoring"},
		Security:    []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		Slug string `path:"slug" maxLength:"200"`
		Body struct {
			// `null` is the built-in default, which is not a row and has no id.
			ScaleID Optional[string] `json:"scale_id,omitempty" doc:"The scale's id, or null for the default."`
		}
	}) (*struct{}, error) {
		p, err := requirePermission(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}

		var scaleID *uuid.UUID
		if in.Body.ScaleID.Sent && !in.Body.ScaleID.Null {
			parsed, err := parseUUID(in.Body.ScaleID.Value, "scale")
			if err != nil {
				return nil, err
			}
			scaleID = &parsed
		}

		if err := svc.SetCourseScale(ctx, p.TenantID, in.Slug, scaleID); err != nil {
			return nil, gradeError(err)
		}
		return &struct{}{}, nil
	})
}

func scaleView(s grade.Scale) ScaleView {
	view := ScaleView{Name: s.Name, Builtin: s.Builtin, Bands: make([]BandView, 0, len(s.Bands))}
	if s.ID != uuid.Nil {
		view.ID = s.ID.String()
	}
	for _, band := range s.Bands {
		view.Bands = append(view.Bands, BandView{Label: band.Label, Min: band.Min, IsPass: band.IsPass})
	}
	return view
}

func itemViews(items []grade.Item) []ItemView {
	views := make([]ItemView, 0, len(items))
	for _, item := range items {
		views = append(views, ItemView{
			ID: item.ID.String(), LessonID: item.LessonID.String(),
			Source: item.Source, Title: item.Title, MaxPoints: item.MaxPoints,
		})
	}
	return views
}

func entryViews(entries []grade.Entry) []EntryView {
	views := make([]EntryView, 0, len(entries))
	for _, entry := range entries {
		views = append(views, EntryView{
			ItemID: entry.ItemID.String(), Points: entry.Points,
			MaxPoints: entry.MaxPoints, GradedAt: entry.GradedAt,
		})
	}
	return views
}

func resultView(r grade.Result) ResultView {
	view := ResultView{
		Graded: r.Graded, ItemCount: r.ItemCount,
		Points: r.Points, MaxPoints: r.MaxPoints, Percent: r.Percent,
	}
	if r.Band.Label != "" {
		view.Band = &BandView{Label: r.Band.Label, Min: r.Band.Min, IsPass: r.Band.IsPass}
	}
	return view
}

// gradeError maps the grade package's sentinels onto status codes. The only place
// that translation happens; the domain never imports net/http.
func gradeError(err error) error {
	switch {
	case errors.Is(err, grade.ErrNotFound):
		return huma.Error404NotFound("We couldn't find that grading scale.")

	case errors.Is(err, grade.ErrScaleExists):
		return huma.Error409Conflict("A grading scale with that name already exists.")

	case errors.Is(err, grade.ErrInvalidScale):
		return huma.Error422UnprocessableEntity("Check the grading scale: each band needs a name and a mark range.")

	case errors.Is(err, grade.ErrInvalidScore):
		// A score this system built, not one a client sent. It is a bug here, and it
		// renders as one.
		return err

	default:
		return err
	}
}
