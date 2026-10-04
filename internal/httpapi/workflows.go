package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Errrori/workpilot/internal/aitasks"
	"github.com/Errrori/workpilot/internal/store"
	"github.com/Errrori/workpilot/internal/workflow"
	"github.com/Errrori/workpilot/internal/ws"
)

const (
	maxWorkflowNameRunes        = 200
	maxWorkflowDescriptionRunes = 2000
	maxWorkflowScheduleRunes    = 100
	maxWorkflowTimezoneRunes    = 64
	defaultWorkflowListLimit    = 100
	maxWorkflowListLimit        = 500
	defaultWorkflowRunListLimit = 20
	maxWorkflowRunListLimit     = 100
)

// WorkflowRunner queues a manual workflow run.
type WorkflowRunner interface {
	EnqueueRun(ctx context.Context, workflowID int64, trigger, actor string) error
}

type createWorkflowRequest struct {
	User        string          `json:"user"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Definition  json.RawMessage `json:"definition"`
	Enabled     *bool           `json:"enabled"`
	Schedule    string          `json:"schedule"`
	Timezone    string          `json:"timezone"`
}

type updateWorkflowRequest struct {
	User        string          `json:"user"`
	Name        *string         `json:"name"`
	Description *string         `json:"description"`
	Definition  json.RawMessage `json:"definition"`
	Enabled     *bool           `json:"enabled"`
	Schedule    *string         `json:"schedule"`
	Timezone    *string         `json:"timezone"`
}

type runWorkflowRequest struct {
	User string `json:"user"`
}

func listWorkflows(pool *pgxpool.Pool) gin.HandlerFunc {
	return func(c *gin.Context) {
		groupID := c.Param("id")
		if !ensureGroup(c, pool, groupID) {
			return
		}
		limit := defaultWorkflowListLimit
		if raw := c.Query("limit"); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= maxWorkflowListLimit {
				limit = n
			}
		}
		list, err := store.ListWorkflows(c.Request.Context(), pool, groupID, limit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if list == nil {
			list = []store.Workflow{}
		}
		c.JSON(http.StatusOK, gin.H{"workflows": list})
	}
}

func createWorkflow(pool *pgxpool.Pool, hub *ws.Hub, defaultTimezone string) gin.HandlerFunc {
	return func(c *gin.Context) {
		groupID := c.Param("id")
		if !ensureGroup(c, pool, groupID) {
			return
		}
		var req createWorkflowRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json body"})
			return
		}
		user := strings.TrimSpace(req.User)
		if user == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "user is required"})
			return
		}
		name := strings.TrimSpace(req.Name)
		if name == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
			return
		}
		if utf8.RuneCountInString(name) > maxWorkflowNameRunes {
			c.JSON(http.StatusBadRequest, gin.H{"error": "name is too long"})
			return
		}
		description := strings.TrimSpace(req.Description)
		if utf8.RuneCountInString(description) > maxWorkflowDescriptionRunes {
			c.JSON(http.StatusBadRequest, gin.H{"error": "description is too long"})
			return
		}
		timezone := strings.TrimSpace(req.Timezone)
		if timezone == "" {
			timezone = defaultTimezone
		}
		if utf8.RuneCountInString(timezone) > maxWorkflowTimezoneRunes {
			c.JSON(http.StatusBadRequest, gin.H{"error": "timezone is too long"})
			return
		}
		schedule := strings.TrimSpace(req.Schedule)
		if utf8.RuneCountInString(schedule) > maxWorkflowScheduleRunes {
			c.JSON(http.StatusBadRequest, gin.H{"error": "schedule is too long"})
			return
		}
		nextRun, err := nextWorkflowRun(schedule, timezone, time.Now())
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		definition, err := normalizeWorkflowDefinition(req.Definition)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		enabled := true
		if req.Enabled != nil {
			enabled = *req.Enabled
		}

		wf, err := store.InsertWorkflow(c.Request.Context(), pool, store.WorkflowInsert{
			GroupID:     groupID,
			Name:        name,
			Description: description,
			Definition:  definition,
			Enabled:     enabled,
			Schedule:    schedule,
			Timezone:    timezone,
			CreatedBy:   user,
			NextRunAt:   nextRun,
		})
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		hub.BroadcastWorkflow(ws.EventWorkflowCreated, &wf)
		c.JSON(http.StatusCreated, gin.H{"workflow": wf})
	}
}

func updateWorkflow(pool *pgxpool.Pool, hub *ws.Hub) gin.HandlerFunc {
	return func(c *gin.Context) {
		workflowID, ok := parseWorkflowID(c)
		if !ok {
			return
		}
		groupID := c.Param("id")
		if !ensureGroup(c, pool, groupID) {
			return
		}
		var req updateWorkflowRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json body"})
			return
		}
		user := strings.TrimSpace(req.User)
		if user == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "user is required"})
			return
		}
		hasDefinition := len(req.Definition) > 0 && string(req.Definition) != "null"
		if req.Name == nil && req.Description == nil && !hasDefinition && req.Enabled == nil && req.Schedule == nil && req.Timezone == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "nothing to update"})
			return
		}

		current, err := store.GetWorkflow(c.Request.Context(), pool, groupID, workflowID)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		upd := store.WorkflowUpdate{}
		if req.Name != nil {
			name := strings.TrimSpace(*req.Name)
			if name == "" {
				c.JSON(http.StatusBadRequest, gin.H{"error": "name cannot be empty"})
				return
			}
			if utf8.RuneCountInString(name) > maxWorkflowNameRunes {
				c.JSON(http.StatusBadRequest, gin.H{"error": "name is too long"})
				return
			}
			upd.Name = &name
		}
		if req.Description != nil {
			description := strings.TrimSpace(*req.Description)
			if utf8.RuneCountInString(description) > maxWorkflowDescriptionRunes {
				c.JSON(http.StatusBadRequest, gin.H{"error": "description is too long"})
				return
			}
			upd.Description = &description
		}
		if hasDefinition {
			definition, err := normalizeWorkflowDefinition(req.Definition)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			upd.Definition = &definition
		}
		schedule := current.Schedule
		if req.Schedule != nil {
			schedule = strings.TrimSpace(*req.Schedule)
			if utf8.RuneCountInString(schedule) > maxWorkflowScheduleRunes {
				c.JSON(http.StatusBadRequest, gin.H{"error": "schedule is too long"})
				return
			}
			upd.Schedule = &schedule
		}
		timezone := current.Timezone
		if req.Timezone != nil {
			timezone = strings.TrimSpace(*req.Timezone)
			if timezone == "" {
				c.JSON(http.StatusBadRequest, gin.H{"error": "timezone cannot be empty"})
				return
			}
			if utf8.RuneCountInString(timezone) > maxWorkflowTimezoneRunes {
				c.JSON(http.StatusBadRequest, gin.H{"error": "timezone is too long"})
				return
			}
			upd.Timezone = &timezone
		}
		if req.Schedule != nil || req.Timezone != nil {
			nextRun, err := nextWorkflowRun(schedule, timezone, time.Now())
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			upd.NextRunAt = &nextRun
		}
		if req.Enabled != nil {
			upd.Enabled = req.Enabled
		}

		wf, err := store.UpdateWorkflow(c.Request.Context(), pool, groupID, workflowID, upd)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		hub.BroadcastWorkflow(ws.EventWorkflowUpdated, &wf)
		c.JSON(http.StatusOK, gin.H{"workflow": wf})
	}
}

func deleteWorkflow(pool *pgxpool.Pool, hub *ws.Hub) gin.HandlerFunc {
	return func(c *gin.Context) {
		workflowID, ok := parseWorkflowID(c)
		if !ok {
			return
		}
		groupID := c.Param("id")
		if !ensureGroup(c, pool, groupID) {
			return
		}
		wf, err := store.DeleteWorkflow(c.Request.Context(), pool, groupID, workflowID)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		hub.BroadcastWorkflow(ws.EventWorkflowDeleted, &wf)
		c.JSON(http.StatusOK, gin.H{"deleted": true, "workflow": wf})
	}
}

func runWorkflow(pool *pgxpool.Pool, runner WorkflowRunner) gin.HandlerFunc {
	return func(c *gin.Context) {
		workflowID, ok := parseWorkflowID(c)
		if !ok {
			return
		}
		groupID := c.Param("id")
		if !ensureGroup(c, pool, groupID) {
			return
		}
		var req runWorkflowRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json body"})
			return
		}
		user := strings.TrimSpace(req.User)
		if user == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "user is required"})
			return
		}
		if _, err := store.GetWorkflow(c.Request.Context(), pool, groupID, workflowID); errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
			return
		} else if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if runner == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "workflow runner is unavailable"})
			return
		}
		if err := runner.EnqueueRun(c.Request.Context(), workflowID, store.WorkflowTriggerManual, user); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusAccepted, gin.H{"queued": true})
	}
}

func listWorkflowRuns(pool *pgxpool.Pool) gin.HandlerFunc {
	return func(c *gin.Context) {
		workflowID, ok := parseWorkflowID(c)
		if !ok {
			return
		}
		groupID := c.Param("id")
		if !ensureGroup(c, pool, groupID) {
			return
		}
		if _, err := store.GetWorkflow(c.Request.Context(), pool, groupID, workflowID); errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
			return
		} else if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		limit := defaultWorkflowRunListLimit
		if raw := c.Query("limit"); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= maxWorkflowRunListLimit {
				limit = n
			}
		}
		runs, err := store.ListWorkflowRuns(c.Request.Context(), pool, groupID, workflowID, limit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if runs == nil {
			runs = []store.WorkflowRun{}
		}
		c.JSON(http.StatusOK, gin.H{"runs": runs})
	}
}

func getWorkflowRun(pool *pgxpool.Pool) gin.HandlerFunc {
	return func(c *gin.Context) {
		workflowID, ok := parseWorkflowID(c)
		if !ok {
			return
		}
		runID, ok := parseWorkflowRunID(c)
		if !ok {
			return
		}
		groupID := c.Param("id")
		if !ensureGroup(c, pool, groupID) {
			return
		}
		run, err := store.GetWorkflowRun(c.Request.Context(), pool, groupID, runID)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "workflow run not found"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if run.WorkflowID != workflowID {
			c.JSON(http.StatusNotFound, gin.H{"error": "workflow run not found"})
			return
		}
		nodes, err := store.ListWorkflowNodeRuns(c.Request.Context(), pool, runID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if nodes == nil {
			nodes = []store.WorkflowNodeRun{}
		}
		run.Nodes = nodes
		c.JSON(http.StatusOK, gin.H{"run": run})
	}
}

// normalizeWorkflowDefinition validates and canonicalizes a definition body.
func normalizeWorkflowDefinition(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, errors.New("definition is required")
	}
	def, err := workflow.ParseDefinition(raw)
	if err != nil {
		return nil, err
	}
	return json.Marshal(def)
}

// nextWorkflowRun validates the timezone and computes the next scheduled run;
// manual-only workflows get the current time and are excluded from the
// scheduler scan by the empty schedule.
func nextWorkflowRun(schedule, timezone string, now time.Time) (time.Time, error) {
	if _, err := aitasks.LoadLocation(timezone); err != nil {
		return time.Time{}, err
	}
	if schedule == "" {
		return now, nil
	}
	return aitasks.NextRun(schedule, timezone, now)
}

func parseWorkflowID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("wfID"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workflow id"})
		return 0, false
	}
	return id, true
}

func parseWorkflowRunID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("runID"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workflow run id"})
		return 0, false
	}
	return id, true
}
