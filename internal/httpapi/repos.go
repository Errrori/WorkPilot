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

	"github.com/Errrori/workpilot/internal/store"
	"github.com/Errrori/workpilot/internal/ws"
)

const (
	repoRefMaxRunes      = 200
	repoPartMaxRunes     = 100
	defaultRepoListLimit = 100
	maxRepoListLimit     = 500
	defaultRepoItemLimit = 100
	maxRepoItemLimit     = 500
)

// RepoRunner queues a manual repository sync.
type RepoRunner interface {
	EnqueueSync(ctx context.Context, repoID int64) error
}

type createRepoRequest struct {
	User string `json:"user"`
	Repo string `json:"repo"`
}

type updateRepoRequest struct {
	User    string `json:"user"`
	Enabled *bool  `json:"enabled"`
}

type syncRepoRequest struct {
	User string `json:"user"`
}

func listRepos(pool *pgxpool.Pool) gin.HandlerFunc {
	return func(c *gin.Context) {
		groupID := c.Param("id")
		if !ensureGroup(c, pool, groupID) {
			return
		}
		limit := defaultRepoListLimit
		if raw := c.Query("limit"); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= maxRepoListLimit {
				limit = n
			}
		}
		repos, err := store.ListRepos(c.Request.Context(), pool, groupID, limit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if repos == nil {
			repos = []store.Repo{}
		}
		c.JSON(http.StatusOK, gin.H{"repos": repos})
	}
}

func createRepo(pool *pgxpool.Pool, hub *ws.Hub) gin.HandlerFunc {
	return func(c *gin.Context) {
		groupID := c.Param("id")
		if !ensureGroup(c, pool, groupID) {
			return
		}
		var req createRepoRequest
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
		repo, err := store.InsertRepo(c.Request.Context(), pool, store.RepoInsert{
			GroupID:    groupID,
			Provider:   store.RepoProviderGitHub,
			Owner:      owner,
			Name:       name,
			CreatedBy:  user,
			NextSyncAt: time.Now(),
		})
		if store.IsUniqueViolation(err) {
			c.JSON(http.StatusConflict, gin.H{"error": "repo already bound to this group"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		hub.BroadcastRepo(ws.EventRepoCreated, &repo)
		c.JSON(http.StatusCreated, gin.H{"repo": repo})
	}
}

func updateRepo(pool *pgxpool.Pool, hub *ws.Hub) gin.HandlerFunc {
	return func(c *gin.Context) {
		repoID, ok := parseRepoID(c)
		if !ok {
			return
		}
		groupID := c.Param("id")
		if !ensureGroup(c, pool, groupID) {
			return
		}
		var req updateRepoRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json body"})
			return
		}
		if strings.TrimSpace(req.User) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "user is required"})
			return
		}
		if req.Enabled == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "nothing to update"})
			return
		}

		upd := store.RepoUpdate{Enabled: req.Enabled}
		if *req.Enabled {
			now := time.Now()
			upd.NextSyncAt = &now
		}
		repo, err := store.UpdateRepo(c.Request.Context(), pool, groupID, repoID, upd)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "repo not found"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		hub.BroadcastRepo(ws.EventRepoUpdated, &repo)
		c.JSON(http.StatusOK, gin.H{"repo": repo})
	}
}

func deleteRepo(pool *pgxpool.Pool, hub *ws.Hub) gin.HandlerFunc {
	return func(c *gin.Context) {
		repoID, ok := parseRepoID(c)
		if !ok {
			return
		}
		groupID := c.Param("id")
		if !ensureGroup(c, pool, groupID) {
			return
		}
		repo, err := store.DeleteRepo(c.Request.Context(), pool, groupID, repoID)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "repo not found"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		hub.BroadcastRepo(ws.EventRepoDeleted, &repo)
		c.JSON(http.StatusOK, gin.H{"deleted": true, "repo": repo})
	}
}

func syncRepo(pool *pgxpool.Pool, runner RepoRunner) gin.HandlerFunc {
	return func(c *gin.Context) {
		repoID, ok := parseRepoID(c)
		if !ok {
			return
		}
		groupID := c.Param("id")
		if !ensureGroup(c, pool, groupID) {
			return
		}
		runSyncRepo(c, pool, runner, groupID, repoID)
	}
}

// runSyncRepo validates the request and queues a manual repository sync.
func runSyncRepo(c *gin.Context, pool *pgxpool.Pool, runner RepoRunner, groupID string, repoID int64) {
	var req syncRepoRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json body"})
		return
	}
	if strings.TrimSpace(req.User) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "user is required"})
		return
	}
	if runner == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "git sync runner is unavailable"})
		return
	}
	if _, err := store.GetRepo(c.Request.Context(), pool, groupID, repoID); errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "repo not found"})
		return
	} else if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if err := runner.EnqueueSync(c.Request.Context(), repoID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"queued": true})
}

func listRepoItems(pool *pgxpool.Pool) gin.HandlerFunc {
	return func(c *gin.Context) {
		repoID, ok := parseRepoID(c)
		if !ok {
			return
		}
		groupID := c.Param("id")
		if !ensureGroup(c, pool, groupID) {
			return
		}
		if _, err := store.GetRepo(c.Request.Context(), pool, groupID, repoID); errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "repo not found"})
			return
		} else if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		kind := strings.TrimSpace(c.Query("kind"))
		if kind != "" && !isValidRepoItemKind(kind) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid kind"})
			return
		}
		limit := defaultRepoItemLimit
		if raw := c.Query("limit"); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= maxRepoItemLimit {
				limit = n
			}
		}
		items, err := store.ListRepoItems(c.Request.Context(), pool, repoID, kind, limit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if items == nil {
			items = []store.RepoItem{}
		}
		c.JSON(http.StatusOK, gin.H{"items": items})
	}
}

// parseRepoRef splits "owner/name" into its parts.
func parseRepoRef(raw string) (string, string, bool) {
	ref := strings.TrimSpace(raw)
	if ref == "" || utf8.RuneCountInString(ref) > repoRefMaxRunes {
		return "", "", false
	}
	owner, name, found := strings.Cut(ref, "/")
	if !found {
		return "", "", false
	}
	owner, name = strings.TrimSpace(owner), strings.TrimSpace(name)
	if !validRepoPart(owner) || !validRepoPart(name) {
		return "", "", false
	}
	return owner, name, true
}

func validRepoPart(part string) bool {
	if part == "" || utf8.RuneCountInString(part) > repoPartMaxRunes {
		return false
	}
	for _, r := range part {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

func isValidRepoItemKind(kind string) bool {
	switch kind {
	case store.RepoKindPullRequest, store.RepoKindIssue, store.RepoKindCommit:
		return true
	default:
		return false
	}
}

func parseRepoID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("repoID"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid repo id"})
		return 0, false
	}
	return id, true
}
