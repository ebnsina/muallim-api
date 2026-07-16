package httpapi

// Granting a bundle crosses a domain boundary: bundle knows which courses it
// holds, enroll knows how to admit a learner, and neither imports the other — so
// the orchestration lives here. A bundle grant is a grant (SourceGranted): the
// learner was given the courses, so they cannot cancel their way out and keep them.

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ebnsina/muallim-api/internal/auth"
	"github.com/ebnsina/muallim-api/internal/bundle"
	"github.com/ebnsina/muallim-api/internal/enroll"
)

func registerBundleGrant(api huma.API, bundles *bundle.Service, learning *enroll.Service) {
	huma.Register(api, huma.Operation{
		OperationID:   "grant-bundle",
		Method:        http.MethodPost,
		Path:          "/v1/bundles/{slug}/grant",
		Summary:       "Enrol a learner in every course of a bundle",
		Tags:          []string{"Bundles"},
		Security:      []map[string][]string{{"bearer": {}}},
		DefaultStatus: http.StatusNoContent,
	}, func(ctx context.Context, in *struct {
		Slug string `path:"slug"`
		Body struct {
			LearnerID string `json:"learner_id" format:"uuid"`
		}
	}) (*struct{}, error) {
		p, err := requirePermission(ctx, auth.PermCourseWrite)
		if err != nil {
			return nil, err
		}
		learnerID, err := parseUUID(in.Body.LearnerID, "learner")
		if err != nil {
			return nil, err
		}

		b, err := bundles.Get(ctx, p.TenantID, in.Slug)
		if err != nil {
			return nil, bundleError(err)
		}
		courseIDs, err := bundles.CourseIDs(ctx, p.TenantID, b.ID)
		if err != nil {
			return nil, bundleError(err)
		}
		if err := learning.GrantCourses(ctx, p.TenantID, courseIDs, learnerID, enroll.SourceGranted); err != nil {
			return nil, enrolError(err)
		}
		return nil, nil
	})
}
