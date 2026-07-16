package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ebnsina/muallim-api/internal/auth"
	"github.com/ebnsina/muallim-api/internal/bundle"
)

// BundleView is a course bundle. Amount is minor units (BDT poisha by default); the
// client formats it with the currency. CourseIDs is populated only when a single
// bundle is fetched — a listing omits it rather than querying per row.
type BundleView struct {
	ID          string   `json:"id" format:"uuid"`
	Slug        string   `json:"slug"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	PriceAmount int64    `json:"price_amount"`
	Currency    string   `json:"currency"`
	CourseIDs   []string `json:"course_ids,omitempty"`
	CreatedAt   string   `json:"created_at,omitempty" format:"date-time"`
}

func bundleView(b bundle.Bundle) BundleView {
	v := BundleView{
		ID: b.ID.String(), Slug: b.Slug, Name: b.Name, Description: b.Description,
		PriceAmount: b.PriceAmount, Currency: b.Currency,
	}
	if !b.CreatedAt.IsZero() {
		v.CreatedAt = b.CreatedAt.Format(time.RFC3339)
	}
	if len(b.Courses) > 0 {
		v.CourseIDs = make([]string, 0, len(b.Courses))
		for _, c := range b.Courses {
			v.CourseIDs = append(v.CourseIDs, c.CourseID.String())
		}
	}
	return v
}

func registerBundles(api huma.API, svc *bundle.Service) {
	admin := []map[string][]string{{"bearer": {}}}

	huma.Register(api, huma.Operation{
		OperationID: "list-bundles",
		Method:      http.MethodGet,
		Path:        "/v1/bundles",
		Summary:     "Course bundles, newest first",
		Description: "Keyset-paginated. A listing omits course_ids; fetch a bundle for its courses.",
		Tags:        []string{"Bundles"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		Limit  int    `query:"limit" minimum:"1" maximum:"100" default:"50"`
		Cursor string `query:"cursor"`
	}) (*struct {
		Body struct {
			Bundles    []BundleView `json:"bundles"`
			NextCursor string       `json:"next_cursor,omitempty"`
			HasMore    bool         `json:"has_more"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermCourseRead)
		if err != nil {
			return nil, err
		}
		page, err := svc.List(ctx, p.TenantID, bundle.PageParams{Limit: in.Limit, Cursor: in.Cursor})
		if err != nil {
			return nil, bundleError(err)
		}
		out := &struct {
			Body struct {
				Bundles    []BundleView `json:"bundles"`
				NextCursor string       `json:"next_cursor,omitempty"`
				HasMore    bool         `json:"has_more"`
			}
		}{}
		out.Body.Bundles = make([]BundleView, 0, len(page.Bundles))
		for _, b := range page.Bundles {
			out.Body.Bundles = append(out.Body.Bundles, bundleView(b))
		}
		out.Body.NextCursor = page.NextCursor
		out.Body.HasMore = page.HasMore
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "create-bundle",
		Method:        http.MethodPost,
		Path:          "/v1/bundles",
		Summary:       "Create a course bundle",
		DefaultStatus: http.StatusCreated,
		Tags:          []string{"Bundles"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		Body struct {
			Slug        string `json:"slug" minLength:"1" maxLength:"100"`
			Name        string `json:"name" minLength:"1" maxLength:"200"`
			Description string `json:"description,omitempty" maxLength:"2000"`
			PriceAmount int64  `json:"price_amount,omitempty" minimum:"0"`
			Currency    string `json:"currency,omitempty" minLength:"3" maxLength:"3"`
		}
	}) (*struct {
		Body struct {
			Bundle BundleView `json:"bundle"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}
		b, err := svc.Create(ctx, p.TenantID, bundle.NewBundle{
			Slug: in.Body.Slug, Name: in.Body.Name, Description: in.Body.Description,
			PriceAmount: in.Body.PriceAmount, Currency: in.Body.Currency,
		}, bundle.Author{UserID: p.UserID})
		if err != nil {
			return nil, bundleError(err)
		}
		out := &struct {
			Body struct {
				Bundle BundleView `json:"bundle"`
			}
		}{}
		out.Body.Bundle = bundleView(b)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-bundle",
		Method:      http.MethodGet,
		Path:        "/v1/bundles/{slug}",
		Summary:     "A bundle and its ordered courses",
		Tags:        []string{"Bundles"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		Slug string `path:"slug" maxLength:"100"`
	}) (*struct {
		Body struct {
			Bundle BundleView `json:"bundle"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermCourseRead)
		if err != nil {
			return nil, err
		}
		b, err := svc.Get(ctx, p.TenantID, in.Slug)
		if err != nil {
			return nil, bundleError(err)
		}
		out := &struct {
			Body struct {
				Bundle BundleView `json:"bundle"`
			}
		}{}
		out.Body.Bundle = bundleView(b)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "update-bundle",
		Method:      http.MethodPut,
		Path:        "/v1/bundles/{slug}",
		Summary:     "Update a bundle's name, description, or price",
		Tags:        []string{"Bundles"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		Slug string `path:"slug" maxLength:"100"`
		Body struct {
			Name        *string `json:"name,omitempty" maxLength:"200"`
			Description *string `json:"description,omitempty" maxLength:"2000"`
			PriceAmount *int64  `json:"price_amount,omitempty" minimum:"0"`
		}
	}) (*struct {
		Body struct {
			Bundle BundleView `json:"bundle"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}
		b, err := svc.Update(ctx, p.TenantID, in.Slug, bundle.BundlePatch{
			Name: in.Body.Name, Description: in.Body.Description, PriceAmount: in.Body.PriceAmount,
		}, bundle.Author{UserID: p.UserID})
		if err != nil {
			return nil, bundleError(err)
		}
		out := &struct {
			Body struct {
				Bundle BundleView `json:"bundle"`
			}
		}{}
		out.Body.Bundle = bundleView(b)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "delete-bundle",
		Method:        http.MethodDelete,
		Path:          "/v1/bundles/{slug}",
		Summary:       "Delete a bundle",
		DefaultStatus: http.StatusNoContent,
		Tags:          []string{"Bundles"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		Slug string `path:"slug" maxLength:"100"`
	}) (*struct{}, error) {
		p, err := requirePermission(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}
		if err := svc.Delete(ctx, p.TenantID, in.Slug, bundle.Author{UserID: p.UserID}); err != nil {
			return nil, bundleError(err)
		}
		return &struct{}{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "set-bundle-courses",
		Method:      http.MethodPut,
		Path:        "/v1/bundles/{slug}/courses",
		Summary:     "Set a bundle's ordered course list",
		Description: "Replaces the list. Positions follow the order submitted; a course named twice is refused.",
		Tags:        []string{"Bundles"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		Slug string `path:"slug" maxLength:"100"`
		Body struct {
			CourseIDs []string `json:"course_ids" maxItems:"500"`
		}
	}) (*struct {
		Body struct {
			Bundle BundleView `json:"bundle"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}
		b, err := svc.Get(ctx, p.TenantID, in.Slug)
		if err != nil {
			return nil, bundleError(err)
		}
		courseIDs, err := parseUUIDs(in.Body.CourseIDs, "course")
		if err != nil {
			return nil, err
		}
		updated, err := svc.SetCourses(ctx, p.TenantID, b.ID, courseIDs, bundle.Author{UserID: p.UserID})
		if err != nil {
			return nil, bundleError(err)
		}
		out := &struct {
			Body struct {
				Bundle BundleView `json:"bundle"`
			}
		}{}
		out.Body.Bundle = bundleView(updated)
		return out, nil
	})
}

// bundleError maps the bundle package's sentinels onto status codes.
func bundleError(err error) error {
	switch {
	case errors.Is(err, bundle.ErrNotFound):
		return huma.Error404NotFound("We couldn't find that bundle.")
	case errors.Is(err, bundle.ErrDuplicate):
		return huma.Error409Conflict("That slug is already used in this workspace.")
	case errors.Is(err, bundle.ErrInvalidPage):
		return huma.Error422UnprocessableEntity("That page link is no longer valid. Start from the first page.")
	case errors.Is(err, bundle.ErrInvalid):
		return huma.Error422UnprocessableEntity("Check the bundle details and try again.")
	default:
		return err
	}
}
