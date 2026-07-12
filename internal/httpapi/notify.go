package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/ebnsina/muallim-api/internal/notify"
)

// NotificationView is one item in a person's bell.
type NotificationView struct {
	ID        string     `json:"id" format:"uuid"`
	Kind      string     `json:"kind"`
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	Link      string     `json:"link"`
	Read      bool       `json:"read"`
	ReadAt    *time.Time `json:"read_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

// ListNotificationsOutput is a keyset page of a person's notifications. Private,
// so it is never shared by a cache.
type ListNotificationsOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Notifications []NotificationView `json:"notifications"`
		NextCursor    string             `json:"next_cursor,omitempty"`
	}
}

// UnreadCountOutput is the bell's badge number.
type UnreadCountOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Unread int `json:"unread"`
	}
}

// NotificationPreferencesOutput carries a person's notification settings.
type NotificationPreferencesOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		EmailDigest bool `json:"email_digest"`
	}
}

func notificationView(n notify.Notification) NotificationView {
	return NotificationView{
		ID:        n.ID.String(),
		Kind:      n.Kind,
		Title:     n.Title,
		Body:      n.Body,
		Link:      n.Link,
		Read:      n.Read(),
		ReadAt:    n.ReadAt,
		CreatedAt: n.CreatedAt,
	}
}

func registerNotifications(api huma.API, svc *notify.Service) {
	huma.Register(api, huma.Operation{
		OperationID: "list-notifications",
		Method:      http.MethodGet,
		Path:        "/v1/notifications",
		Summary:     "My notifications",
		Description: "A keyset page of your notifications, newest first. Pass the returned next_cursor back " +
			"as cursor for the next page.",
		Tags:     []string{"Notifications"},
		Security: []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		Cursor string `query:"cursor" maxLength:"128" doc:"Opaque cursor from a previous page"`
		Limit  int    `query:"limit" minimum:"1" maximum:"50" default:"20"`
	}) (*ListNotificationsOutput, error) {
		p, err := requirePrincipal(ctx)
		if err != nil {
			return nil, err
		}

		page, err := svc.List(ctx, p.TenantID, p.UserID, in.Cursor, in.Limit)
		if err != nil {
			return nil, notifyError(err)
		}

		out := &ListNotificationsOutput{CacheControl: "private, no-store"}
		out.Body.Notifications = make([]NotificationView, 0, len(page.Notifications))
		for _, n := range page.Notifications {
			out.Body.Notifications = append(out.Body.Notifications, notificationView(n))
		}
		out.Body.NextCursor = page.NextCursor
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "unread-notification-count",
		Method:      http.MethodGet,
		Path:        "/v1/notifications/unread-count",
		Summary:     "How many notifications I have not seen",
		Description: "The number for the header bell. Cheap: it reads a partial index of unread rows only.",
		Tags:        []string{"Notifications"},
		Security:    []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct{}) (*UnreadCountOutput, error) {
		p, err := requirePrincipal(ctx)
		if err != nil {
			return nil, err
		}

		count, err := svc.UnreadCount(ctx, p.TenantID, p.UserID)
		if err != nil {
			return nil, notifyError(err)
		}

		out := &UnreadCountOutput{CacheControl: "private, no-store"}
		out.Body.Unread = count
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "mark-notification-read",
		Method:        http.MethodPost,
		Path:          "/v1/notifications/{id}/read",
		Summary:       "Mark one notification read",
		Tags:          []string{"Notifications"},
		DefaultStatus: http.StatusNoContent,
		Security:      []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID uuid.UUID `path:"id" format:"uuid"`
	}) (*struct{}, error) {
		p, err := requirePrincipal(ctx)
		if err != nil {
			return nil, err
		}
		if err := svc.MarkRead(ctx, p.TenantID, p.UserID, in.ID); err != nil {
			return nil, notifyError(err)
		}
		return nil, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-notification-preferences",
		Method:      http.MethodGet,
		Path:        "/v1/notifications/preferences",
		Summary:     "My notification settings",
		Tags:        []string{"Notifications"},
		Security:    []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct{}) (*NotificationPreferencesOutput, error) {
		p, err := requirePrincipal(ctx)
		if err != nil {
			return nil, err
		}
		prefs, err := svc.Preferences(ctx, p.TenantID, p.UserID)
		if err != nil {
			return nil, notifyError(err)
		}
		out := &NotificationPreferencesOutput{CacheControl: "private, no-store"}
		out.Body.EmailDigest = prefs.EmailDigest
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "set-notification-preferences",
		Method:      http.MethodPut,
		Path:        "/v1/notifications/preferences",
		Summary:     "Change my notification settings",
		Description: "Turn the daily email digest of unread notifications on or off. On by default.",
		Tags:        []string{"Notifications"},
		Security:    []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		Body struct {
			EmailDigest bool `json:"email_digest"`
		}
	}) (*NotificationPreferencesOutput, error) {
		p, err := requirePrincipal(ctx)
		if err != nil {
			return nil, err
		}
		if err := svc.SetPreferences(ctx, p.TenantID, p.UserID, notify.Preferences{EmailDigest: in.Body.EmailDigest}); err != nil {
			return nil, notifyError(err)
		}
		out := &NotificationPreferencesOutput{CacheControl: "private, no-store"}
		out.Body.EmailDigest = in.Body.EmailDigest
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "mark-all-notifications-read",
		Method:        http.MethodPost,
		Path:          "/v1/notifications/read-all",
		Summary:       "Mark every notification read",
		Tags:          []string{"Notifications"},
		DefaultStatus: http.StatusNoContent,
		Security:      []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct{}) (*struct{}, error) {
		p, err := requirePrincipal(ctx)
		if err != nil {
			return nil, err
		}
		if err := svc.MarkAllRead(ctx, p.TenantID, p.UserID); err != nil {
			return nil, notifyError(err)
		}
		return nil, nil
	})
}

// notifyError maps the notify package's sentinels onto status codes.
func notifyError(err error) error {
	switch {
	case errors.Is(err, notify.ErrNotFound):
		return huma.Error404NotFound("That notification does not exist.")

	case errors.Is(err, notify.ErrInvalidPage):
		return huma.Error422UnprocessableEntity("That page cursor is not valid.")

	default:
		return err
	}
}
