package httpapi

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Errrori/workpilot/internal/store"
)

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
