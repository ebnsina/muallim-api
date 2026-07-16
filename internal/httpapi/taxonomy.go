package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ebnsina/muallim-api/internal/auth"
	"github.com/ebnsina/muallim-api/internal/taxonomy"
)

// CourseCategoryView is one section of the course catalogue.
type CourseCategoryView struct {
	ID   string `json:"id" format:"uuid"`
	Name string `json:"name"`
	Slug string `json:"slug"`
}

// TagView is one cross-cutting label on the course catalogue.
type TagView struct {
	ID   string `json:"id" format:"uuid"`
	Name string `json:"name"`
	Slug string `json:"slug"`
}

func courseCategoryView(c taxonomy.Category) CourseCategoryView {
	return CourseCategoryView{ID: c.ID.String(), Name: c.Name, Slug: c.Slug}
}

func tagView(t taxonomy.Tag) TagView {
	return TagView{ID: t.ID.String(), Name: t.Name, Slug: t.Slug}
}

func registerTaxonomy(api huma.API, svc *taxonomy.Service) {
	admin := []map[string][]string{{"bearer": {}}}

	huma.Register(api, huma.Operation{
		OperationID: "list-course-categories",
		Method:      http.MethodGet,
		Path:        "/v1/course-categories",
		Summary:     "The course catalogue's categories, by name",
		Description: "Keyset-paginated: pass the next_cursor from the previous page.",
		Tags:        []string{"Taxonomy"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		Limit  int    `query:"limit" minimum:"1" maximum:"200" default:"50"`
		Cursor string `query:"cursor"`
	}) (*struct {
		Body struct {
			Categories []CourseCategoryView `json:"categories"`
			NextCursor string               `json:"next_cursor,omitempty"`
			HasMore    bool                 `json:"has_more"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermCourseRead)
		if err != nil {
			return nil, err
		}
		page, err := svc.ListCategories(ctx, p.TenantID, taxonomy.PageParams{Limit: in.Limit, Cursor: in.Cursor})
		if err != nil {
			return nil, taxonomyError(err)
		}
		out := &struct {
			Body struct {
				Categories []CourseCategoryView `json:"categories"`
				NextCursor string               `json:"next_cursor,omitempty"`
				HasMore    bool                 `json:"has_more"`
			}
		}{}
		out.Body.Categories = make([]CourseCategoryView, 0, len(page.Categories))
		for _, c := range page.Categories {
			out.Body.Categories = append(out.Body.Categories, courseCategoryView(c))
		}
		out.Body.NextCursor = page.NextCursor
		out.Body.HasMore = page.HasMore
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "create-course-category",
		Method:        http.MethodPost,
		Path:          "/v1/course-categories",
		Summary:       "Add a category to the catalogue",
		Description:   "The slug is derived from the name when omitted. A slug clash is a 409.",
		DefaultStatus: http.StatusCreated,
		Tags:          []string{"Taxonomy"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		Body struct {
			Name string `json:"name" minLength:"1" maxLength:"120"`
			Slug string `json:"slug,omitempty" maxLength:"140"`
		}
	}) (*struct {
		Body struct {
			Category CourseCategoryView `json:"category"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}
		c, err := svc.CreateCategory(ctx, p.TenantID,
			taxonomy.NewTerm{Name: in.Body.Name, Slug: in.Body.Slug}, taxonomy.Author{UserID: p.UserID})
		if err != nil {
			return nil, taxonomyError(err)
		}
		out := &struct {
			Body struct {
				Category CourseCategoryView `json:"category"`
			}
		}{}
		out.Body.Category = courseCategoryView(c)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "delete-course-category",
		Method:      http.MethodDelete,
		Path:        "/v1/course-categories/{id}",
		Summary:     "Remove a category",
		Description: "Its course links are removed with it.",
		Tags:        []string{"Taxonomy"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*struct{}, error) {
		p, err := requirePermission(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}
		id, err := parseUUID(in.ID, "category")
		if err != nil {
			return nil, err
		}
		if err := svc.DeleteCategory(ctx, p.TenantID, id, taxonomy.Author{UserID: p.UserID}); err != nil {
			return nil, taxonomyError(err)
		}
		return &struct{}{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-course-category-courses",
		Method:      http.MethodGet,
		Path:        "/v1/course-categories/{id}/courses",
		Summary:     "The ids of the courses filed under a category",
		Description: "For the catalogue to filter a course listing by category.",
		Tags:        []string{"Taxonomy"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*struct {
		Body struct {
			CourseIDs []string `json:"course_ids"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermCourseRead)
		if err != nil {
			return nil, err
		}
		id, err := parseUUID(in.ID, "category")
		if err != nil {
			return nil, err
		}
		ids, err := svc.CourseIDsInCategory(ctx, p.TenantID, id)
		if err != nil {
			return nil, taxonomyError(err)
		}
		out := &struct {
			Body struct {
				CourseIDs []string `json:"course_ids"`
			}
		}{}
		out.Body.CourseIDs = make([]string, 0, len(ids))
		for _, id := range ids {
			out.Body.CourseIDs = append(out.Body.CourseIDs, id.String())
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-course-tags",
		Method:      http.MethodGet,
		Path:        "/v1/course-tags",
		Summary:     "The course catalogue's tags, by name",
		Description: "Keyset-paginated: pass the next_cursor from the previous page.",
		Tags:        []string{"Taxonomy"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		Limit  int    `query:"limit" minimum:"1" maximum:"200" default:"50"`
		Cursor string `query:"cursor"`
	}) (*struct {
		Body struct {
			Tags       []TagView `json:"tags"`
			NextCursor string    `json:"next_cursor,omitempty"`
			HasMore    bool      `json:"has_more"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermCourseRead)
		if err != nil {
			return nil, err
		}
		page, err := svc.ListTags(ctx, p.TenantID, taxonomy.PageParams{Limit: in.Limit, Cursor: in.Cursor})
		if err != nil {
			return nil, taxonomyError(err)
		}
		out := &struct {
			Body struct {
				Tags       []TagView `json:"tags"`
				NextCursor string    `json:"next_cursor,omitempty"`
				HasMore    bool      `json:"has_more"`
			}
		}{}
		out.Body.Tags = make([]TagView, 0, len(page.Tags))
		for _, t := range page.Tags {
			out.Body.Tags = append(out.Body.Tags, tagView(t))
		}
		out.Body.NextCursor = page.NextCursor
		out.Body.HasMore = page.HasMore
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "create-course-tag",
		Method:        http.MethodPost,
		Path:          "/v1/course-tags",
		Summary:       "Add a tag to the catalogue",
		Description:   "The slug is derived from the name when omitted. A slug clash is a 409.",
		DefaultStatus: http.StatusCreated,
		Tags:          []string{"Taxonomy"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		Body struct {
			Name string `json:"name" minLength:"1" maxLength:"120"`
			Slug string `json:"slug,omitempty" maxLength:"140"`
		}
	}) (*struct {
		Body struct {
			Tag TagView `json:"tag"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}
		t, err := svc.CreateTag(ctx, p.TenantID,
			taxonomy.NewTerm{Name: in.Body.Name, Slug: in.Body.Slug}, taxonomy.Author{UserID: p.UserID})
		if err != nil {
			return nil, taxonomyError(err)
		}
		out := &struct {
			Body struct {
				Tag TagView `json:"tag"`
			}
		}{}
		out.Body.Tag = tagView(t)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "delete-course-tag",
		Method:      http.MethodDelete,
		Path:        "/v1/course-tags/{id}",
		Summary:     "Remove a tag",
		Description: "Its course links are removed with it.",
		Tags:        []string{"Taxonomy"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		ID string `path:"id" format:"uuid"`
	}) (*struct{}, error) {
		p, err := requirePermission(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}
		id, err := parseUUID(in.ID, "tag")
		if err != nil {
			return nil, err
		}
		if err := svc.DeleteTag(ctx, p.TenantID, id, taxonomy.Author{UserID: p.UserID}); err != nil {
			return nil, taxonomyError(err)
		}
		return &struct{}{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-course-taxonomy",
		Method:      http.MethodGet,
		Path:        "/v1/courses/{slug}/taxonomy",
		Summary:     "A course's category and tags",
		Tags:        []string{"Taxonomy"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		Slug string `path:"slug"`
	}) (*struct {
		Body struct {
			Category *CourseCategoryView `json:"category"`
			Tags     []TagView           `json:"tags"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermCourseRead)
		if err != nil {
			return nil, err
		}
		cat, tags, err := svc.CourseTaxonomyBySlug(ctx, p.TenantID, in.Slug)
		if err != nil {
			return nil, taxonomyError(err)
		}
		out := &struct {
			Body struct {
				Category *CourseCategoryView `json:"category"`
				Tags     []TagView           `json:"tags"`
			}
		}{}
		if cat != nil {
			v := courseCategoryView(*cat)
			out.Body.Category = &v
		}
		out.Body.Tags = tagViews(tags)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "set-course-taxonomy",
		Method:      http.MethodPut,
		Path:        "/v1/courses/{slug}/taxonomy",
		Summary:     "Set a course's category and tags",
		Description: "The category replaces any existing one; a null category clears it. The tags " +
			"replace the whole set. An unknown category or tag is a 404.",
		Tags:     []string{"Taxonomy"},
		Security: admin,
	}, func(ctx context.Context, in *struct {
		Slug string `path:"slug"`
		Body struct {
			CategoryID string   `json:"category_id,omitempty" format:"uuid"`
			TagIDs     []string `json:"tag_ids"`
		}
	}) (*struct {
		Body struct {
			Category *CourseCategoryView `json:"category"`
			Tags     []TagView           `json:"tags"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}
		categoryID, err := optionalUUIDPtr(in.Body.CategoryID, "category")
		if err != nil {
			return nil, err
		}
		tagIDs, err := parseUUIDs(in.Body.TagIDs, "tag")
		if err != nil {
			return nil, err
		}
		cat, tags, err := svc.SetCourseTaxonomyBySlug(ctx, p.TenantID, in.Slug, categoryID, tagIDs, taxonomy.Author{UserID: p.UserID})
		if err != nil {
			return nil, taxonomyError(err)
		}
		out := &struct {
			Body struct {
				Category *CourseCategoryView `json:"category"`
				Tags     []TagView           `json:"tags"`
			}
		}{}
		if cat != nil {
			v := courseCategoryView(*cat)
			out.Body.Category = &v
		}
		out.Body.Tags = tagViews(tags)
		return out, nil
	})
}

func tagViews(tags []taxonomy.Tag) []TagView {
	out := make([]TagView, 0, len(tags))
	for _, t := range tags {
		out = append(out, tagView(t))
	}
	return out
}

// taxonomyError maps the taxonomy package's sentinels onto status codes.
func taxonomyError(err error) error {
	switch {
	case errors.Is(err, taxonomy.ErrNotFound):
		return huma.Error404NotFound("Not found.")
	case errors.Is(err, taxonomy.ErrDuplicate):
		return huma.Error409Conflict("That slug is already taken.")
	case errors.Is(err, taxonomy.ErrInvalidPage):
		return huma.Error422UnprocessableEntity("That page cursor is not valid.")
	case errors.Is(err, taxonomy.ErrInvalid):
		return huma.Error422UnprocessableEntity(err.Error())
	default:
		return err
	}
}
