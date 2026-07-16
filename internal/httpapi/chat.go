package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/ebnsina/muallim-api/internal/auth"
	"github.com/ebnsina/muallim-api/internal/chat"
	"github.com/ebnsina/muallim-api/internal/enroll"
)

// chatCacheControl keeps chat out of shared caches: what a caller sees depends on
// which conversations they belong to, so a URL-keyed cache would leak one member's
// messages to another.
const chatCacheControl = "private, no-store"

// ChatConversationView is a conversation as a client sees it.
type ChatConversationView struct {
	ID            string           `json:"id" format:"uuid"`
	Kind          string           `json:"kind" enum:"course,direct,group"`
	Title         string           `json:"title"`
	CourseID      string           `json:"course_id,omitempty" format:"uuid"`
	CreatedAt     time.Time        `json:"created_at"`
	LastMessageAt time.Time        `json:"last_message_at"`
	Unread        int              `json:"unread"`
	LastMessage   *ChatMessageView `json:"last_message,omitempty"`
	Members       []ChatMemberView `json:"members,omitempty"`
}

// ChatMemberView is one participant.
type ChatMemberView struct {
	UserID     string     `json:"user_id" format:"uuid"`
	Name       string     `json:"name"`
	Role       string     `json:"role" enum:"member,admin"`
	LastReadAt *time.Time `json:"last_read_at,omitempty"`
}

// ChatMessageView is one message.
type ChatMessageView struct {
	ID             string    `json:"id" format:"uuid"`
	ConversationID string    `json:"conversation_id" format:"uuid"`
	SenderID       string    `json:"sender_id,omitempty" format:"uuid"`
	SenderName     string    `json:"sender_name"`
	Body           string    `json:"body"`
	Mine           bool      `json:"mine"`
	CreatedAt      time.Time `json:"created_at"`
}

func chatConversationView(c chat.Conversation, viewer uuid.UUID) ChatConversationView {
	v := ChatConversationView{
		ID:            c.ID.String(),
		Kind:          c.Kind,
		Title:         c.Title,
		CreatedAt:     c.CreatedAt,
		LastMessageAt: c.LastMessageAt,
		Unread:        c.Unread,
	}
	if c.CourseID != nil {
		v.CourseID = c.CourseID.String()
	}
	if c.LastMessage != nil {
		m := chatMessageView(*c.LastMessage, viewer)
		v.LastMessage = &m
	}
	if len(c.Members) > 0 {
		v.Members = make([]ChatMemberView, 0, len(c.Members))
		for _, m := range c.Members {
			v.Members = append(v.Members, ChatMemberView{
				UserID: m.UserID.String(), Name: m.Name, Role: m.Role, LastReadAt: m.LastReadAt,
			})
		}
	}
	return v
}

func chatMessageView(m chat.Message, viewer uuid.UUID) ChatMessageView {
	return ChatMessageView{
		ID:             m.ID.String(),
		ConversationID: m.ConversationID.String(),
		SenderID:       uuidPtrString(nonNil(m.SenderID)),
		SenderName:     m.SenderName,
		Body:           m.Body,
		Mine:           m.SenderID != uuid.Nil && m.SenderID == viewer,
		CreatedAt:      m.CreatedAt,
	}
}

// nonNil returns a pointer to id, or nil when id is the zero uuid, so a removed
// sender renders with no sender_id rather than the all-zero uuid.
func nonNil(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

// Output envelopes.
type ListChatConversationsOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Conversations []ChatConversationView `json:"conversations"`
		NextCursor    string                 `json:"next_cursor,omitempty"`
	}
}

type ChatConversationOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Conversation ChatConversationView `json:"conversation"`
	}
}

type ListChatMessagesOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Messages   []ChatMessageView `json:"messages"`
		NextCursor string            `json:"next_cursor,omitempty"`
	}
}

type ChatMessageOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Message ChatMessageView `json:"message"`
	}
}

