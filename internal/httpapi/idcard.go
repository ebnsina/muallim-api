package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ebnsina/muallim-api/internal/auth"
	"github.com/ebnsina/muallim-api/internal/idcard"
)

// IDCardTemplateView is one saved ID-card template. Layout is passed through as
// raw JSON — the designer owns its shape, and the API only carries it.
type IDCardTemplateView struct {
	ID              string          `json:"id" format:"uuid"`
	Name            string          `json:"name"`
	Subject         string          `json:"subject" enum:"student,staff"`
	Orientation     string          `json:"orientation" enum:"portrait,landscape"`
	Accent          string          `json:"accent,omitempty"`
	BackgroundColor string          `json:"background_color,omitempty"`
	BackgroundURL   string          `json:"background_url,omitempty"`
	Layout          json.RawMessage `json:"layout"`
	CreatedAt       string          `json:"created_at" format:"date-time"`
	UpdatedAt       string          `json:"updated_at" format:"date-time"`
}

func idCardTemplateView(t idcard.Template, backgroundURL string) IDCardTemplateView {
	layout := t.Layout
	if len(layout) == 0 {
		layout = json.RawMessage("[]")
	}
	return IDCardTemplateView{
		ID: t.ID.String(), Name: t.Name, Subject: t.Subject, Orientation: t.Orientation,
		Accent: t.Accent, BackgroundColor: t.BackgroundColor, BackgroundURL: backgroundURL,
		Layout:    layout,
		CreatedAt: t.CreatedAt.Format(time.RFC3339),
		UpdatedAt: t.UpdatedAt.Format(time.RFC3339),
	}
}

