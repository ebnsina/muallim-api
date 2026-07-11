package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ebnsina/lms-api/internal/enroll"
)

// ReviewView is one learner's public review of a course.
type ReviewView struct {
	Rating     int       `json:"rating" minimum:"1" maximum:"5"`
	Body       string    `json:"body"`
	AuthorName string    `json:"author_name"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// ReviewSummaryView is a course's rating at a glance.
type ReviewSummaryView struct {
	Count   int     `json:"count"`
	Average float64 `json:"average"`
}

// ListReviewsOutput is a course's review wall, its summary, and the caller's own
// review if they left one. It carries the reader's own verdict, so it is private.
type ListReviewsOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Reviews []ReviewView      `json:"reviews"`
		Summary ReviewSummaryView `json:"summary"`
		Mine    *ReviewView       `json:"mine,omitempty"`
	}
}

// CourseReviewOutput confirms a review was written.
type CourseReviewOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Review ReviewView `json:"review"`
	}
}

func reviewView(r enroll.Review) ReviewView {
	return ReviewView{
		Rating:     r.Rating,
		Body:       r.Body,
		AuthorName: r.AuthorName,
		CreatedAt:  r.CreatedAt,
		UpdatedAt:  r.UpdatedAt,
	}
}

func registerReviews(api huma.API, svc *enroll.Service) {
	huma.Register(api, huma.Operation{
		OperationID: "list-course-reviews",
		Method:      http.MethodGet,
		Path:        "/v1/courses/{slug}/reviews",
		Summary:     "A course's reviews and rating",
		Description: "The review wall, newest first, with the mean rating. If you are signed in and have " +
			"reviewed this course, your own review comes back too, so a form can prefill it.",
		Tags:     []string{"Learning"},
		Security: []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		Slug  string `path:"slug" maxLength:"200"`
		Limit int    `query:"limit" minimum:"1" maximum:"100" default:"20"`
	}) (*ListReviewsOutput, error) {
		p, err := requirePrincipal(ctx)
		if err != nil {
			return nil, err
		}

		list, summary, mine, err := svc.Reviews(ctx, p.TenantID, in.Slug, p.UserID, in.Limit)
		if err != nil {
			return nil, enrolError(err)
		}

		out := &ListReviewsOutput{CacheControl: lessonCacheControl}
		out.Body.Reviews = make([]ReviewView, 0, len(list))
		for _, r := range list {
			out.Body.Reviews = append(out.Body.Reviews, reviewView(r))
		}
		out.Body.Summary = ReviewSummaryView{Count: summary.Count, Average: summary.Average}
		if mine != nil {
			v := reviewView(*mine)
			out.Body.Mine = &v
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "review-course",
		Method:      http.MethodPut,
		Path:        "/v1/courses/{slug}/reviews",
		Summary:     "Rate and review a course",
		Description: "One review per learner: submitting again edits your first. Requires a live enrolment — " +
			"you review a course you took.",
		Tags:     []string{"Learning"},
		Security: []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		Slug string `path:"slug" maxLength:"200"`
		Body struct {
			Rating int    `json:"rating" minimum:"1" maximum:"5"`
			Body   string `json:"body,omitempty" maxLength:"4000"`
		}
	}) (*CourseReviewOutput, error) {
		p, err := requirePrincipal(ctx)
		if err != nil {
			return nil, err
		}

		review, err := svc.Review(ctx, p.TenantID, in.Slug, actorFrom(ctx, p), in.Body.Rating, in.Body.Body)
		if err != nil {
			return nil, enrolError(err)
		}

		out := &CourseReviewOutput{CacheControl: lessonCacheControl}
		out.Body.Review = reviewView(review)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "unreview-course",
		Method:        http.MethodDelete,
		Path:          "/v1/courses/{slug}/reviews",
		Summary:       "Retract my review",
		Tags:          []string{"Learning"},
		DefaultStatus: http.StatusNoContent,
		Security:      []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		Slug string `path:"slug" maxLength:"200"`
	}) (*struct{}, error) {
		p, err := requirePrincipal(ctx)
		if err != nil {
			return nil, err
		}
		if err := svc.UnReview(ctx, p.TenantID, in.Slug, actorFrom(ctx, p)); err != nil {
			return nil, enrolError(err)
		}
		return nil, nil
	})
}
