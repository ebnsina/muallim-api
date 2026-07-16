package httpapi

// The chat realtime layer. Messages persist over REST; this delivers them — plus
// typing and presence — live over a WebSocket. Fan-out is Postgres LISTEN/NOTIFY:
// an event published on any API instance reaches the sockets held by every
// instance, so it is correct behind a load balancer with no Redis. The transport
// is deliberately narrow (Notify/Listen on the DB); swapping it for Redis or NATS
// later is a database method, not a rewrite.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/ebnsina/muallim-api/internal/auth"
	"github.com/ebnsina/muallim-api/internal/chat"
	"github.com/ebnsina/muallim-api/internal/platform/database"
)

// ChatNotifyChannel is the Postgres NOTIFY channel every chat event rides.
const ChatNotifyChannel = "chat_events"

// chatEvent is one realtime event — JSON both over NOTIFY and over the socket.
type chatEvent struct {
	Type           string         `json:"type"` // "message" | "typing" | "presence"
	TenantID       uuid.UUID      `json:"tenant_id"`
	ConversationID uuid.UUID      `json:"conversation_id,omitempty"`
	UserID         uuid.UUID      `json:"user_id,omitempty"`
	Online         bool           `json:"online,omitempty"`
	Message        *chatWSMessage `json:"message,omitempty"`
}

type chatWSMessage struct {
	ID         string `json:"id"`
	SenderID   string `json:"sender_id"`
	SenderName string `json:"sender_name"`
	Body       string `json:"body"`
	CreatedAt  string `json:"created_at"`
}

type wsClient struct {
	userID   uuid.UUID
	tenantID uuid.UUID
	conn     *websocket.Conn
	writeMu  sync.Mutex
}

func (c *wsClient) send(payload []byte) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.conn.Write(ctx, websocket.MessageText, payload)
}

type wsTicket struct {
	userID   uuid.UUID
	tenantID uuid.UUID
	expires  time.Time
}

// ChatHub tracks live sockets and routes events to them. One per process.
type ChatHub struct {
	db      *database.DB
	svc     *chat.Service
	origins []string

	mu      sync.RWMutex
	byConv  map[uuid.UUID]map[*wsClient]bool
	byUser  map[uuid.UUID]map[*wsClient]bool
	tickets map[string]wsTicket
}

// NewChatHub builds the hub. Start its listener with `go db.Listen(ctx,
// ChatNotifyChannel, hub.Dispatch)` from the process that owns the lifecycle.
func NewChatHub(db *database.DB, svc *chat.Service, origins []string) *ChatHub {
	return &ChatHub{
		db: db, svc: svc, origins: origins,
		byConv:  map[uuid.UUID]map[*wsClient]bool{},
		byUser:  map[uuid.UUID]map[*wsClient]bool{},
		tickets: map[string]wsTicket{},
	}
}

func (h *ChatHub) newTicket(userID, tenantID uuid.UUID) string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	t := hex.EncodeToString(b)
	h.mu.Lock()
	h.tickets[t] = wsTicket{userID: userID, tenantID: tenantID, expires: time.Now().Add(time.Minute)}
	for k, v := range h.tickets { // opportunistic sweep of the expired
		if time.Now().After(v.expires) {
			delete(h.tickets, k)
		}
	}
	h.mu.Unlock()
	return t
}

func (h *ChatHub) redeem(t string) (uuid.UUID, uuid.UUID, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	tk, ok := h.tickets[t]
	delete(h.tickets, t) // single use, whether valid or not
	if !ok || time.Now().After(tk.expires) {
		return uuid.Nil, uuid.Nil, false
	}
	return tk.userID, tk.tenantID, true
}

func (h *ChatHub) register(c *wsClient) (firstForUser bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	set := h.byUser[c.userID]
	firstForUser = len(set) == 0
	if set == nil {
		set = map[*wsClient]bool{}
		h.byUser[c.userID] = set
	}
	set[c] = true
	return
}

func (h *ChatHub) unregister(c *wsClient) (lastForUser bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if set := h.byUser[c.userID]; set != nil {
		delete(set, c)
		if len(set) == 0 {
			delete(h.byUser, c.userID)
			lastForUser = true
		}
	}
	for conv, set := range h.byConv {
		delete(set, c)
		if len(set) == 0 {
			delete(h.byConv, conv)
		}
	}
	return
}

func (h *ChatHub) subscribe(c *wsClient, conv uuid.UUID) {
	h.mu.Lock()
	defer h.mu.Unlock()
	set := h.byConv[conv]
	if set == nil {
		set = map[*wsClient]bool{}
		h.byConv[conv] = set
	}
	set[c] = true
}

