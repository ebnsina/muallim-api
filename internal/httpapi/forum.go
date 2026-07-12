package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/ebnsina/muallim-api/internal/auth"
	"github.com/ebnsina/muallim-api/internal/forum"
)

// forumCacheControl keeps forum reads out of shared caches: what a caller sees
// depends on their enrolments, so a URL-keyed cache would serve one member's
// board to another.
const forumCacheControl = "private, no-store"

// ForumSpaceView is a board as a client sees it.
type ForumSpaceView struct {
	ID          string `json:"id" format:"uuid"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Workspace   bool   `json:"workspace"`
	CourseSlug  string `json:"course_slug,omitempty"`
	CourseTitle string `json:"course_title,omitempty"`
	ThreadCount int    `json:"thread_count"`
}

// ForumThreadView is a thread in a list or on its own page.
type ForumThreadView struct {
	ID             string    `json:"id" format:"uuid"`
	SpaceID        string    `json:"space_id" format:"uuid"`
	Title          string    `json:"title"`
	Body           string    `json:"body"`
	AuthorName     string    `json:"author_name"`
	Pinned         bool      `json:"pinned"`
	Locked         bool      `json:"locked"`
	ReplyCount     int       `json:"reply_count"`
	Mine           bool      `json:"mine"`
	LastActivityAt time.Time `json:"last_activity_at"`
	CreatedAt      time.Time `json:"created_at"`
}

// ForumPostView is one reply.
type ForumPostView struct {
	ID         string    `json:"id" format:"uuid"`
	Body       string    `json:"body"`
	AuthorName string    `json:"author_name"`
	Mine       bool      `json:"mine"`
	CreatedAt  time.Time `json:"created_at"`
}

func forumSpaceView(s forum.Space) ForumSpaceView {
	return ForumSpaceView{
		ID:          s.ID.String(),
		Title:       s.Title,
		Description: s.Desc,
		Workspace:   s.Workspace(),
		CourseSlug:  s.CourseSlug,
		CourseTitle: s.CourseTitle,
		ThreadCount: s.ThreadCount,
	}
}

func forumThreadView(t forum.Thread, viewer uuid.UUID) ForumThreadView {
	return ForumThreadView{
		ID:             t.ID.String(),
		SpaceID:        t.SpaceID.String(),
		Title:          t.Title,
		Body:           t.Body,
		AuthorName:     t.AuthorName,
		Pinned:         t.Pinned,
		Locked:         t.Locked,
		ReplyCount:     t.ReplyCount,
		Mine:           t.AuthorID != uuid.Nil && t.AuthorID == viewer,
		LastActivityAt: t.LastActivityAt,
		CreatedAt:      t.CreatedAt,
	}
}

func forumPostView(p forum.Post, viewer uuid.UUID) ForumPostView {
	return ForumPostView{
		ID:         p.ID.String(),
		Body:       p.Body,
		AuthorName: p.AuthorName,
		Mine:       p.AuthorID != uuid.Nil && p.AuthorID == viewer,
		CreatedAt:  p.CreatedAt,
	}
}

// forumActor builds the acting identity from the session. CanModerate maps to
// forum:moderate — an authorisation decision, never a request field.
func forumActor(p auth.Principal) forum.Actor {
	return forum.Actor{UserID: p.UserID, CanModerate: p.Can(auth.PermForumModerate)}
}

// Output envelopes.
type ListForumSpacesOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Spaces []ForumSpaceView `json:"spaces"`
	}
}

type ForumSpaceOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Space ForumSpaceView `json:"space"`
	}
}

type ListForumThreadsOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Space      ForumSpaceView    `json:"space"`
		Threads    []ForumThreadView `json:"threads"`
		NextCursor string            `json:"next_cursor,omitempty"`
	}
}

type ForumThreadOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Thread     ForumThreadView `json:"thread"`
		Posts      []ForumPostView `json:"posts"`
		NextCursor string          `json:"next_cursor,omitempty"`
	}
}

type ForumPostOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Post ForumPostView `json:"post"`
	}
}

func registerForum(api huma.API, svc *forum.Service) {
	huma.Register(api, huma.Operation{
		OperationID: "list-forum-spaces",
		Method:      http.MethodGet,
		Path:        "/v1/forum/spaces",
		Summary:     "The community boards you can see",
		Description: "Every workspace-wide space, plus a space for each course you are enrolled in. A moderator " +
			"sees them all.",
		Tags:     []string{"Forum"},
		Security: []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct{}) (*ListForumSpacesOutput, error) {
		p, err := requirePrincipal(ctx)
		if err != nil {
			return nil, err
		}
		spaces, err := svc.Spaces(ctx, p.TenantID, forumActor(p))
		if err != nil {
			return nil, forumError(err)
		}
		out := &ListForumSpacesOutput{CacheControl: forumCacheControl}
		out.Body.Spaces = make([]ForumSpaceView, 0, len(spaces))
		for _, s := range spaces {
			out.Body.Spaces = append(out.Body.Spaces, forumSpaceView(s))
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "create-forum-space",
		Method:        http.MethodPost,
		Path:          "/v1/forum/spaces",
		Summary:       "Open a community board",
		Description:   "Requires forum:moderate. A course slug binds the space to that course; omit it for a workspace-wide board.",
		Tags:          []string{"Forum"},
		DefaultStatus: http.StatusCreated,
		Security:      []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		Body struct {
			Title       string `json:"title" maxLength:"200"`
			Description string `json:"description,omitempty" maxLength:"20000"`
			CourseSlug  string `json:"course_slug,omitempty" maxLength:"200"`
		}
	}) (*ForumSpaceOutput, error) {
		p, err := requirePermission(ctx, auth.PermForumModerate)
		if err != nil {
			return nil, err
		}
		space, err := svc.CreateSpace(ctx, p.TenantID, in.Body.CourseSlug, in.Body.Title, in.Body.Description)
		if err != nil {
			return nil, forumError(err)
		}
		out := &ForumSpaceOutput{CacheControl: forumCacheControl}
		out.Body.Space = forumSpaceView(space)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-forum-threads",
		Method:      http.MethodGet,
		Path:        "/v1/forum/spaces/{id}/threads",
		Summary:     "A board's threads",
		Description: "Pinned first, then most recently active. Keyset-paginated.",
		Tags:        []string{"Forum"},
		Security:    []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID     uuid.UUID `path:"id" format:"uuid"`
		Cursor string    `query:"cursor" maxLength:"160"`
		Limit  int       `query:"limit" minimum:"1" maximum:"50" default:"20"`
	}) (*ListForumThreadsOutput, error) {
		p, err := requirePrincipal(ctx)
		if err != nil {
			return nil, err
		}
		space, page, err := svc.Threads(ctx, p.TenantID, in.ID, forumActor(p), in.Cursor, in.Limit)
		if err != nil {
			return nil, forumError(err)
		}
		out := &ListForumThreadsOutput{CacheControl: forumCacheControl}
		out.Body.Space = forumSpaceView(space)
		out.Body.Threads = make([]ForumThreadView, 0, len(page.Threads))
		for _, t := range page.Threads {
			out.Body.Threads = append(out.Body.Threads, forumThreadView(t, p.UserID))
		}
		out.Body.NextCursor = page.NextCursor
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "start-forum-thread",
		Method:        http.MethodPost,
		Path:          "/v1/forum/spaces/{id}/threads",
		Summary:       "Start a thread",
		Description:   "Anyone who may see the board may start a thread on it.",
		Tags:          []string{"Forum"},
		DefaultStatus: http.StatusCreated,
		Security:      []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID   uuid.UUID `path:"id" format:"uuid"`
		Body struct {
			Title string `json:"title" maxLength:"200"`
			Body  string `json:"body" maxLength:"20000"`
		}
	}) (*ForumThreadOutput, error) {
		p, err := requirePrincipal(ctx)
		if err != nil {
			return nil, err
		}
		thread, err := svc.StartThread(ctx, p.TenantID, in.ID, forumActor(p), in.Body.Title, in.Body.Body)
		if err != nil {
			return nil, forumError(err)
		}
		out := &ForumThreadOutput{CacheControl: forumCacheControl}
		out.Body.Thread = forumThreadView(thread, p.UserID)
		out.Body.Posts = []ForumPostView{}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-forum-thread",
		Method:      http.MethodGet,
		Path:        "/v1/forum/threads/{id}",
		Summary:     "A thread and its replies",
		Tags:        []string{"Forum"},
		Security:    []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID     uuid.UUID `path:"id" format:"uuid"`
		Cursor string    `query:"cursor" maxLength:"128"`
		Limit  int       `query:"limit" minimum:"1" maximum:"50" default:"20"`
	}) (*ForumThreadOutput, error) {
		p, err := requirePrincipal(ctx)
		if err != nil {
			return nil, err
		}
		thread, page, err := svc.Thread(ctx, p.TenantID, in.ID, forumActor(p), in.Cursor, in.Limit)
		if err != nil {
			return nil, forumError(err)
		}
		out := &ForumThreadOutput{CacheControl: forumCacheControl}
		out.Body.Thread = forumThreadView(thread, p.UserID)
		out.Body.Posts = make([]ForumPostView, 0, len(page.Posts))
		for _, post := range page.Posts {
			out.Body.Posts = append(out.Body.Posts, forumPostView(post, p.UserID))
		}
		out.Body.NextCursor = page.NextCursor
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "reply-forum-thread",
		Method:        http.MethodPost,
		Path:          "/v1/forum/threads/{id}/posts",
		Summary:       "Reply to a thread",
		Description:   "A locked thread takes no replies except a moderator's. The thread's author is notified.",
		Tags:          []string{"Forum"},
		DefaultStatus: http.StatusCreated,
		Security:      []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID   uuid.UUID `path:"id" format:"uuid"`
		Body struct {
			Body string `json:"body" maxLength:"20000"`
		}
	}) (*ForumPostOutput, error) {
		p, err := requirePrincipal(ctx)
		if err != nil {
			return nil, err
		}
		post, err := svc.Reply(ctx, p.TenantID, in.ID, forumActor(p), in.Body.Body)
		if err != nil {
			return nil, forumError(err)
		}
		out := &ForumPostOutput{CacheControl: forumCacheControl}
		out.Body.Post = forumPostView(post, p.UserID)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "moderate-forum-thread",
		Method:        http.MethodPatch,
		Path:          "/v1/forum/threads/{id}",
		Summary:       "Pin or lock a thread",
		Description:   "Requires forum:moderate. Send pinned and/or locked.",
		Tags:          []string{"Forum"},
		DefaultStatus: http.StatusNoContent,
		Security:      []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID   uuid.UUID `path:"id" format:"uuid"`
		Body struct {
			Pinned *bool `json:"pinned,omitempty"`
			Locked *bool `json:"locked,omitempty"`
		}
	}) (*struct{}, error) {
		p, err := requirePermission(ctx, auth.PermForumModerate)
		if err != nil {
			return nil, err
		}
		if in.Body.Pinned != nil {
			if err := svc.Pin(ctx, p.TenantID, in.ID, *in.Body.Pinned); err != nil {
				return nil, forumError(err)
			}
		}
		if in.Body.Locked != nil {
			if err := svc.Lock(ctx, p.TenantID, in.ID, *in.Body.Locked); err != nil {
				return nil, forumError(err)
			}
		}
		return nil, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "delete-forum-thread",
		Method:        http.MethodDelete,
		Path:          "/v1/forum/threads/{id}",
		Summary:       "Delete a thread",
		Description:   "Yours to delete, or anyone's if you can moderate. Removing a thread removes its replies.",
		Tags:          []string{"Forum"},
		DefaultStatus: http.StatusNoContent,
		Security:      []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID uuid.UUID `path:"id" format:"uuid"`
	}) (*struct{}, error) {
		p, err := requirePrincipal(ctx)
		if err != nil {
			return nil, err
		}
		if err := svc.DeleteThread(ctx, p.TenantID, in.ID, forumActor(p)); err != nil {
			return nil, forumError(err)
		}
		return nil, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "delete-forum-post",
		Method:        http.MethodDelete,
		Path:          "/v1/forum/posts/{id}",
		Summary:       "Delete a reply",
		Description:   "Yours to delete, or anyone's if you can moderate.",
		Tags:          []string{"Forum"},
		DefaultStatus: http.StatusNoContent,
		Security:      []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, in *struct {
		ID uuid.UUID `path:"id" format:"uuid"`
	}) (*struct{}, error) {
		p, err := requirePrincipal(ctx)
		if err != nil {
			return nil, err
		}
		if err := svc.DeletePost(ctx, p.TenantID, in.ID, forumActor(p)); err != nil {
			return nil, forumError(err)
		}
		return nil, nil
	})
}

// forumError maps the forum package's sentinels onto status codes.
func forumError(err error) error {
	switch {
	case errors.Is(err, forum.ErrSpaceNotFound):
		return huma.Error404NotFound("That board does not exist.")

	case errors.Is(err, forum.ErrThreadNotFound):
		return huma.Error404NotFound("That thread does not exist.")

	case errors.Is(err, forum.ErrPostNotFound):
		return huma.Error404NotFound("That reply does not exist.")

	case errors.Is(err, forum.ErrLocked):
		return huma.Error409Conflict("This thread is locked.")

	case errors.Is(err, forum.ErrEmpty):
		return huma.Error422UnprocessableEntity("A title and a body are required.")

	case errors.Is(err, forum.ErrTooLong):
		return huma.Error422UnprocessableEntity("That is too long.")

	case errors.Is(err, forum.ErrForbidden):
		return huma.Error403Forbidden("You may not do that.")

	case errors.Is(err, forum.ErrInvalidPage):
		return huma.Error422UnprocessableEntity("That page cursor is not valid.")

	default:
		return err
	}
}