func registerIDCards(api huma.API, svc *idcard.Service) {
	admin := []map[string][]string{{"bearer": {}}}

	huma.Register(api, huma.Operation{
		OperationID: "list-id-card-templates",
		Method:      http.MethodGet,
		Path:        "/v1/id-card-templates",
		Summary:     "Saved ID-card templates, newest first",
		Description: "Keyset-paginated. Requires academics:manage.",
		Tags:        []string{"ID Card Designer"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		Limit  int    `query:"limit" minimum:"1" maximum:"100" default:"50"`
		Cursor string `query:"cursor"`
	}) (*struct {
		Body struct {
			Templates  []IDCardTemplateView `json:"templates"`
			NextCursor string               `json:"next_cursor,omitempty"`
			HasMore    bool                 `json:"has_more"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		page, err := svc.List(ctx, p.TenantID, idcard.PageParams{Limit: in.Limit, Cursor: in.Cursor})
		if err != nil {
			return nil, idCardError(err)
		}
		out := &struct {
			Body struct {
				Templates  []IDCardTemplateView `json:"templates"`
				NextCursor string               `json:"next_cursor,omitempty"`
				HasMore    bool                 `json:"has_more"`
			}
		}{}
		out.Body.Templates = make([]IDCardTemplateView, 0, len(page.Templates))
		for _, t := range page.Templates {
			url, err := svc.BackgroundURL(ctx, p.TenantID, t)
			if err != nil {
				return nil, idCardError(err)
			}
			out.Body.Templates = append(out.Body.Templates, idCardTemplateView(t, url))
		}
		out.Body.NextCursor = page.NextCursor
		out.Body.HasMore = page.HasMore
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "create-id-card-template",
		Method:        http.MethodPost,
		Path:          "/v1/id-card-templates",
		Summary:       "Create an ID-card template",
		DefaultStatus: http.StatusCreated,
		Description:   "Requires academics:manage.",
		Tags:          []string{"ID Card Designer"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		Body struct {
			Name            string          `json:"name,omitempty" maxLength:"200"`
			Subject         string          `json:"subject,omitempty" enum:"student,staff"`
			Orientation     string          `json:"orientation,omitempty" enum:"portrait,landscape"`
			Accent          string          `json:"accent,omitempty" maxLength:"32"`
			BackgroundColor string          `json:"background_color,omitempty" maxLength:"32"`
			Layout          json.RawMessage `json:"layout,omitempty"`
		}
	}) (*struct {
		Body struct {
			Template IDCardTemplateView `json:"template"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		t, err := svc.Create(ctx, p.TenantID, idcard.NewTemplate{
			Name: in.Body.Name, Subject: in.Body.Subject, Orientation: in.Body.Orientation,
			Accent: in.Body.Accent, BackgroundColor: in.Body.BackgroundColor, Layout: in.Body.Layout,
		}, idcard.Author{UserID: p.UserID})
		if err != nil {
			return nil, idCardError(err)
		}
		out := &struct {
			Body struct {
				Template IDCardTemplateView `json:"template"`
			}
		}{}
		out.Body.Template = idCardTemplateView(t, "")
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-id-card-template",
		Method:      http.MethodGet,
		Path:        "/v1/id-card-templates/{id}",
		Summary:     "One ID-card template",
		Description: "Requires academics:manage.",
		Tags:        []string{"ID Card Designer"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*struct {
		Body struct {
			Template IDCardTemplateView `json:"template"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		id, err := parseUUID(in.ID, "template")
		if err != nil {
			return nil, err
		}
		t, err := svc.Get(ctx, p.TenantID, id)
		if err != nil {
			return nil, idCardError(err)
		}
		url, err := svc.BackgroundURL(ctx, p.TenantID, t)
		if err != nil {
			return nil, idCardError(err)
		}
		out := &struct {
			Body struct {
				Template IDCardTemplateView `json:"template"`
			}
		}{}
		out.Body.Template = idCardTemplateView(t, url)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "update-id-card-template",
		Method:      http.MethodPut,
		Path:        "/v1/id-card-templates/{id}",
		Summary:     "Update an ID-card template",
		Description: "Requires academics:manage. Every field is optional; an omitted field is left unchanged.",
		Tags:        []string{"ID Card Designer"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id" format:"uuid"`
		Body struct {
			Name            *string         `json:"name,omitempty" maxLength:"200"`
			Subject         *string         `json:"subject,omitempty" enum:"student,staff"`
			Orientation     *string         `json:"orientation,omitempty" enum:"portrait,landscape"`
			Accent          *string         `json:"accent,omitempty" maxLength:"32"`
			BackgroundColor *string         `json:"background_color,omitempty" maxLength:"32"`
			Layout          json.RawMessage `json:"layout,omitempty"`
		}
	}) (*struct {
		Body struct {
			Template IDCardTemplateView `json:"template"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		id, err := parseUUID(in.ID, "template")
		if err != nil {
			return nil, err
		}
		t, err := svc.Update(ctx, p.TenantID, id, idcard.TemplatePatch{
			Name: in.Body.Name, Subject: in.Body.Subject, Orientation: in.Body.Orientation,
			Accent: in.Body.Accent, BackgroundColor: in.Body.BackgroundColor, Layout: in.Body.Layout,
		}, idcard.Author{UserID: p.UserID})
		if err != nil {
			return nil, idCardError(err)
		}
		url, err := svc.BackgroundURL(ctx, p.TenantID, t)
		if err != nil {
			return nil, idCardError(err)
		}
		out := &struct {
			Body struct {
				Template IDCardTemplateView `json:"template"`
			}
		}{}
		out.Body.Template = idCardTemplateView(t, url)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "delete-id-card-template",
		Method:        http.MethodDelete,
		Path:          "/v1/id-card-templates/{id}",
		Summary:       "Delete an ID-card template",
		DefaultStatus: http.StatusNoContent,
		Description:   "Requires academics:manage.",
		Tags:          []string{"ID Card Designer"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*struct{}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		id, err := parseUUID(in.ID, "template")
		if err != nil {
			return nil, err
		}
		if err := svc.Delete(ctx, p.TenantID, id, idcard.Author{UserID: p.UserID}); err != nil {
			return nil, idCardError(err)
		}
		return &struct{}{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "presign-id-card-template-background",
		Method:        http.MethodPost,
		Path:          "/v1/id-card-templates/{id}/background/uploads",
		Summary:       "Ask for a URL to upload a background image to",
		DefaultStatus: http.StatusCreated,
		Description: "Requires academics:manage. Returns a URL that accepts one image of the declared size for " +
			"fifteen minutes; the bytes go straight to the object store. Confirm the upload to record it.",
		Tags:     []string{"ID Card Designer"},
		Security: admin,
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id" format:"uuid"`
		Body struct {
			ContentType string `json:"content_type" enum:"image/png,image/jpeg,image/webp"`
			Bytes       int64  `json:"bytes" minimum:"1" maximum:"8388608"`
		}
	}) (*PresignOutput, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		id, err := parseUUID(in.ID, "template")
		if err != nil {
			return nil, err
		}
		upload, key, err := svc.PresignBackground(ctx, p.TenantID, id, in.Body.ContentType, in.Body.Bytes)
		if err != nil {
			return nil, idCardError(err)
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
		OperationID:   "confirm-id-card-template-background",
		Method:        http.MethodPost,
		Path:          "/v1/id-card-templates/{id}/background",
		Summary:       "Record a background image you uploaded",
		DefaultStatus: http.StatusNoContent,
		Description: "Requires academics:manage. The object store is asked what is really at the key before it is " +
			"recorded; a key that is not this template's, or that nothing was uploaded to, is refused. The " +
			"image it replaces is deleted.",
		Tags:     []string{"ID Card Designer"},
		Security: admin,
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id" format:"uuid"`
		Body struct {
			Key string `json:"key" minLength:"1" maxLength:"1024"`
		}
	}) (*struct{}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		id, err := parseUUID(in.ID, "template")
		if err != nil {
			return nil, err
		}
		if err := svc.ConfirmBackground(ctx, p.TenantID, id, in.Body.Key, idcard.Author{UserID: p.UserID}); err != nil {
			return nil, idCardError(err)
		}
		return &struct{}{}, nil
	})
}

// idCardError maps the idcard package's sentinels onto status codes.
func idCardError(err error) error {
	switch {
	case errors.Is(err, idcard.ErrNotFound):
		return huma.Error404NotFound("We couldn't find that ID card design.")
	case errors.Is(err, idcard.ErrInvalidPage):
		return huma.Error422UnprocessableEntity("That page link is no longer valid. Start from the first page.")
	case errors.Is(err, idcard.ErrInvalidLayout),
		errors.Is(err, idcard.ErrInvalidTemplate):
		return huma.Error422UnprocessableEntity("Check the ID card design and try again.")
	default:
		return err
	}
}
