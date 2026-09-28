package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Errrori/workpilot/internal/risks"
	"github.com/Errrori/workpilot/internal/store"
	"github.com/Errrori/workpilot/internal/ws"
)

const (
	maxRiskTitleRunes       = 200
	maxRiskDescriptionRunes = 2000
	maxRiskOwnerRunes       = 64
	defaultRiskListLimit    = 100
	maxRiskListLimit        = 500
)

// RiskService identifies AI risk suggestions for a group.
type RiskService interface {
	Extract(ctx context.Context, groupID, user string, fileID int64) ([]store.Risk, error)
}

type createRiskRequest struct {
	User        string `json:"user"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Severity    string `json:"severity"`
	Owner       string `json:"owner"`
}

type updateRiskRequest struct {
	User        string  `json:"user"`
	Title       *string `json:"title"`
	Description *string `json:"description"`
	Severity    *string `json:"severity"`
	Status      *string `json:"status"`
	Owner       *string `json:"owner"`
}

type extractRisksRequest struct {
	User   string `json:"user"`
	FileID int64  `json:"file_id"`
}

func listRisks(pool *pgxpool.Pool) gin.HandlerFunc {
	return func(c *gin.Context) {
		groupID := c.Param("id")
		if !ensureGroup(c, pool, groupID) {
			return
		}
		status := strings.TrimSpace(c.Query("status"))
		if status != "" && !store.IsValidRiskStatus(status) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid status"})
			return
		}
		limit := defaultRiskListLimit
		if raw := c.Query("limit"); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= maxRiskListLimit {
				limit = n
			}
		}
		list, err := store.ListRisks(c.Request.Context(), pool, groupID, status, limit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if list == nil {
			list = []store.Risk{}
		}
		c.JSON(http.StatusOK, gin.H{"risks": list})
	}
}

func createRisk(pool *pgxpool.Pool, hub *ws.Hub) gin.HandlerFunc {
	return func(c *gin.Context) {
		groupID := c.Param("id")
		if !ensureGroup(c, pool, groupID) {
			return
		}
		var req createRiskRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json body"})
			return
		}
		user := strings.TrimSpace(req.User)
		if user == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "user is required"})
			return
		}
		title := strings.TrimSpace(req.Title)
		if title == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "title is required"})
			return
		}
		if utf8.RuneCountInString(title) > maxRiskTitleRunes {
			c.JSON(http.StatusBadRequest, gin.H{"error": "title is too long"})
			return
		}
		description := strings.TrimSpace(req.Description)
		if utf8.RuneCountInString(description) > maxRiskDescriptionRunes {
			c.JSON(http.StatusBadRequest, gin.H{"error": "description is too long"})
			return
		}
		owner := strings.TrimSpace(req.Owner)
		if utf8.RuneCountInString(owner) > maxRiskOwnerRunes {
			c.JSON(http.StatusBadRequest, gin.H{"error": "owner is too long"})
			return
		}
		severity := strings.ToLower(strings.TrimSpace(req.Severity))
		if severity == "" {
			severity = store.RiskSeverityMedium
		}
		if !store.IsValidRiskSeverity(severity) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid severity"})
			return
		}

		risk, err := store.InsertRisk(c.Request.Context(), pool, store.RiskInsert{
			GroupID:     groupID,
			Title:       title,
			Description: description,
			Severity:    severity,
			Status:      store.RiskStatusOpen,
			Owner:       owner,
			Source:      store.RiskSourceManual,
			CreatedBy:   user,
		})
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		hub.BroadcastRisk(ws.EventRiskCreated, &risk)
		c.JSON(http.StatusCreated, gin.H{"risk": risk})
	}
}

func updateRisk(pool *pgxpool.Pool, hub *ws.Hub) gin.HandlerFunc {
	return func(c *gin.Context) {
		riskID, ok := parseRiskID(c)
		if !ok {
			return
		}
		groupID := c.Param("id")
		if !ensureGroup(c, pool, groupID) {
			return
		}
		var req updateRiskRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json body"})
			return
		}
		user := strings.TrimSpace(req.User)
		if user == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "user is required"})
			return
		}
		if req.Title == nil && req.Description == nil && req.Severity == nil && req.Status == nil && req.Owner == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "nothing to update"})
			return
		}

		upd := store.RiskUpdate{Actor: user}
		if req.Title != nil {
			title := strings.TrimSpace(*req.Title)
			if title == "" {
				c.JSON(http.StatusBadRequest, gin.H{"error": "title cannot be empty"})
				return
			}
			if utf8.RuneCountInString(title) > maxRiskTitleRunes {
				c.JSON(http.StatusBadRequest, gin.H{"error": "title is too long"})
				return
			}
			upd.Title = &title
		}
		if req.Description != nil {
			description := strings.TrimSpace(*req.Description)
			if utf8.RuneCountInString(description) > maxRiskDescriptionRunes {
				c.JSON(http.StatusBadRequest, gin.H{"error": "description is too long"})
				return
			}
			upd.Description = &description
		}
		if req.Severity != nil {
			severity := strings.ToLower(strings.TrimSpace(*req.Severity))
			if !store.IsValidRiskSeverity(severity) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid severity"})
				return
			}
			upd.Severity = &severity
		}
		if req.Status != nil {
			status := strings.TrimSpace(*req.Status)
			if !store.IsValidRiskStatus(status) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid status"})
				return
			}
			upd.Status = &status
		}
		if req.Owner != nil {
			owner := strings.TrimSpace(*req.Owner)
			if utf8.RuneCountInString(owner) > maxRiskOwnerRunes {
				c.JSON(http.StatusBadRequest, gin.H{"error": "owner is too long"})
				return
			}
			upd.Owner = &owner
		}

		risk, err := store.UpdateRisk(c.Request.Context(), pool, groupID, riskID, upd)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "risk not found"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		hub.BroadcastRisk(ws.EventRiskUpdated, &risk)
		c.JSON(http.StatusOK, gin.H{"risk": risk})
	}
}

func deleteRisk(pool *pgxpool.Pool, hub *ws.Hub) gin.HandlerFunc {
	return func(c *gin.Context) {
		riskID, ok := parseRiskID(c)
		if !ok {
			return
		}
		groupID := c.Param("id")
		if !ensureGroup(c, pool, groupID) {
			return
		}
		risk, err := store.DeleteRisk(c.Request.Context(), pool, groupID, riskID)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "risk not found"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		hub.BroadcastRisk(ws.EventRiskDeleted, &risk)
		c.JSON(http.StatusOK, gin.H{"deleted": true, "risk": risk})
	}
}

func extractRisks(pool *pgxpool.Pool, hub *ws.Hub, svc RiskService) gin.HandlerFunc {
	return func(c *gin.Context) {
		groupID := c.Param("id")
		if !ensureGroup(c, pool, groupID) {
			return
		}
		runExtractRisks(c, pool, hub, svc, groupID)
	}
}

// runExtractRisks validates the request and stores AI suggested risks.
func runExtractRisks(c *gin.Context, pool *pgxpool.Pool, hub *ws.Hub, svc RiskService, groupID string) {
	if svc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "risk service is unavailable"})
		return
	}
	var req extractRisksRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json body"})
		return
	}
	user := strings.TrimSpace(req.User)
	if user == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "user is required"})
		return
	}
	if req.FileID < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid file_id"})
		return
	}
	if req.FileID > 0 {
		_, err := store.GetFile(c.Request.Context(), pool, groupID, req.FileID)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "file not found"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}

	created, err := svc.Extract(c.Request.Context(), groupID, user, req.FileID)
	if errors.Is(err, risks.ErrNoMaterial) {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	if err != nil {
		slog.Error("extract risks", "group_id", groupID, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	for i := range created {
		hub.BroadcastRisk(ws.EventRiskSuggested, &created[i])
	}
	c.JSON(http.StatusOK, gin.H{"risks": created, "count": len(created)})
}

func parseRiskID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("riskID"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid risk id"})
		return 0, false
	}
	return id, true
}