func (h *ChatHub) publish(ctx context.Context, e chatEvent) {
	payload, err := json.Marshal(e)
	if err != nil {
		return
	}
	_ = h.db.Notify(ctx, ChatNotifyChannel, string(payload))
}

// Dispatch delivers one NOTIFY payload to this instance's matching sockets. It
// collects targets under the read lock, then writes without it, so a slow socket
// cannot block routing.
func (h *ChatHub) Dispatch(payload string) {
	var e chatEvent
	if json.Unmarshal([]byte(payload), &e) != nil {
		return
	}
	raw := []byte(payload)

	var targets []*wsClient
	h.mu.RLock()
	switch e.Type {
	case "message", "typing":
		for c := range h.byConv[e.ConversationID] {
			if e.Type == "typing" && c.userID == e.UserID {
				continue // never echo a typing signal to its sender
			}
			targets = append(targets, c)
		}
	case "presence":
		for _, set := range h.byUser {
			for c := range set {
				if c.userID != e.UserID {
					targets = append(targets, c)
				}
			}
		}
	}
	h.mu.RUnlock()

	for _, c := range targets {
		c.send(raw)
	}
}

func (h *ChatHub) handleWS(w http.ResponseWriter, r *http.Request) {
	userID, tenantID, ok := h.redeem(r.URL.Query().Get("ticket"))
	if !ok {
		http.Error(w, "invalid or expired chat ticket", http.StatusUnauthorized)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: h.origins})
	if err != nil {
		return
	}
	defer conn.CloseNow()

	c := &wsClient{userID: userID, tenantID: tenantID, conn: conn}
	if first := h.register(c); first {
		h.publish(r.Context(), chatEvent{Type: "presence", TenantID: tenantID, UserID: userID, Online: true})
	}
	defer func() {
		if last := h.unregister(c); last {
			h.publish(context.Background(), chatEvent{Type: "presence", TenantID: tenantID, UserID: userID, Online: false})
		}
	}()

	ctx := r.Context()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var in struct {
			Type           string    `json:"type"`
			ConversationID uuid.UUID `json:"conversation_id"`
		}
		if json.Unmarshal(data, &in) != nil {
			continue
		}
		switch in.Type {
		case "subscribe":
			// Membership is proven once here, so routing later needs no DB hit.
			if member, err := h.svc.IsMember(ctx, tenantID, in.ConversationID, userID); err == nil && member {
				h.subscribe(c, in.ConversationID)
			}
		case "typing":
			if member, err := h.svc.IsMember(ctx, tenantID, in.ConversationID, userID); err == nil && member {
				h.publish(ctx, chatEvent{Type: "typing", TenantID: tenantID, ConversationID: in.ConversationID, UserID: userID})
			}
		}
	}
}

// publishMessage is called by the REST send handler after a message persists, so
// it reaches every member's live socket.
func (h *ChatHub) publishMessage(ctx context.Context, tenantID uuid.UUID, m chat.Message) {
	h.publish(ctx, chatEvent{
		Type:           "message",
		TenantID:       tenantID,
		ConversationID: m.ConversationID,
		Message: &chatWSMessage{
			ID: m.ID.String(), SenderID: m.SenderID.String(), SenderName: m.SenderName,
			Body: m.Body, CreatedAt: m.CreatedAt.Format(time.RFC3339),
		},
	})
}

// registerChatWS mounts the ticket endpoint (on the huma API) and the raw
// WebSocket route (on the mux, since a socket is not an OpenAPI operation).
func registerChatWS(api huma.API, mux *http.ServeMux, hub *ChatHub) {
	// The ticket op is registered even when the hub is nil, so it appears in the
	// contract dumped from a service-less handler. Only a real, running server has
	// a hub; the spec dump never invokes the handler.
	huma.Register(api, huma.Operation{
		OperationID: "chat-ws-ticket",
		Method:      http.MethodPost,
		Path:        "/v1/chat/ws-ticket",
		Summary:     "Mint a short-lived, single-use ticket to open the chat WebSocket",
		Tags:        []string{"Chat"},
		Security:    []map[string][]string{{"bearer": {}}},
	}, func(ctx context.Context, _ *struct{}) (*struct {
		Body struct {
			Ticket string `json:"ticket"`
		}
	}, error) {
		p, err := requirePermission(ctx, auth.PermCourseRead)
		if err != nil {
			return nil, err
		}
		if hub == nil {
			return nil, huma.Error503ServiceUnavailable("Chat realtime is not enabled.")
		}
		out := &struct {
			Body struct {
				Ticket string `json:"ticket"`
			}
		}{}
		out.Body.Ticket = hub.newTicket(p.UserID, p.TenantID)
		return out, nil
	})

	if hub != nil {
		mux.HandleFunc("/v1/chat/ws", hub.handleWS)
	}
}
