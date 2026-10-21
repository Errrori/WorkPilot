package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Errrori/workpilot/internal/store"
)

const maxQuestionRunes = 2000

// AskService streams a grounded answer for a private QA conversation.
type AskService interface {
	Stream(ctx context.Context, groupID, user string, conversationID int64, question string, onConversation func(store.QAConversation) error, onSources func([]store.Citation) error, onDelta func(string) error) (store.QAMessage, error)
}

// QAConversationReader validates that a conversation belongs to the caller.
type QAConversationReader interface {
	GetConversation(ctx context.Context, groupID, userName string, id int64) (store.QAConversation, error)
}

type askRequest struct {
	User           string `json:"user"`
	Question       string `json:"question"`
	ConversationID int64  `json:"conversation_id"`
}

func askGroup(pool *pgxpool.Pool, svc AskService) gin.HandlerFunc {
	return func(c *gin.Context) {
		groupID := c.Param("id")
		if !ensureGroup(c, pool, groupID) {
			return
		}
		streamAnswer(c, store.QAStore{Pool: pool}, svc, groupID)
	}
}

// streamAnswer validates the request and relays the answer as SSE events.
func streamAnswer(c *gin.Context, conversations QAConversationReader, svc AskService, groupID string) {
	var req askRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json body"})
		return
	}
	user := strings.TrimSpace(req.User)
	question := strings.TrimSpace(req.Question)
	if user == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "user is required"})
		return
	}
	if question == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "question is required"})
		return
	}
	if utf8.RuneCountInString(question) > maxQuestionRunes {
		c.JSON(http.StatusBadRequest, gin.H{"error": "question is too long"})
		return
	}
	if req.ConversationID < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid conversation id"})
		return
	}
	if svc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "qa service is unavailable"})
		return
	}
	if req.ConversationID > 0 {
		if _, err := conversations.GetConversation(c.Request.Context(), groupID, user, req.ConversationID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				c.JSON(http.StatusNotFound, gin.H{"error": "conversation not found"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	send := func(event string, data any) {
		c.SSEvent(event, data)
		c.Writer.Flush()
	}

	var conversation store.QAConversation
	msg, err := svc.Stream(c.Request.Context(), groupID, user, req.ConversationID, question,
		func(conv store.QAConversation) error {
			conversation = conv
			send("conversation", gin.H{"conversation": conv})
			return nil
		},
		func(citations []store.Citation) error {
			send("sources", gin.H{"sources": citations})
			return nil
		},
		func(delta string) error {
			send("delta", gin.H{"text": delta})
			return nil
		})
	if err != nil {
		slog.Error("ask group", "group_id", groupID, "error", err)
		send("error", gin.H{"error": err.Error()})
		return
	}
	send("done", gin.H{"message": msg, "conversation": conversation})
}
