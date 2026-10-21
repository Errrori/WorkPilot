package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Errrori/workpilot/internal/store"
)

const (
	defaultQAConversationLimit = 50
	maxQAConversationLimit     = 200
	defaultQAMessageLimit      = 100
	maxQAMessageLimit          = 200
)

func listQAConversations(pool *pgxpool.Pool) gin.HandlerFunc {
	return func(c *gin.Context) {
		groupID := c.Param("id")
		if !ensureGroup(c, pool, groupID) {
			return
		}
		user := strings.TrimSpace(c.Query("user"))
		if user == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "user is required"})
			return
		}
		limit := qaLimit(c.Query("limit"), defaultQAConversationLimit, maxQAConversationLimit)
		conversations, err := store.ListQAConversations(c.Request.Context(), pool, groupID, user, limit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if conversations == nil {
			conversations = []store.QAConversation{}
		}
		c.JSON(http.StatusOK, gin.H{"conversations": conversations})
	}
}

func listQAMessages(pool *pgxpool.Pool) gin.HandlerFunc {
	return func(c *gin.Context) {
		groupID := c.Param("id")
		if !ensureGroup(c, pool, groupID) {
			return
		}
		user := strings.TrimSpace(c.Query("user"))
		if user == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "user is required"})
			return
		}
		conversationID, ok := parseQAConversationID(c)
		if !ok {
			return
		}
		if _, err := store.GetQAConversation(c.Request.Context(), pool, groupID, user, conversationID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				c.JSON(http.StatusNotFound, gin.H{"error": "conversation not found"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		limit := qaLimit(c.Query("limit"), defaultQAMessageLimit, maxQAMessageLimit)
		messages, err := store.ListQAMessages(c.Request.Context(), pool, conversationID, limit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if messages == nil {
			messages = []store.QAMessage{}
		}
		c.JSON(http.StatusOK, gin.H{"messages": messages})
	}
}

func parseQAConversationID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("conversationID"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid conversation id"})
		return 0, false
	}
	return id, true
}

func qaLimit(raw string, def, max int) int {
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 || n > max {
		return def
	}
	return n
}
