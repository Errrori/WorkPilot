package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Errrori/workpilot/internal/docsync"
	"github.com/Errrori/workpilot/internal/store"
	"github.com/Errrori/workpilot/internal/ws"
)

const (
	docSourceNameMaxRunes = 200
	defaultDocSourceLimit = 100
	maxDocSourceLimit     = 500
	defaultDocItemLimit   = 200
	maxDocItemLimit       = 500
)

// DocSourceRunner queues a manual document sync.
type DocSourceRunner interface {
	EnqueueSync(ctx context.Context, sourceID int64) error
}

type createDocSourceRequest struct {
	User       string   `json:"user"`
	Repo       string   `json:"repo"`
	Name       string   `json:"name"`
	Ref        string   `json:"ref"`
	Include    []string `json:"include"`
	Exclude    []string `json:"exclude"`
	Extensions []string `json:"extensions"`
	MaxFileMB  int      `json:"max_file_mb"`
	MaxFiles   int      `json:"max_files"`
}

type updateDocSourceRequest struct {
	User       string    `json:"user"`
	Name       *string   `json:"name"`
	Enabled    *bool     `json:"enabled"`
	Ref        *string   `json:"ref"`
	Include    *[]string `json:"include"`
	Exclude    *[]string `json:"exclude"`
	Extensions *[]string `json:"extensions"`
	MaxFileMB  *int      `json:"max_file_mb"`
	MaxFiles   *int      `json:"max_files"`
}

type syncDocSourceRequest struct {
	User string `json:"user"`
}

func listDocSources(pool *pgxpool.Pool) gin.HandlerFunc {
	return func(c *gin.Context) {
		groupID := c.Param("id")
		if !ensureGroup(c, pool, groupID) {
			return
		}
		limit := defaultDocSourceLimit
		if raw := c.Query("limit"); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= maxDocSourceLimit {
				limit = n
			}
		}
		sources, err := store.ListDocSources(c.Request.Context(), pool, groupID, limit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if sources == nil {
			sources = []store.DocSource{}
		}
		c.JSON(http.StatusOK, gin.H{"doc_sources": sources})
	}
}

func createDocSource(pool *pgxpool.Pool, hub *ws.Hub) gin.HandlerFunc {
	return func(c *gin.Context) {
		groupID := c.Param("id")
		if !ensureGroup(c, pool, groupID) {
			return
		}
		var req createDocSourceRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json body"})
			return
		}
		user := strings.TrimSpace(req.User)
		if user == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "user is required"})
			return
		}
		owner, name, ok := parseRepoRef(req.Repo)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "repo must look like owner/name"})
			return
		}
		cfg, err := docsync.NormalizeConfig(store.DocSourceConfig{
			Owner:      owner,
			Name:       name,
			Ref:        req.Ref,
			Include:    req.Include,
			Exclude:    req.Exclude,
			Extensions: req.Extensions,
			MaxFileMB:  req.MaxFileMB,
			MaxFiles:   req.MaxFiles,
		})
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		sourceName := strings.TrimSpace(req.Name)
		if sourceName == "" {
			sourceName = owner + "/" + name
		}
		if utf8.RuneCountInString(sourceName) > docSourceNameMaxRunes {
			c.JSON(http.StatusBadRequest, gin.H{"error": "name is too long"})
			return
		}

		source, err := store.InsertDocSource(c.Request.Context(), pool, store.DocSourceInsert{
			GroupID:    groupID,
			Provider:   store.DocSourceProviderGitHub,
			Name:       sourceName,
			Config:     cfg,
			CreatedBy:  user,
			NextSyncAt: time.Now(),
		})
		if store.IsUniqueViolation(err) {
			c.JSON(http.StatusConflict, gin.H{"error": "document source already bound to this group"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		hub.BroadcastDocSource(ws.EventDocSourceCreated, &source)
		c.JSON(http.StatusCreated, gin.H{"doc_source": source})
	}
}

func updateDocSource(pool *pgxpool.Pool, hub *ws.Hub) gin.HandlerFunc {
	return func(c *gin.Context) {
		sourceID, ok := parseDocSourceID(c)
		if !ok {
			return
		}
		groupID := c.Param("id")
		if !ensureGroup(c, pool, groupID) {
			return
		}
		var req updateDocSourceRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json body"})
			return
		}
		if strings.TrimSpace(req.User) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "user is required"})
			return
		}
		current, err := store.GetDocSource(c.Request.Context(), pool, groupID, sourceID)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "document source not found"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		upd := store.DocSourceUpdate{Enabled: req.Enabled}
		needsSync := false
		if req.Enabled != nil && *req.Enabled {
			needsSync = true
		}
		if req.Name != nil {
			name := strings.TrimSpace(*req.Name)
			if name == "" || utf8.RuneCountInString(name) > docSourceNameMaxRunes {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid name"})
				return
			}
			upd.Name = &name
		}
		if req.Ref != nil || req.Include != nil || req.Exclude != nil || req.Extensions != nil || req.MaxFileMB != nil || req.MaxFiles != nil {
			cfg := current.Config
			if req.Ref != nil {
				cfg.Ref = *req.Ref
			}
			if req.Include != nil {
				cfg.Include = *req.Include
			}
			if req.Exclude != nil {
				cfg.Exclude = *req.Exclude
			}
			if req.Extensions != nil {
				cfg.Extensions = *req.Extensions
			}
			if req.MaxFileMB != nil {
				cfg.MaxFileMB = *req.MaxFileMB
			}
			if req.MaxFiles != nil {
				cfg.MaxFiles = *req.MaxFiles
			}
			normalized, err := docsync.NormalizeConfig(cfg)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			upd.Config = &normalized
			needsSync = true
		}
		if upd.Name == nil && upd.Config == nil && upd.Enabled == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "nothing to update"})
			return
		}
		if needsSync {
			now := time.Now()
			upd.NextSyncAt = &now
		}
		source, err := store.UpdateDocSource(c.Request.Context(), pool, groupID, sourceID, upd)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "document source not found"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		hub.BroadcastDocSource(ws.EventDocSourceUpdated, &source)
		c.JSON(http.StatusOK, gin.H{"doc_source": source})
	}
}

