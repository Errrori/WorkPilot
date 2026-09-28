package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Errrori/workpilot/internal/store"
)

const maxQuestionRunes = 2000

// AskService streams a grounded answer for a group question.
type AskService interface {
	Stream(ctx context.Context, groupID, user, question string, onSources func([]store.Citation) error, onDelta func(string) error) (store.Message, error)
}

type askRequest struct {
	User     string `json:"user"`
	Question string `json:"question"`
}

func askGroup(pool *pgxpool.Pool, svc AskService) gin.HandlerFunc {
	return func(c *gin.Context) {
		groupID := c.Param("id")
		if !ensureGroup(c, pool, groupID) {
			return
		}
		streamAnswer(c, svc, groupID)
	}
}

// streamAnswer validates the request and relays the answer as SSE events.
func streamAnswer(c *gin.Context, svc AskService, groupID string) {
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
	if svc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "qa service is unavailable"})
		return
	}

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	send := func(event string, data any) {
		c.SSEvent(event, data)
		c.Writer.Flush()
	}

	msg, err := svc.Stream(c.Request.Context(), groupID, user, question,
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
	send("done", gin.H{"message": msg})
}
