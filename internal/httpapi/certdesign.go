package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ebnsina/muallim-api/internal/auth"
	"github.com/ebnsina/muallim-api/internal/certdesign"
)

// DesignView is one saved certificate design. Layout is passed through as raw JSON
// — the designer owns its shape, and the API only carries it.
type DesignView struct {
	ID              string          `json:"id" format:"uuid"`
	Name            string          `json:"name"`
	Orientation     string          `json:"orientation" enum:"landscape,portrait"`
	Accent          string          `json:"accent,omitempty"`
	BackgroundColor string          `json:"background_color,omitempty"`
	BackgroundURL   string          `json:"background_url,omitempty"`
	Layout          json.RawMessage `json:"layout"`
	CreatedAt       string          `json:"created_at" format:"date-time"`
	UpdatedAt       string          `json:"updated_at" format:"date-time"`
}

func designView(d certdesign.Design, backgroundURL string) DesignView {
	layout := d.Layout
	if len(layout) == 0 {
		layout = json.RawMessage("[]")
	}
	return DesignView{
		ID: d.ID.String(), Name: d.Name, Orientation: d.Orientation,
		Accent: d.Accent, BackgroundColor: d.BackgroundColor, BackgroundURL: backgroundURL,
		Layout:    layout,
		CreatedAt: d.CreatedAt.Format(time.RFC3339),
		UpdatedAt: d.UpdatedAt.Format(time.RFC3339),
	}
}

