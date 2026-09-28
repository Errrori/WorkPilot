package ws

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Errrori/workpilot/internal/store"
)

const (
	writeWait  = 10 * time.Second
	pongWait   = 60 * time.Second
	pingPeriod = pongWait / 2
	maxMsgSize = 64 * 1024
)

const (
	EventFileUploaded    = "file_uploaded"
	EventFileDeleted     = "file_deleted"
	EventFileParsed      = "file_parsed"
	EventFileParseFailed = "file_parse_failed"
	EventFileIndexed     = "file_indexed"
	EventFileIndexFailed = "file_index_failed"
	EventTaskSuggested   = "task_suggested"
	EventTaskCreated     = "task_created"
	EventTaskUpdated     = "task_updated"
	EventTaskDeleted     = "task_deleted"
	EventRiskSuggested   = "risk_suggested"
	EventRiskCreated     = "risk_created"
	EventRiskUpdated     = "risk_updated"
	EventRiskDeleted     = "risk_deleted"
	EventAiTaskCreated   = "ai_task_created"
	EventAiTaskUpdated   = "ai_task_updated"
	EventAiTaskDeleted   = "ai_task_deleted"
	EventReportCreated   = "report_created"
	EventRepoCreated     = "repo_created"
	EventRepoUpdated     = "repo_updated"
	EventRepoDeleted     = "repo_deleted"
	EventRepoSynced      = "repo_synced"
)

type Hub struct {
	ctx      context.Context
	pool     *pgxpool.Pool
	mu       sync.RWMutex
	rooms    map[string]map[*client]struct{}
	upgrader websocket.Upgrader
}

type client struct {
	hub      *Hub
	conn     *websocket.Conn
	send     chan []byte
	groupID  string
	userName string
}

type inbound struct {
	Type    string `json:"type"`
	Content string `json:"content"`
}

type outbound struct {
	Type    string         `json:"type"`
	Message *store.Message `json:"message,omitempty"`
	File    *store.File    `json:"file,omitempty"`
	Task    *store.Task    `json:"task,omitempty"`
	Risk    *store.Risk    `json:"risk,omitempty"`
	AiTask  *store.AiTask  `json:"ai_task,omitempty"`
	Report  *store.Report  `json:"report,omitempty"`
	Repo    *store.Repo    `json:"repo,omitempty"`
	Error   string         `json:"error,omitempty"`
}

func NewHub(ctx context.Context, pool *pgxpool.Pool) *Hub {
	return &Hub{
		ctx:   ctx,
		pool:  pool,
		rooms: make(map[string]map[*client]struct{}),
		upgrader: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			CheckOrigin:     func(*http.Request) bool { return true },
		},
	}
}

func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request) {
	groupID := r.URL.Query().Get("group_id")
	userName := r.URL.Query().Get("user")
	if groupID == "" || userName == "" {
		http.Error(w, "group_id and user are required", http.StatusBadRequest)
		return
	}
	exists, err := store.GroupExists(h.ctx, h.pool, groupID)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	if !exists {
		http.Error(w, "group not found", http.StatusNotFound)
		return
	}

	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c := &client{hub: h, conn: conn, send: make(chan []byte, 64), groupID: groupID, userName: userName}
	h.add(c)
	go c.writePump()
	go c.readPump()
}

func (h *Hub) add(c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.rooms[c.groupID] == nil {
		h.rooms[c.groupID] = make(map[*client]struct{})
	}
	h.rooms[c.groupID][c] = struct{}{}
}

func (h *Hub) remove(c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if room, ok := h.rooms[c.groupID]; ok {
		delete(room, c)
		if len(room) == 0 {
			delete(h.rooms, c.groupID)
		}
	}
}

func (h *Hub) broadcast(groupID string, payload []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.rooms[groupID] {
		select {
		case c.send <- payload:
		default:
		}
	}
}

// BroadcastFile sends a file event to every client in the group room.
func (h *Hub) BroadcastFile(event string, f *store.File) {
	if f == nil {
		return
	}
	payload, err := json.Marshal(outbound{Type: event, File: f})
	if err != nil {
		slog.Warn("broadcast file event", "error", err)
		return
	}
	h.broadcast(f.GroupID, payload)
}

// BroadcastTask sends a task event to every client in the group room.
func (h *Hub) BroadcastTask(event string, t *store.Task) {
	if t == nil {
		return
	}
	payload, err := json.Marshal(outbound{Type: event, Task: t})
	if err != nil {
		slog.Warn("broadcast task event", "error", err)
		return
	}
	h.broadcast(t.GroupID, payload)
}

// BroadcastRisk sends a risk event to every client in the group room.
func (h *Hub) BroadcastRisk(event string, r *store.Risk) {
	if r == nil {
		return
	}
	payload, err := json.Marshal(outbound{Type: event, Risk: r})
	if err != nil {
		slog.Warn("broadcast risk event", "error", err)
		return
	}
	h.broadcast(r.GroupID, payload)
}

// BroadcastAiTask sends an AI task event to every client in the group room.
func (h *Hub) BroadcastAiTask(event string, t *store.AiTask) {
	if t == nil {
		return
	}
	payload, err := json.Marshal(outbound{Type: event, AiTask: t})
	if err != nil {
		slog.Warn("broadcast ai task event", "error", err)
		return
	}
	h.broadcast(t.GroupID, payload)
}

// BroadcastReport sends a report event to every client in the group room.
func (h *Hub) BroadcastReport(event string, r *store.Report) {
	if r == nil {
		return
	}
	payload, err := json.Marshal(outbound{Type: event, Report: r})
	if err != nil {
		slog.Warn("broadcast report event", "error", err)
		return
	}
	h.broadcast(r.GroupID, payload)
}

// BroadcastRepo sends a repository event to every client in the group room.
func (h *Hub) BroadcastRepo(event string, r *store.Repo) {
	if r == nil {
		return
	}
	payload, err := json.Marshal(outbound{Type: event, Repo: r})
	if err != nil {
		slog.Warn("broadcast repo event", "error", err)
		return
	}
	h.broadcast(r.GroupID, payload)
}

// BroadcastMessage sends a chat message to every client in the group room,
// used for AI-published reports.
func (h *Hub) BroadcastMessage(m *store.Message) {
	if m == nil {
		return
	}
	payload, err := json.Marshal(outbound{Type: "message", Message: m})
	if err != nil {
		slog.Warn("broadcast message event", "error", err)
		return
	}
	h.broadcast(m.GroupID, payload)
}

func (c *client) readPump() {
	defer func() {
		c.hub.remove(c)
		close(c.send)
		c.conn.Close()
	}()

	c.conn.SetReadLimit(maxMsgSize)
	_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(pongWait))
	})

	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		var msg inbound
		if err := json.Unmarshal(data, &msg); err != nil || msg.Type != "message" || msg.Content == "" {
			c.pushError("invalid payload")
			continue
		}
		saved, err := store.InsertMessage(c.hub.ctx, c.hub.pool, c.groupID, c.userName, msg.Content, nil)
		if err != nil {
			slog.Warn("insert message", "group_id", c.groupID, "error", err)
			c.pushError("failed to save message")
			continue
		}
		payload, err := json.Marshal(outbound{Type: "message", Message: &saved})
		if err != nil {
			continue
		}
		c.hub.broadcast(c.groupID, payload)
	}
}

func (c *client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()

	for {
		select {
		case payload, ok := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, payload); err != nil {
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func (c *client) pushError(message string) {
	payload, err := json.Marshal(outbound{Type: "error", Error: message})
	if err != nil {
		return
	}
	select {
	case c.send <- payload:
	default:
	}
}
