package httpapi

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Errrori/workpilot/internal/store"
)

const maxGroupNameRunes = 100

func listGroups(pool *pgxpool.Pool) gin.HandlerFunc {
	return func(c *gin.Context) {
		groups, err := store.ListGroups(c.Request.Context(), pool)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if groups == nil {
			groups = []store.Group{}
		}
		c.JSON(http.StatusOK, gin.H{"groups": groups})
	}
}

type createGroupRequest struct {
	Name string `json:"name"`
}

func createGroup(pool *pgxpool.Pool) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req createGroupRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json body"})
			return
		}
		name := strings.TrimSpace(req.Name)
		if name == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
			return
		}
		if utf8.RuneCountInString(name) > maxGroupNameRunes {
			c.JSON(http.StatusBadRequest, gin.H{"error": "name is too long"})
			return
		}
		group, err := store.CreateGroup(c.Request.Context(), pool, name)
		if err != nil {
			slog.Error("create group", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusCreated, gin.H{"group": group})
	}
}

func listMessages(pool *pgxpool.Pool) gin.HandlerFunc {
	return func(c *gin.Context) {
		limit := 50
		if raw := c.Query("limit"); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 200 {
				limit = n
			}
		}
		messages, err := store.ListMessages(c.Request.Context(), pool, c.Param("id"), limit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if messages == nil {
			messages = []store.Message{}
		}
		c.JSON(http.StatusOK, gin.H{"messages": messages})
	}
}
