package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ebnsina/muallim-api/internal/gamify"
)

// BadgeView is one badge in the catalogue or on a learner's shelf.
type BadgeView struct {
	Code        string     `json:"code"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Earned      bool       `json:"earned"`
	AwardedAt   *time.Time `json:"awarded_at,omitempty"`
}

// StandingOutput is a learner's own gamification summary. Private to them, so it
// is not shared by a cache.
type StandingOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Points int         `json:"points"`
		Rank   int         `json:"rank"`
		OutOf  int         `json:"out_of"`
		Badges []BadgeView `json:"badges"`
	}
}

// LeaderboardEntryView is one row of the leaderboard.
type LeaderboardEntryView struct {
	Rank   int    `json:"rank"`
	Name   string `json:"name"`
	Points int    `json:"points"`
}

// LeaderboardOutput is a workspace's top learners. Shared among members, but it
// carries their names, so it is not a public cache.
type LeaderboardOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Entries []LeaderboardEntryView `json:"entries"`
	}
}

func registerGamification(api huma.API, svc *gamify.Service) {
	huma.Register(api, huma.Operation{
		OperationID: "my-gamification",
		Method:      http.MethodGet,
		Path:        "/v1/me/gamification",
		Summary:     "My points, rank, and badges",
		Description: "Every badge in the catalogue, marked earned or not, plus your points and standing.",
		Tags:        []string{"Gamification"},
		Security:    []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct{}) (*StandingOutput, error) {
		p, err := requirePrincipal(ctx)
		if err != nil {
			return nil, err
		}

		standing, err := svc.MyStanding(ctx, p.TenantID, p.UserID)
		if err != nil {
			return nil, gamifyError(err)
		}

		// The whole catalogue, each marked earned or not, so a client can show the
		// locked ones as something to aim for.
		earned := make(map[string]time.Time, len(standing.Badges))
		for _, b := range standing.Badges {
			earned[b.Code] = b.AwardedAt
		}

		out := &StandingOutput{CacheControl: "private, no-store"}
		out.Body.Points = standing.Points
		out.Body.Rank = standing.Rank
		out.Body.OutOf = standing.OutOf
		out.Body.Badges = make([]BadgeView, 0)
		for _, b := range svc.Catalog() {
			view := BadgeView{Code: b.Code, Name: b.Name, Description: b.Description}
			if at, ok := earned[b.Code]; ok {
				view.Earned = true
				awardedAt := at
				view.AwardedAt = &awardedAt
			}
			out.Body.Badges = append(out.Body.Badges, view)
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "leaderboard",
		Method:      http.MethodGet,
		Path:        "/v1/leaderboard",
		Summary:     "The workspace leaderboard",
		Description: "The top learners by points, highest first.",
		Tags:        []string{"Gamification"},
		Security:    []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		Limit int `query:"limit" minimum:"1" maximum:"100" default:"20"`
	}) (*LeaderboardOutput, error) {
		p, err := requirePrincipal(ctx)
		if err != nil {
			return nil, err
		}

		entries, err := svc.Leaderboard(ctx, p.TenantID, in.Limit)
		if err != nil {
			return nil, gamifyError(err)
		}

		out := &LeaderboardOutput{CacheControl: "private, no-store"}
		out.Body.Entries = make([]LeaderboardEntryView, 0, len(entries))
		for _, e := range entries {
			out.Body.Entries = append(out.Body.Entries, LeaderboardEntryView{
				Rank: e.Rank, Name: e.Name, Points: e.Points,
			})
		}
		return out, nil
	})
}

// gamifyError maps the gamify package's sentinels onto status codes.
func gamifyError(err error) error {
	switch {
	case err == nil:
		return nil
	default:
		// gamify has one sentinel today (ErrInvalidPage), unused by these endpoints.
		// Anything else is unexpected and renders as a 500 with a correlation id.
		return err
	}
}
