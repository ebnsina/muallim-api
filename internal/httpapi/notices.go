package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ebnsina/muallim-api/internal/auth"
	"github.com/ebnsina/muallim-api/internal/notices"
)

// NoticeView is a message posted to guardians.
type NoticeView struct {
	ID             string `json:"id" format:"uuid"`
	Title          string `json:"title"`
	Body           string `json:"body"`
	Audience       string `json:"audience" enum:"all_guardians,class_guardians,section_guardians"`
	TargetID       string `json:"target_id,omitempty" format:"uuid"`
	Channel        string `json:"channel"`
	RecipientCount int    `json:"recipient_count"`
	CreatedAt      string `json:"created_at" format:"date-time"`
}

func noticeView(n notices.Notice) NoticeView {
	return NoticeView{
		ID: n.ID.String(), Title: n.Title, Body: n.Body, Audience: n.Audience,
		TargetID: uuidPtrString(n.TargetID), Channel: n.Channel,
		RecipientCount: n.RecipientCount, CreatedAt: n.CreatedAt.Format(time.RFC3339),
	}
}

func registerNotices(api huma.API, svc *notices.Service) {
	admin := []map[string][]string{{"bearer": {}}}

	huma.Register(api, huma.Operation{
		OperationID: "list-notices",
		Method:      http.MethodGet,
		Path:        "/v1/notices",
		Summary:     "The notice board, newest first",
		Description: "Keyset-paginated: pass the next_cursor from the previous page.",
		Tags:        []string{"Notices"},
		Security:    admin,
	}, func(ctx context.Context, in *struct {
		Limit  int    `query:"limit" minimum:"1" maximum:"100" default:"50"`
		Cursor string `query:"cursor"`
	}) (*struct {
		Body struct {
			Notices    []NoticeView `json:"notices"`
			NextCursor string       `json:"next_cursor,omitempty"`
			HasMore    bool         `json:"has_more"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		page, err := svc.Board(ctx, p.TenantID, notices.PageParams{Limit: in.Limit, Cursor: in.Cursor})
		if err != nil {
			return nil, noticesError(err)
		}
		out := &struct {
			Body struct {
				Notices    []NoticeView `json:"notices"`
				NextCursor string       `json:"next_cursor,omitempty"`
				HasMore    bool         `json:"has_more"`
			}
		}{}
		out.Body.Notices = make([]NoticeView, 0, len(page.Notices))
		for _, n := range page.Notices {
			out.Body.Notices = append(out.Body.Notices, noticeView(n))
		}
		out.Body.NextCursor = page.NextCursor
		out.Body.HasMore = page.HasMore
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "post-notice",
		Method:      http.MethodPost,
		Path:        "/v1/notices",
		Summary:     "Post a notice to guardians",
		Description: "Fans out to every guardian in the audience by email, in the same " +
			"transaction. An audience with nobody to reach is refused.",
		DefaultStatus: http.StatusCreated,
		Tags:          []string{"Notices"},
		Security:      admin,
	}, func(ctx context.Context, in *struct {
		Body struct {
			Title    string `json:"title" minLength:"1" maxLength:"200"`
			Body     string `json:"body" minLength:"1" maxLength:"5000"`
			Audience string `json:"audience" enum:"all_guardians,class_guardians,section_guardians"`
			TargetID string `json:"target_id,omitempty" format:"uuid"`
			Channel  string `json:"channel,omitempty" enum:"email,sms,both" default:"email"`
		}
	}) (*struct {
		Body struct {
			Notice NoticeView `json:"notice"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermAcademicsManage)
		if err != nil {
			return nil, err
		}
		target, err := optionalUUIDPtr(in.Body.TargetID, "target")
		if err != nil {
			return nil, err
		}
		notice, err := svc.Post(ctx, p.TenantID, notices.NewNotice{
			Title: in.Body.Title, Body: in.Body.Body, Audience: in.Body.Audience,
			TargetID: target, Channel: in.Body.Channel,
		}, notices.Author{UserID: p.UserID})
		if err != nil {
			return nil, noticesError(err)
		}
		out := &struct {
			Body struct {
				Notice NoticeView `json:"notice"`
			}
		}{}
		out.Body.Notice = noticeView(notice)
		return out, nil
	})
}

// noticesError maps the notices package's sentinels onto status codes.
func noticesError(err error) error {
	switch {
	case errors.Is(err, notices.ErrNotFound):
		return huma.Error404NotFound("Not found.")
	case errors.Is(err, notices.ErrNoRecipients):
		return huma.Error422UnprocessableEntity("Nobody in that audience has a contact to reach.")
	case errors.Is(err, notices.ErrTargetRequired):
		return huma.Error422UnprocessableEntity("That audience needs a class or section.")
	case errors.Is(err, notices.ErrInvalidPage):
		return huma.Error422UnprocessableEntity("That page cursor is not valid.")
	case errors.Is(err, notices.ErrInvalidNotice):
		return huma.Error422UnprocessableEntity(err.Error())
	default:
		return err
	}
}