func registerCertDesigns(api huma.API, svc *certdesign.Service) {
	admin := []map[string][]string{{"bearer": {}}}

	huma.Register(api, huma.Operation{
		OperationID: "list-certificate-designs",
		Method:      http.MethodGet,
		Path:        "/v1/certificate-designs",
		Summary:     "Saved certificate designs, newest first",
		Description: "Keyset-paginated. Requires course:write.",
		Tags:        []string{"Certificate Designer"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		Limit  int    `query:"limit" minimum:"1" maximum:"100" default:"50"`
		Cursor string `query:"cursor"`
	}) (*struct {
		Body struct {
			Designs    []DesignView `json:"designs"`
			NextCursor string       `json:"next_cursor,omitempty"`
			HasMore    bool         `json:"has_more"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}
		page, err := svc.List(ctx, p.TenantID, certdesign.PageParams{Limit: in.Limit, Cursor: in.Cursor})
		if err != nil {
			return nil, certDesignError(err)
		}
		out := &struct {
			Body struct {
				Designs    []DesignView `json:"designs"`
				NextCursor string       `json:"next_cursor,omitempty"`
				HasMore    bool         `json:"has_more"`
			}
		}{}
		out.Body.Designs = make([]DesignView, 0, len(page.Designs))
		for _, d := range page.Designs {
			url, err := svc.BackgroundURL(ctx, p.TenantID, d)
			if err != nil {
				return nil, certDesignError(err)
			}
			out.Body.Designs = append(out.Body.Designs, designView(d, url))
		}
		out.Body.NextCursor = page.NextCursor
		out.Body.HasMore = page.HasMore
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "create-certificate-design",
		Method:        http.MethodPost,
		Path:          "/v1/certificate-designs",
		Summary:       "Create a certificate design",
		DefaultStatus: http.StatusCreated,
		Description:   "Requires course:write.",
		Tags:          []string{"Certificate Designer"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		Body struct {
			Name            string          `json:"name,omitempty" maxLength:"200"`
			Orientation     string          `json:"orientation,omitempty" enum:"landscape,portrait"`
			Accent          string          `json:"accent,omitempty" maxLength:"32"`
			BackgroundColor string          `json:"background_color,omitempty" maxLength:"32"`
			Layout          json.RawMessage `json:"layout,omitempty"`
		}
	}) (*struct {
		Body struct {
			Design DesignView `json:"design"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}
		d, err := svc.Create(ctx, p.TenantID, certdesign.NewDesign{
			Name: in.Body.Name, Orientation: in.Body.Orientation, Accent: in.Body.Accent,
			BackgroundColor: in.Body.BackgroundColor, Layout: in.Body.Layout,
		}, certdesign.Author{UserID: p.UserID})
		if err != nil {
			return nil, certDesignError(err)
		}
		out := &struct {
			Body struct {
				Design DesignView `json:"design"`
			}
		}{}
		out.Body.Design = designView(d, "")
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-certificate-design",
		Method:      http.MethodGet,
		Path:        "/v1/certificate-designs/{id}",
		Summary:     "One certificate design",
		Description: "Requires course:write.",
		Tags:        []string{"Certificate Designer"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*struct {
		Body struct {
			Design DesignView `json:"design"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}
		id, err := parseUUID(in.ID, "design")
		if err != nil {
			return nil, err
		}
		d, err := svc.Get(ctx, p.TenantID, id)
		if err != nil {
			return nil, certDesignError(err)
		}
		url, err := svc.BackgroundURL(ctx, p.TenantID, d)
		if err != nil {
			return nil, certDesignError(err)
		}
		out := &struct {
			Body struct {
				Design DesignView `json:"design"`
			}
		}{}
		out.Body.Design = designView(d, url)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "update-certificate-design",
		Method:      http.MethodPut,
		Path:        "/v1/certificate-designs/{id}",
		Summary:     "Update a certificate design",
		Description: "Requires course:write. Every field is optional; an omitted field is left unchanged.",
		Tags:        []string{"Certificate Designer"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id" format:"uuid"`
		Body struct {
			Name            *string         `json:"name,omitempty" maxLength:"200"`
			Orientation     *string         `json:"orientation,omitempty" enum:"landscape,portrait"`
			Accent          *string         `json:"accent,omitempty" maxLength:"32"`
			BackgroundColor *string         `json:"background_color,omitempty" maxLength:"32"`
			Layout          json.RawMessage `json:"layout,omitempty"`
		}
	}) (*struct {
		Body struct {
			Design DesignView `json:"design"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}
		id, err := parseUUID(in.ID, "design")
		if err != nil {
			return nil, err
		}
		d, err := svc.Update(ctx, p.TenantID, id, certdesign.DesignPatch{
			Name: in.Body.Name, Orientation: in.Body.Orientation, Accent: in.Body.Accent,
			BackgroundColor: in.Body.BackgroundColor, Layout: in.Body.Layout,
		}, certdesign.Author{UserID: p.UserID})
		if err != nil {
			return nil, certDesignError(err)
		}
		url, err := svc.BackgroundURL(ctx, p.TenantID, d)
		if err != nil {
			return nil, certDesignError(err)
		}
		out := &struct {
			Body struct {
				Design DesignView `json:"design"`
			}
		}{}
		out.Body.Design = designView(d, url)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "delete-certificate-design",
		Method:        http.MethodDelete,
		Path:          "/v1/certificate-designs/{id}",
		Summary:       "Delete a certificate design",
		DefaultStatus: http.StatusNoContent,
		Description:   "Requires course:write.",
		Tags:          []string{"Certificate Designer"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*struct{}, error) {
		p, err := requirePermission(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}
		id, err := parseUUID(in.ID, "design")
		if err != nil {
			return nil, err
		}
		if err := svc.Delete(ctx, p.TenantID, id, certdesign.Author{UserID: p.UserID}); err != nil {
			return nil, certDesignError(err)
		}
		return &struct{}{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "presign-certificate-design-background",
		Method:        http.MethodPost,
		Path:          "/v1/certificate-designs/{id}/background/uploads",
		Summary:       "Ask for a URL to upload a background image to",
		DefaultStatus: http.StatusCreated,
		Description: "Requires course:write. Returns a URL that accepts one image of the declared size for " +
			"fifteen minutes; the bytes go straight to the object store. Confirm the upload to record it.",
		Tags:     []string{"Certificate Designer"},
		Security: admin,
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id" format:"uuid"`
		Body struct {
			ContentType string `json:"content_type" enum:"image/png,image/jpeg,image/webp"`
			Bytes       int64  `json:"bytes" minimum:"1" maximum:"8388608"`
		}
	}) (*PresignOutput, error) {
		p, err := requirePermission(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}
		id, err := parseUUID(in.ID, "design")
		if err != nil {
			return nil, err
		}
		upload, key, err := svc.PresignBackground(ctx, p.TenantID, id, in.Body.ContentType, in.Body.Bytes)
		if err != nil {
			return nil, certDesignError(err)
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
		OperationID:   "confirm-certificate-design-background",
		Method:        http.MethodPost,
		Path:          "/v1/certificate-designs/{id}/background",
		Summary:       "Record a background image you uploaded",
		DefaultStatus: http.StatusNoContent,
		Description: "Requires course:write. The object store is asked what is really at the key before it is " +
			"recorded; a key that is not this design's, or that nothing was uploaded to, is refused. The " +
			"image it replaces is deleted.",
		Tags:     []string{"Certificate Designer"},
		Security: admin,
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id" format:"uuid"`
		Body struct {
			Key string `json:"key" minLength:"1" maxLength:"1024"`
		}
	}) (*struct{}, error) {
		p, err := requirePermission(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}
		id, err := parseUUID(in.ID, "design")
		if err != nil {
			return nil, err
		}
		if err := svc.ConfirmBackground(ctx, p.TenantID, id, in.Body.Key, certdesign.Author{UserID: p.UserID}); err != nil {
			return nil, certDesignError(err)
		}
		return &struct{}{}, nil
	})
}

// certDesignError maps the certdesign package's sentinels onto status codes.
func certDesignError(err error) error {
	switch {
	case errors.Is(err, certdesign.ErrNotFound):
		return huma.Error404NotFound("Not found.")
	case errors.Is(err, certdesign.ErrInvalidPage):
		return huma.Error422UnprocessableEntity("That page cursor is not valid.")
	case errors.Is(err, certdesign.ErrInvalidLayout),
		errors.Is(err, certdesign.ErrInvalidDesign):
		return huma.Error422UnprocessableEntity(err.Error())
	default:
		return err
	}
}