// registerChat wires the chat REST surface. Every read and send is gated on
// membership: a non-member gets 403 on a conversation they can be told exists, and
// 404 on one they cannot. Realtime (WebSocket) is registered separately.
func registerChat(api huma.API, svc *chat.Service, learning *enroll.Service) {
	bearer := []map[string][]string{{"bearer": {}}}

	huma.Register(api, huma.Operation{
		OperationID: "list-chat-conversations",
		Method:      http.MethodGet,
		Path:        "/v1/chat/conversations",
		Summary:     "Your conversations",
		Description: "The conversations you belong to, most recently active first, each with its last message and your unread count. Keyset-paginated.",
		Tags:        []string{"Chat"},
		Security:    bearer,
	}, func(ctx context.Context, in *struct {
		Cursor string `query:"cursor" maxLength:"256"`
		Limit  int    `query:"limit" minimum:"1" maximum:"100" default:"30"`
	}) (*ListChatConversationsOutput, error) {
		p, err := requirePermission(ctx, auth.PermCourseRead)
		if err != nil {
			return nil, err
		}
		page, err := svc.ListForUser(ctx, p.TenantID, p.UserID, chat.PageParams{Limit: in.Limit, Cursor: in.Cursor})
		if err != nil {
			return nil, chatError(err)
		}
		out := &ListChatConversationsOutput{CacheControl: chatCacheControl}
		out.Body.Conversations = make([]ChatConversationView, 0, len(page.Conversations))
		for _, c := range page.Conversations {
			out.Body.Conversations = append(out.Body.Conversations, chatConversationView(c, p.UserID))
		}
		out.Body.NextCursor = page.NextCursor
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "create-chat-conversation",
		Method:        http.MethodPost,
		Path:          "/v1/chat/conversations",
		Summary:       "Open a direct message or a group",
		Description:   "kind 'direct' with user_id opens (or reuses) the 1:1 with that member; kind 'group' with a title and member_ids opens a group you own.",
		Tags:          []string{"Chat"},
		DefaultStatus: http.StatusCreated,
		Security:      bearer,
	}, func(ctx context.Context, in *struct {
		Body struct {
			Kind      string   `json:"kind" enum:"direct,group"`
			UserID    string   `json:"user_id,omitempty" format:"uuid"`
			Title     string   `json:"title,omitempty" maxLength:"200"`
			MemberIDs []string `json:"member_ids,omitempty" maxItems:"500"`
		}
	}) (*ChatConversationOutput, error) {
		p, err := requirePermission(ctx, auth.PermCourseRead)
		if err != nil {
			return nil, err
		}
		var conv chat.Conversation
		switch in.Body.Kind {
		case chat.KindDirect:
			other, err := parseUUID(in.Body.UserID, "user")
			if err != nil {
				return nil, err
			}
			conv, err = svc.EnsureDirect(ctx, p.TenantID, p.UserID, other)
			if err != nil {
				return nil, chatError(err)
			}
		case chat.KindGroup:
			members, err := parseUUIDs(in.Body.MemberIDs, "member")
			if err != nil {
				return nil, err
			}
			conv, err = svc.CreateGroup(ctx, p.TenantID, in.Body.Title, members, p.UserID)
			if err != nil {
				return nil, chatError(err)
			}
		default:
			return nil, huma.Error422UnprocessableEntity("kind must be 'direct' or 'group'.")
		}
		full, err := svc.Conversation(ctx, p.TenantID, conv.ID)
		if err != nil {
			return nil, chatError(err)
		}
		out := &ChatConversationOutput{CacheControl: chatCacheControl}
		out.Body.Conversation = chatConversationView(full, p.UserID)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-chat-conversation",
		Method:      http.MethodGet,
		Path:        "/v1/chat/conversations/{id}",
		Summary:     "A conversation and its members",
		Tags:        []string{"Chat"},
		Security:    bearer,
	}, func(ctx context.Context, in *struct {
		ID uuid.UUID `path:"id" format:"uuid"`
	}) (*ChatConversationOutput, error) {
		p, err := requirePermission(ctx, auth.PermCourseRead)
		if err != nil {
			return nil, err
		}
		if err := requireChatMember(ctx, svc, p, in.ID); err != nil {
			return nil, err
		}
		conv, err := svc.Conversation(ctx, p.TenantID, in.ID)
		if err != nil {
			return nil, chatError(err)
		}
		out := &ChatConversationOutput{CacheControl: chatCacheControl}
		out.Body.Conversation = chatConversationView(conv, p.UserID)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-chat-messages",
		Method:      http.MethodGet,
		Path:        "/v1/chat/conversations/{id}/messages",
		Summary:     "A conversation's messages",
		Description: "Newest first, keyset-paginated. Members only.",
		Tags:        []string{"Chat"},
		Security:    bearer,
	}, func(ctx context.Context, in *struct {
		ID     uuid.UUID `path:"id" format:"uuid"`
		Cursor string    `query:"cursor" maxLength:"256"`
		Limit  int       `query:"limit" minimum:"1" maximum:"100" default:"30"`
	}) (*ListChatMessagesOutput, error) {
		p, err := requirePermission(ctx, auth.PermCourseRead)
		if err != nil {
			return nil, err
		}
		page, err := svc.Messages(ctx, p.TenantID, in.ID, p.UserID, chat.PageParams{Limit: in.Limit, Cursor: in.Cursor})
		if err != nil {
			return nil, chatError(err)
		}
		out := &ListChatMessagesOutput{CacheControl: chatCacheControl}
		out.Body.Messages = make([]ChatMessageView, 0, len(page.Messages))
		for _, m := range page.Messages {
			out.Body.Messages = append(out.Body.Messages, chatMessageView(m, p.UserID))
		}
		out.Body.NextCursor = page.NextCursor
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "send-chat-message",
		Method:        http.MethodPost,
		Path:          "/v1/chat/conversations/{id}/messages",
		Summary:       "Send a message",
		Description:   "Members only.",
		Tags:          []string{"Chat"},
		DefaultStatus: http.StatusCreated,
		Security:      bearer,
	}, func(ctx context.Context, in *struct {
		ID   uuid.UUID `path:"id" format:"uuid"`
		Body struct {
			Body string `json:"body" minLength:"1" maxLength:"20000"`
		}
	}) (*ChatMessageOutput, error) {
		p, err := requirePermission(ctx, auth.PermCourseRead)
		if err != nil {
			return nil, err
		}
		msg, err := svc.Send(ctx, p.TenantID, in.ID, p.UserID, in.Body.Body)
		if err != nil {
			return nil, chatError(err)
		}
		out := &ChatMessageOutput{CacheControl: chatCacheControl}
		out.Body.Message = chatMessageView(msg, p.UserID)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "mark-chat-read",
		Method:        http.MethodPost,
		Path:          "/v1/chat/conversations/{id}/read",
		Summary:       "Mark a conversation read up to now",
		Tags:          []string{"Chat"},
		DefaultStatus: http.StatusNoContent,
		Security:      bearer,
	}, func(ctx context.Context, in *struct {
		ID uuid.UUID `path:"id" format:"uuid"`
	}) (*struct{}, error) {
		p, err := requirePermission(ctx, auth.PermCourseRead)
		if err != nil {
			return nil, err
		}
		if err := svc.MarkRead(ctx, p.TenantID, in.ID, p.UserID); err != nil {
			return nil, chatError(err)
		}
		return nil, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "add-chat-member",
		Method:        http.MethodPost,
		Path:          "/v1/chat/conversations/{id}/members",
		Summary:       "Add someone to a group",
		Description:   "Group admins only.",
		Tags:          []string{"Chat"},
		DefaultStatus: http.StatusNoContent,
		Security:      bearer,
	}, func(ctx context.Context, in *struct {
		ID   uuid.UUID `path:"id" format:"uuid"`
		Body struct {
			UserID string `json:"user_id" format:"uuid"`
		}
	}) (*struct{}, error) {
		p, err := requirePermission(ctx, auth.PermCourseRead)
		if err != nil {
			return nil, err
		}
		if err := requireChatAdmin(ctx, svc, p, in.ID); err != nil {
			return nil, err
		}
		userID, err := parseUUID(in.Body.UserID, "user")
		if err != nil {
			return nil, err
		}
		if err := svc.AddMember(ctx, p.TenantID, in.ID, userID, chat.Author{UserID: p.UserID}); err != nil {
			return nil, chatError(err)
		}
		return nil, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "remove-chat-member",
		Method:        http.MethodDelete,
		Path:          "/v1/chat/conversations/{id}/members/{user_id}",
		Summary:       "Remove someone from a group",
		Description:   "Group admins only.",
		Tags:          []string{"Chat"},
		DefaultStatus: http.StatusNoContent,
		Security:      bearer,
	}, func(ctx context.Context, in *struct {
		ID     uuid.UUID `path:"id" format:"uuid"`
		UserID uuid.UUID `path:"user_id" format:"uuid"`
	}) (*struct{}, error) {
		p, err := requirePermission(ctx, auth.PermCourseRead)
		if err != nil {
			return nil, err
		}
		if err := requireChatAdmin(ctx, svc, p, in.ID); err != nil {
			return nil, err
		}
		if err := svc.RemoveMember(ctx, p.TenantID, in.ID, in.UserID, chat.Author{UserID: p.UserID}); err != nil {
			return nil, chatError(err)
		}
		return nil, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "join-course-chat-channel",
		Method:      http.MethodPost,
		Path:        "/v1/courses/{slug}/chat/channel",
		Summary:     "Join (or open) a course's chat channel",
		Description: "Get-or-create the course's single channel and join it. An enrolled learner, or anyone who may edit the course, may do this.",
		Tags:        []string{"Chat"},
		Security:    bearer,
	}, func(ctx context.Context, in *struct {
		Slug string `path:"slug" maxLength:"200"`
	}) (*ChatConversationOutput, error) {
		p, err := requirePermission(ctx, auth.PermCourseRead)
		if err != nil {
			return nil, err
		}
		courseID, err := svc.CourseIDBySlug(ctx, p.TenantID, in.Slug)
		if err != nil {
			return nil, chatError(err)
		}
		if !auth.Can(p.Role, auth.PermCourseWrite) {
			enrolled, err := learning.IsEnrolled(ctx, p.TenantID, courseID, p.UserID)
			if err != nil {
				return nil, chatError(err)
			}
			if !enrolled {
				return nil, huma.Error403Forbidden("Enrol in this course to join its channel.")
			}
		}
		conv, err := svc.EnsureCourseChannel(ctx, p.TenantID, courseID, "")
		if err != nil {
			return nil, chatError(err)
		}
		if err := svc.EnsureMember(ctx, p.TenantID, conv.ID, p.UserID); err != nil {
			return nil, chatError(err)
		}
		full, err := svc.Conversation(ctx, p.TenantID, conv.ID)
		if err != nil {
			return nil, chatError(err)
		}
		out := &ChatConversationOutput{CacheControl: chatCacheControl}
		out.Body.Conversation = chatConversationView(full, p.UserID)
		return out, nil
	})
}

