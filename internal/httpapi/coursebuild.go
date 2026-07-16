package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ebnsina/muallim-api/internal/auth"
	"github.com/ebnsina/muallim-api/internal/coursebuild"
)

// BlueprintView is one course-structure sketch. Structure is passed through as raw
// JSON so the designer's document is never reshaped by the wire layer.
type BlueprintView struct {
	ID          string          `json:"id" format:"uuid"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Structure   json.RawMessage `json:"structure"`
	CreatedAt   string          `json:"created_at" format:"date-time"`
	UpdatedAt   string          `json:"updated_at" format:"date-time"`
}

func blueprintView(b coursebuild.Blueprint) BlueprintView {
	structure := b.Structure
	if len(structure) == 0 {
		structure = json.RawMessage("[]")
	}
	return BlueprintView{
		ID:          b.ID.String(),
		Name:        b.Name,
		Description: b.Description,
		Structure:   structure,
		CreatedAt:   b.CreatedAt.Format(http.TimeFormat),
		UpdatedAt:   b.UpdatedAt.Format(http.TimeFormat),
	}
}

func registerCourseBlueprints(api huma.API, svc *coursebuild.Service) {
	admin := []map[string][]string{{"bearer": {}}}

	huma.Register(api, huma.Operation{
		OperationID: "list-course-blueprints",
		Method:      http.MethodGet,
		Path:        "/v1/course-blueprints",
		Summary:     "Course blueprints, newest first",
		Description: "Keyset-paginated list of the standalone course-structure sketches.",
		Tags:        []string{"Course Builder"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		Limit  int    `query:"limit" minimum:"1" maximum:"100" default:"50"`
		Cursor string `query:"cursor"`
	}) (*struct {
		Body struct {
			Blueprints []BlueprintView `json:"blueprints"`
			NextCursor string          `json:"next_cursor,omitempty"`
			HasMore    bool            `json:"has_more"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}
		page, err := svc.List(ctx, p.TenantID, coursebuild.PageParams{Limit: in.Limit, Cursor: in.Cursor})
		if err != nil {
			return nil, courseBuildError(err)
		}
		out := &struct {
			Body struct {
				Blueprints []BlueprintView `json:"blueprints"`
				NextCursor string          `json:"next_cursor,omitempty"`
				HasMore    bool            `json:"has_more"`
			}
		}{}
		out.Body.Blueprints = make([]BlueprintView, 0, len(page.Blueprints))
		for _, b := range page.Blueprints {
			out.Body.Blueprints = append(out.Body.Blueprints, blueprintView(b))
		}
		out.Body.NextCursor = page.NextCursor
		out.Body.HasMore = page.HasMore
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "create-course-blueprint",
		Method:        http.MethodPost,
		Path:          "/v1/course-blueprints",
		Summary:       "Create a course blueprint",
		Description:   "The structure is an array of modules, each holding lessons of kind video, text, quiz, assignment or file.",
		DefaultStatus: http.StatusCreated,
		Tags:          []string{"Course Builder"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		Body struct {
			Name        string          `json:"name" minLength:"1" maxLength:"300"`
			Description string          `json:"description,omitempty" maxLength:"5000"`
			Structure   json.RawMessage `json:"structure,omitempty"`
		}
	}) (*struct {
		Body struct {
			Blueprint BlueprintView `json:"blueprint"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}
		b, err := svc.Create(ctx, p.TenantID, coursebuild.NewBlueprint{
			Name: in.Body.Name, Description: in.Body.Description, Structure: in.Body.Structure,
		}, coursebuild.Author{UserID: p.UserID})
		if err != nil {
			return nil, courseBuildError(err)
		}
		out := &struct {
			Body struct {
				Blueprint BlueprintView `json:"blueprint"`
			}
		}{}
		out.Body.Blueprint = blueprintView(b)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-course-blueprint",
		Method:      http.MethodGet,
		Path:        "/v1/course-blueprints/{id}",
		Summary:     "One course blueprint",
		Tags:        []string{"Course Builder"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*struct {
		Body struct {
			Blueprint BlueprintView `json:"blueprint"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}
		id, err := parseUUID(in.ID, "blueprint")
		if err != nil {
			return nil, err
		}
		b, err := svc.Get(ctx, p.TenantID, id)
		if err != nil {
			return nil, courseBuildError(err)
		}
		out := &struct {
			Body struct {
				Blueprint BlueprintView `json:"blueprint"`
			}
		}{}
		out.Body.Blueprint = blueprintView(b)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "update-course-blueprint",
		Method:      http.MethodPut,
		Path:        "/v1/course-blueprints/{id}",
		Summary:     "Update a course blueprint",
		Description: "Any omitted field is left unchanged; a supplied structure replaces the whole document.",
		Tags:        []string{"Course Builder"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id" format:"uuid"`
		Body struct {
			Name        *string         `json:"name,omitempty" minLength:"1" maxLength:"300"`
			Description *string         `json:"description,omitempty" maxLength:"5000"`
			Structure   json.RawMessage `json:"structure,omitempty"`
		}
	}) (*struct {
		Body struct {
			Blueprint BlueprintView `json:"blueprint"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}
		id, err := parseUUID(in.ID, "blueprint")
		if err != nil {
			return nil, err
		}
		b, err := svc.Update(ctx, p.TenantID, id, coursebuild.BlueprintPatch{
			Name: in.Body.Name, Description: in.Body.Description, Structure: in.Body.Structure,
		}, coursebuild.Author{UserID: p.UserID})
		if err != nil {
			return nil, courseBuildError(err)
		}
		out := &struct {
			Body struct {
				Blueprint BlueprintView `json:"blueprint"`
			}
		}{}
		out.Body.Blueprint = blueprintView(b)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "delete-course-blueprint",
		Method:        http.MethodDelete,
		Path:          "/v1/course-blueprints/{id}",
		Summary:       "Delete a course blueprint",
		DefaultStatus: http.StatusNoContent,
		Tags:          []string{"Course Builder"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*struct{}, error) {
		p, err := requirePermission(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}
		id, err := parseUUID(in.ID, "blueprint")
		if err != nil {
			return nil, err
		}
		if err := svc.Delete(ctx, p.TenantID, id, coursebuild.Author{UserID: p.UserID}); err != nil {
			return nil, courseBuildError(err)
		}
		return nil, nil
	})
}

// courseBuildError maps the coursebuild package's sentinels onto status codes.
func courseBuildError(err error) error {
	switch {
	case errors.Is(err, coursebuild.ErrNotFound):
		return huma.Error404NotFound("Not found.")
	case errors.Is(err, coursebuild.ErrInvalidPage):
		return huma.Error422UnprocessableEntity("That page cursor is not valid.")
	case errors.Is(err, coursebuild.ErrInvalidBlueprint):
		return huma.Error422UnprocessableEntity(err.Error())
	default:
		return err
	}
}
