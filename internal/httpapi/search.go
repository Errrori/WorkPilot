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

const maxSearchTopK = 20

// SearchService returns group-scoped retrieval hits without calling the LLM.
type SearchService interface {
	Search(ctx context.Context, groupID, query string, topK int) ([]store.Citation, error)
}

type searchRequest struct {
	Query string `json:"query"`
	TopK  int    `json:"top_k"`
}

func searchGroup(pool *pgxpool.Pool, svc SearchService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !ensureGroup(c, pool, c.Param("id")) {
			return
		}
		performSearch(c, svc)
	}
}

// performSearch validates the request and relays retrieval hits as JSON.
func performSearch(c *gin.Context, svc SearchService) {
	groupID := c.Param("id")
	var req searchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json body"})
		return
	}
	query := strings.TrimSpace(req.Query)
	if query == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "query is required"})
		return
	}
	if utf8.RuneCountInString(query) > maxQuestionRunes {
		c.JSON(http.StatusBadRequest, gin.H{"error": "query is too long"})
		return
	}
	if req.TopK < 0 || req.TopK > maxSearchTopK {
		c.JSON(http.StatusBadRequest, gin.H{"error": "top_k must be between 1 and 20"})
		return
	}
	if svc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "search service is unavailable"})
		return
	}

	sources, err := svc.Search(c.Request.Context(), groupID, query, req.TopK)
	if err != nil {
		slog.Error("search group", "group_id", groupID, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if sources == nil {
		sources = []store.Citation{}
	}
	c.JSON(http.StatusOK, gin.H{"sources": sources})
}