// requireChatMember denies a caller who is not in the conversation: 403 when the
// conversation exists (they may know that), 404 when it does not.
func requireChatMember(ctx context.Context, svc *chat.Service, p auth.Principal, convID uuid.UUID) error {
	member, err := svc.IsMember(ctx, p.TenantID, convID, p.UserID)
	if err != nil {
		return chatError(err)
	}
	if member {
		return nil
	}
	if _, err := svc.Conversation(ctx, p.TenantID, convID); err != nil {
		return chatError(err)
	}
	return huma.Error403Forbidden("You are not a member of this conversation.")
}

// requireChatAdmin denies anyone who is not an admin of the group.
func requireChatAdmin(ctx context.Context, svc *chat.Service, p auth.Principal, convID uuid.UUID) error {
	conv, err := svc.Conversation(ctx, p.TenantID, convID)
	if err != nil {
		return chatError(err)
	}
	for _, m := range conv.Members {
		if m.UserID == p.UserID {
			if m.Role == chat.RoleAdmin {
				return nil
			}
			return huma.Error403Forbidden("Only a group admin can do that.")
		}
	}
	return huma.Error403Forbidden("You are not a member of this conversation.")
}

// chatError maps the chat package's sentinels onto status codes.
func chatError(err error) error {
	switch {
	case errors.Is(err, chat.ErrNotFound):
		return huma.Error404NotFound("That conversation does not exist.")
	case errors.Is(err, chat.ErrNotMember):
		return huma.Error403Forbidden("You are not a member of this conversation.")
	case errors.Is(err, chat.ErrInvalid):
		return huma.Error422UnprocessableEntity("That request is not valid.")
	case errors.Is(err, chat.ErrInvalidPage):
		return huma.Error422UnprocessableEntity("That page cursor is not valid.")
	default:
		return err
	}
}
