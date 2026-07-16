package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ebnsina/muallim-api/internal/auth"
	"github.com/ebnsina/muallim-api/internal/learnpath"
)

// LearningPathView is one ordered track of courses. CourseIDs is present on the
// detail view; the list carries metadata alone.
type LearningPathView struct {
	ID          string   `json:"id" format:"uuid"`
	Slug        string   `json:"slug"`
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	Status      string   `json:"status" enum:"draft,published"`
	CourseIDs   []string `json:"course_ids,omitempty"`
}

func learnPathView(p learnpath.Path) LearningPathView {
	v := LearningPathView{
		ID: p.ID.String(), Slug: p.Slug, Title: p.Title,
		Description: p.Description, Status: p.Status,
	}
	if len(p.Courses) > 0 {
		v.CourseIDs = make([]string, len(p.Courses))
		for i, id := range p.Courses {
			v.CourseIDs[i] = id.String()
		}
	}
	return v
}

func registerLearningPaths(api huma.API, svc *learnpath.Service) {
	admin := []map[string][]string{{"bearer": {}}}

	huma.Register(api, huma.Operation{
		OperationID: "list-learning-paths",
		Method:      http.MethodGet,
		Path:        "/v1/learning-paths",
		Summary:     "Learning paths, newest first",
		Description: "Keyset-paginated. Filter by status.",
		Tags:        []string{"Learning paths"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		Status string `query:"status" enum:"draft,published"`
		Limit  int    `query:"limit" minimum:"1" maximum:"100" default:"50"`
		Cursor string `query:"cursor"`
	}) (*struct {
		Body struct {
			Paths      []LearningPathView `json:"paths"`
			NextCursor string             `json:"next_cursor,omitempty"`
			HasMore    bool               `json:"has_more"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermCourseRead)
		if err != nil {
			return nil, err
		}
		page, err := svc.List(ctx, p.TenantID, learnpath.PathFilter{Status: in.Status},
			learnpath.PageParams{Limit: in.Limit, Cursor: in.Cursor})
		if err != nil {
			return nil, learnPathError(err)
		}
		out := &struct {
			Body struct {
				Paths      []LearningPathView `json:"paths"`
				NextCursor string             `json:"next_cursor,omitempty"`
				HasMore    bool               `json:"has_more"`
			}
		}{}
		out.Body.Paths = make([]LearningPathView, 0, len(page.Paths))
		for _, path := range page.Paths {
			out.Body.Paths = append(out.Body.Paths, learnPathView(path))
		}
		out.Body.NextCursor = page.NextCursor
		out.Body.HasMore = page.HasMore
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "create-learning-path",
		Method:        http.MethodPost,
		Path:          "/v1/learning-paths",
		Summary:       "Create a learning path",
		Description:   "Always a draft. Publishing is a later update.",
		DefaultStatus: http.StatusCreated,
		Tags:          []string{"Learning paths"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		Body struct {
			Slug        string `json:"slug" minLength:"1" maxLength:"120"`
			Title       string `json:"title" minLength:"1" maxLength:"300"`
			Description string `json:"description,omitempty" maxLength:"2000"`
		}
	}) (*struct {
		Body struct {
			Path LearningPathView `json:"path"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}
		path, err := svc.Create(ctx, p.TenantID, learnpath.NewPath{
			Slug: in.Body.Slug, Title: in.Body.Title, Description: in.Body.Description,
		}, learnpath.Author{UserID: p.UserID})
		if err != nil {
			return nil, learnPathError(err)
		}
		out := &struct {
			Body struct {
				Path LearningPathView `json:"path"`
			}
		}{}
		out.Body.Path = learnPathView(path)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-learning-path",
		Method:      http.MethodGet,
		Path:        "/v1/learning-paths/{slug}",
		Summary:     "A learning path with its ordered courses",
		Tags:        []string{"Learning paths"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		Slug string `path:"slug"`
	}) (*struct {
		Body struct {
			Path LearningPathView `json:"path"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermCourseRead)
		if err != nil {
			return nil, err
		}
		path, err := svc.Get(ctx, p.TenantID, in.Slug)
		if err != nil {
			return nil, learnPathError(err)
		}
		out := &struct {
			Body struct {
				Path LearningPathView `json:"path"`
			}
		}{}
		out.Body.Path = learnPathView(path)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "update-learning-path",
		Method:      http.MethodPut,
		Path:        "/v1/learning-paths/{slug}",
		Summary:     "Update a learning path's title, description or status",
		Tags:        []string{"Learning paths"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		Slug string `path:"slug"`
		Body struct {
			Title       *string `json:"title,omitempty" maxLength:"300"`
			Description *string `json:"description,omitempty" maxLength:"2000"`
			Status      *string `json:"status,omitempty" enum:"draft,published"`
		}
	}) (*struct {
		Body struct {
			Path LearningPathView `json:"path"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}
		path, err := svc.Update(ctx, p.TenantID, in.Slug, learnpath.PathPatch{
			Title: in.Body.Title, Description: in.Body.Description, Status: in.Body.Status,
		}, learnpath.Author{UserID: p.UserID})
		if err != nil {
			return nil, learnPathError(err)
		}
		out := &struct {
			Body struct {
				Path LearningPathView `json:"path"`
			}
		}{}
		out.Body.Path = learnPathView(path)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "delete-learning-path",
		Method:        http.MethodDelete,
		Path:          "/v1/learning-paths/{slug}",
		Summary:       "Delete a learning path",
		DefaultStatus: http.StatusNoContent,
		Tags:          []string{"Learning paths"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		Slug string `path:"slug"`
	}) (*struct{}, error) {
		p, err := requirePermission(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}
		if err := svc.Delete(ctx, p.TenantID, in.Slug, learnpath.Author{UserID: p.UserID}); err != nil {
			return nil, learnPathError(err)
		}
		return &struct{}{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "set-learning-path-courses",
		Method:      http.MethodPut,
		Path:        "/v1/learning-paths/{slug}/courses",
		Summary:     "Set a learning path's ordered course list",
		Description: "Replaces the whole membership. The list must name each course exactly once.",
		Tags:        []string{"Learning paths"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		Slug string `path:"slug"`
		Body struct {
			CourseIDs []string `json:"course_ids" format:"uuid"`
		}
	}) (*struct {
		Body struct {
			Path LearningPathView `json:"path"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}
		ids, err := parseUUIDs(in.Body.CourseIDs, "course")
		if err != nil {
			return nil, err
		}
		path, err := svc.Get(ctx, p.TenantID, in.Slug)
		if err != nil {
			return nil, learnPathError(err)
		}
		if err := svc.SetCourses(ctx, p.TenantID, path.ID, ids, learnpath.Author{UserID: p.UserID}); err != nil {
			return nil, learnPathError(err)
		}
		path, err = svc.Get(ctx, p.TenantID, in.Slug)
		if err != nil {
			return nil, learnPathError(err)
		}
		out := &struct {
			Body struct {
				Path LearningPathView `json:"path"`
			}
		}{}
		out.Body.Path = learnPathView(path)
		return out, nil
	})
}

// learnPathError maps the learnpath package's sentinels onto status codes.
func learnPathError(err error) error {
	switch {
	case errors.Is(err, learnpath.ErrNotFound):
		return huma.Error404NotFound("Not found.")
	case errors.Is(err, learnpath.ErrDuplicate):
		return huma.Error409Conflict("That slug is already taken.")
	case errors.Is(err, learnpath.ErrIncompleteOrder):
		return huma.Error409Conflict("The course order must name each course exactly once.")
	case errors.Is(err, learnpath.ErrInvalidPage):
		return huma.Error422UnprocessableEntity("That page cursor is not valid.")
	case errors.Is(err, learnpath.ErrInvalid):
		return huma.Error422UnprocessableEntity(err.Error())
	default:
		return err
	}
}
