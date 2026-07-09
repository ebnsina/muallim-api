package httpapi

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ebnsina/lms-api/internal/auth"
	"github.com/ebnsina/lms-api/internal/catalog"
	"github.com/ebnsina/lms-api/internal/tenant"
)

// ListPrerequisitesOutput names the courses a course requires.
type ListPrerequisitesOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Prerequisites []CourseSummary `json:"prerequisites"`
	}
}

func registerPrerequisites(api huma.API, svc *catalog.Service) {
	huma.Register(api, huma.Operation{
		OperationID: "list-prerequisites",
		Method:      http.MethodGet,
		Path:        "/v1/courses/{slug}/prerequisites",
		Summary:     "The courses this one requires",
		Description: "A learner must complete all of them before enrolling. Empty for most courses, " +
			"which is why it is not folded into the curriculum: that page would pay for a query it " +
			"almost never needs.",
		Tags: []string{"Catalog"},
	}, func(ctx context.Context, in *struct {
		Slug string `path:"slug" maxLength:"200"`
	}) (*ListPrerequisitesOutput, error) {
		p, isAuthor := principalFrom(ctx)
		canSeeDrafts := isAuthor && p.Can(auth.PermCourseWrite)

		course, prerequisites, err := svc.Prerequisites(ctx, tenant.ID(ctx), in.Slug, canSeeDrafts)
		if err != nil {
			return nil, catalogError(err)
		}

		// What a draft requires is a fact about a draft. Decided from the course's
		// own status, rather than from who asked, so an author fetching a published
		// course still takes the fast shared path.
		directive := catalogCacheControl
		if course.Status != catalog.StatusPublished {
			directive = draftCacheControl
		}

		out := &ListPrerequisitesOutput{CacheControl: directive}
		out.Body.Prerequisites = make([]CourseSummary, 0, len(prerequisites))
		for _, c := range prerequisites {
			out.Body.Prerequisites = append(out.Body.Prerequisites, courseSummary(c))
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "add-prerequisite",
		Method:      http.MethodPost,
		Path:        "/v1/courses/{slug}/prerequisites",
		Summary:     "Require another course before this one",
		Description: "Requires course:write. A learner must complete the required course before they may " +
			"enrol on this one. An administrator granting an enrolment overrides that. " +
			"A cycle is refused: a course nobody can start is worse than a course with no prerequisites.",
		Tags:          []string{"Authoring"},
		DefaultStatus: http.StatusNoContent,
		Security:      []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		Slug string `path:"slug" maxLength:"200"`
		Body struct {
			RequiresSlug string `json:"requires_slug" minLength:"1" maxLength:"200" doc:"Slug of the course that must be completed first"`
		}
	}) (*struct{}, error) {
		_, author, err := authorFor(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}

		if err := svc.AddPrerequisite(ctx, tenant.ID(ctx), in.Slug, in.Body.RequiresSlug, author); err != nil {
			return nil, catalogError(err)
		}
		return nil, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "remove-prerequisite",
		Method:        http.MethodDelete,
		Path:          "/v1/courses/{slug}/prerequisites/{requires_slug}",
		Summary:       "Stop requiring another course",
		Description:   "Requires course:write. Removing one that was never there is a 404.",
		Tags:          []string{"Authoring"},
		DefaultStatus: http.StatusNoContent,
		Security:      []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		Slug         string `path:"slug" maxLength:"200"`
		RequiresSlug string `path:"requires_slug" maxLength:"200"`
	}) (*struct{}, error) {
		_, author, err := authorFor(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}

		if err := svc.RemovePrerequisite(ctx, tenant.ID(ctx), in.Slug, in.RequiresSlug, author); err != nil {
			return nil, catalogError(err)
		}
		return nil, nil
	})
}