func deleteDocSource(pool *pgxpool.Pool, hub *ws.Hub) gin.HandlerFunc {
	return func(c *gin.Context) {
		sourceID, ok := parseDocSourceID(c)
		if !ok {
			return
		}
		groupID := c.Param("id")
		if !ensureGroup(c, pool, groupID) {
			return
		}
		source, err := store.DeleteDocSource(c.Request.Context(), pool, groupID, sourceID)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "document source not found"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		hub.BroadcastDocSource(ws.EventDocSourceDeleted, &source)
		c.JSON(http.StatusOK, gin.H{"deleted": true, "doc_source": source})
	}
}

func syncDocSource(pool *pgxpool.Pool, runner DocSourceRunner) gin.HandlerFunc {
	return func(c *gin.Context) {
		sourceID, ok := parseDocSourceID(c)
		if !ok {
			return
		}
		groupID := c.Param("id")
		if !ensureGroup(c, pool, groupID) {
			return
		}
		var req syncDocSourceRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json body"})
			return
		}
		if strings.TrimSpace(req.User) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "user is required"})
			return
		}
		if runner == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "document sync runner is unavailable"})
			return
		}
		if _, err := store.GetDocSource(c.Request.Context(), pool, groupID, sourceID); errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "document source not found"})
			return
		} else if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if err := runner.EnqueueSync(c.Request.Context(), sourceID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusAccepted, gin.H{"queued": true})
	}
}

func listDocSourceItems(pool *pgxpool.Pool) gin.HandlerFunc {
	return func(c *gin.Context) {
		sourceID, ok := parseDocSourceID(c)
		if !ok {
			return
		}
		groupID := c.Param("id")
		if !ensureGroup(c, pool, groupID) {
			return
		}
		if _, err := store.GetDocSource(c.Request.Context(), pool, groupID, sourceID); errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "document source not found"})
			return
		} else if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		state := strings.TrimSpace(c.Query("state"))
		if state != "" && !validDocSourceItemState(state) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid state"})
			return
		}
		limit := defaultDocItemLimit
		if raw := c.Query("limit"); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= maxDocItemLimit {
				limit = n
			}
		}
		items, err := store.ListDocSourceItems(c.Request.Context(), pool, sourceID, state, limit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if items == nil {
			items = []store.DocSourceItem{}
		}
		c.JSON(http.StatusOK, gin.H{"items": items})
	}
}

func validDocSourceItemState(state string) bool {
	switch state {
	case store.DocSourceItemIncluded, store.DocSourceItemExcluded, store.DocSourceItemFailed:
		return true
	default:
		return false
	}
}

func parseDocSourceID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("sourceID"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid document source id"})
		return 0, false
	}
	return id, true
}
