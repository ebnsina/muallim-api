package httpapi

// A learner's progress across a learning path: the path knows its ordered
// courses, enroll knows how far the learner has got in each. Combined here, since
// neither domain may import the other.

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ebnsina/muallim-api/internal/auth"
	"github.com/ebnsina/muallim-api/internal/enroll"
	"github.com/ebnsina/muallim-api/internal/learnpath"
)

// PathCourseProgressView is one course of a path and the caller's progress in it.
type PathCourseProgressView struct {
	CourseID         string `json:"course_id" format:"uuid"`
	LessonsCompleted int    `json:"lessons_completed"`
	LessonsTotal     int    `json:"lessons_total"`
	Percent          int    `json:"percent"`
}

func registerLearnPathProgress(api huma.API, paths *learnpath.Service, learning *enroll.Service) {
	huma.Register(api, huma.Operation{
		OperationID: "learning-path-progress",
		Method:      http.MethodGet,
		Path:        "/v1/learning-paths/{slug}/progress",
		Summary:     "The signed-in learner's progress through a path's courses",
		Tags:        []string{"Learning paths"},
		Security:    []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		Slug string `path:"slug"`
	}) (*struct {
		Body struct {
			Courses        []PathCourseProgressView `json:"courses"`
			OverallPercent int                      `json:"overall_percent"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermCourseRead)
		if err != nil {
			return nil, err
		}
		// A learner has no progress through a path they may not see: an unpublished
		// path is 404 here too, exactly as it is on the path itself.
		path, err := paths.Get(ctx, p.TenantID, in.Slug, p.Can(auth.PermCourseWrite))
		if err != nil {
			return nil, learnPathError(err)
		}
		courseIDs, err := paths.CourseIDs(ctx, p.TenantID, path.ID)
		if err != nil {
			return nil, learnPathError(err)
		}
		prog, err := learning.PathProgress(ctx, p.TenantID, p.UserID, courseIDs)
		if err != nil {
			return nil, enrolError(err)
		}

		out := &struct {
			Body struct {
				Courses        []PathCourseProgressView `json:"courses"`
				OverallPercent int                      `json:"overall_percent"`
			}
		}{}
		out.Body.Courses = make([]PathCourseProgressView, 0, len(courseIDs))
		sum := 0
		for _, id := range courseIDs { // ordered by the path
			pr := prog[id]
			out.Body.Courses = append(out.Body.Courses, PathCourseProgressView{
				CourseID: id.String(), LessonsCompleted: pr.LessonsCompleted,
				LessonsTotal: pr.LessonsTotal, Percent: pr.Percent,
			})
			sum += pr.Percent
		}
		if len(courseIDs) > 0 {
			out.Body.OverallPercent = sum / len(courseIDs)
		}
		return out, nil
	})
}
