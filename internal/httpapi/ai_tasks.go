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

	"github.com/Errrori/workpilot/internal/aitasks"
	"github.com/Errrori/workpilot/internal/store"
	"github.com/Errrori/workpilot/internal/ws"
)

const (
	maxAiTaskNameRunes     = 200
	maxAiTaskPromptRunes   = 2000
	maxAiTaskScheduleRunes = 100
	maxAiTaskTimezoneRunes = 64
	defaultAiTaskListLimit = 100
	maxAiTaskListLimit     = 500
	defaultReportListLimit = 20
	maxReportListLimit     = 100
)

// AiTaskRunner queues a run for a custom AI task.
type AiTaskRunner interface {
	EnqueueRun(ctx context.Context, taskID int64, trigger, actor string) error
}

type createAiTaskRequest struct {
	User         string   `json:"user"`
	Name         string   `json:"name"`
	Prompt       string   `json:"prompt"`
	Schedule     string   `json:"schedule"`
	Timezone     string   `json:"timezone"`
	Sources      []string `json:"sources"`
	LookbackDays int      `json:"lookback_days"`
	Enabled      *bool    `json:"enabled"`
}

type updateAiTaskRequest struct {
	User         string    `json:"user"`
	Name         *string   `json:"name"`
	Prompt       *string   `json:"prompt"`
	Schedule     *string   `json:"schedule"`
	Timezone     *string   `json:"timezone"`
	Sources      *[]string `json:"sources"`
	LookbackDays *int      `json:"lookback_days"`
	Enabled      *bool     `json:"enabled"`
}

type runAiTaskRequest struct {
	User string `json:"user"`
}

func listAiTasks(pool *pgxpool.Pool) gin.HandlerFunc {
	return func(c *gin.Context) {
		groupID := c.Param("id")
		if !ensureGroup(c, pool, groupID) {
			return
		}
		limit := defaultAiTaskListLimit
		if raw := c.Query("limit"); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= maxAiTaskListLimit {
				limit = n
			}
		}
		list, err := store.ListAiTasks(c.Request.Context(), pool, groupID, limit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if list == nil {
			list = []store.AiTask{}
		}
		c.JSON(http.StatusOK, gin.H{"ai_tasks": list})
	}
}

func createAiTask(pool *pgxpool.Pool, hub *ws.Hub, defaultTimezone string) gin.HandlerFunc {
	return func(c *gin.Context) {
		groupID := c.Param("id")
		if !ensureGroup(c, pool, groupID) {
			return
		}
		var req createAiTaskRequest
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
		if utf8.RuneCountInString(name) > maxAiTaskNameRunes {
			c.JSON(http.StatusBadRequest, gin.H{"error": "name is too long"})
			return
		}
		prompt := strings.TrimSpace(req.Prompt)
		if utf8.RuneCountInString(prompt) > maxAiTaskPromptRunes {
			c.JSON(http.StatusBadRequest, gin.H{"error": "prompt is too long"})
			return
		}
		schedule := strings.TrimSpace(req.Schedule)
		if utf8.RuneCountInString(schedule) > maxAiTaskScheduleRunes {
			c.JSON(http.StatusBadRequest, gin.H{"error": "schedule is too long"})
			return
		}
		timezone := strings.TrimSpace(req.Timezone)
		if timezone == "" {
			timezone = defaultTimezone
		}
		if utf8.RuneCountInString(timezone) > maxAiTaskTimezoneRunes {
			c.JSON(http.StatusBadRequest, gin.H{"error": "timezone is too long"})
			return
		}
		nextRun, err := aitasks.NextRun(schedule, timezone, time.Now())
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		sources, ok := normalizeAiTaskSources(req.Sources)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid sources"})
			return
		}
		lookback := req.LookbackDays
		if lookback == 0 {
			lookback = aitasks.DefaultLookbackDays
		}
		if lookback < 1 || lookback > 90 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "lookback_days must be between 1 and 90"})
			return
		}
		enabled := true
		if req.Enabled != nil {
			enabled = *req.Enabled
		}

		task, err := store.InsertAiTask(c.Request.Context(), pool, store.AiTaskInsert{
			GroupID:      groupID,
			Name:         name,
			Prompt:       prompt,
			Schedule:     schedule,
			Timezone:     timezone,
			Sources:      sources,
			LookbackDays: lookback,
			Enabled:      enabled,
			CreatedBy:    user,
			NextRunAt:    nextRun,
		})
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		hub.BroadcastAiTask(ws.EventAiTaskCreated, &task)
		c.JSON(http.StatusCreated, gin.H{"ai_task": task})
	}
}

func updateAiTask(pool *pgxpool.Pool, hub *ws.Hub) gin.HandlerFunc {
	return func(c *gin.Context) {
		taskID, ok := parseAiTaskID(c)
		if !ok {
			return
		}
		groupID := c.Param("id")
		if !ensureGroup(c, pool, groupID) {
			return
		}
		var req updateAiTaskRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json body"})
			return
		}
		user := strings.TrimSpace(req.User)
		if user == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "user is required"})
			return
		}
		if req.Name == nil && req.Prompt == nil && req.Schedule == nil && req.Timezone == nil && req.Sources == nil && req.LookbackDays == nil && req.Enabled == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "nothing to update"})
			return
		}

		current, err := store.GetAiTask(c.Request.Context(), pool, groupID, taskID)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "ai task not found"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		upd := store.AiTaskUpdate{}
		if req.Name != nil {
			name := strings.TrimSpace(*req.Name)
			if name == "" {
				c.JSON(http.StatusBadRequest, gin.H{"error": "name cannot be empty"})
				return
			}
			if utf8.RuneCountInString(name) > maxAiTaskNameRunes {
				c.JSON(http.StatusBadRequest, gin.H{"error": "name is too long"})
				return
			}
			upd.Name = &name
		}
		if req.Prompt != nil {
			prompt := strings.TrimSpace(*req.Prompt)
			if utf8.RuneCountInString(prompt) > maxAiTaskPromptRunes {
				c.JSON(http.StatusBadRequest, gin.H{"error": "prompt is too long"})
				return
			}
			upd.Prompt = &prompt
		}
		schedule := current.Schedule
		if req.Schedule != nil {
			schedule = strings.TrimSpace(*req.Schedule)
			if utf8.RuneCountInString(schedule) > maxAiTaskScheduleRunes {
				c.JSON(http.StatusBadRequest, gin.H{"error": "schedule is too long"})
				return
			}
			upd.Schedule = &schedule
		}
		timezone := current.Timezone
		if req.Timezone != nil {
			timezone = strings.TrimSpace(*req.Timezone)
			if utf8.RuneCountInString(timezone) > maxAiTaskTimezoneRunes {
				c.JSON(http.StatusBadRequest, gin.H{"error": "timezone is too long"})
				return
			}
			upd.Timezone = &timezone
		}
		if req.Schedule != nil || req.Timezone != nil {
			nextRun, err := aitasks.NextRun(schedule, timezone, time.Now())
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			upd.NextRunAt = &nextRun
		}
		if req.Sources != nil {
			sources, ok := normalizeAiTaskSources(*req.Sources)
			if !ok {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid sources"})
				return
			}
			upd.Sources = &sources
		}
		if req.LookbackDays != nil {
			lookback := *req.LookbackDays
			if lookback < 1 || lookback > 90 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "lookback_days must be between 1 and 90"})
				return
			}
			upd.LookbackDays = &lookback
		}
		if req.Enabled != nil {
			upd.Enabled = req.Enabled
		}

		task, err := store.UpdateAiTask(c.Request.Context(), pool, groupID, taskID, upd)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "ai task not found"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		hub.BroadcastAiTask(ws.EventAiTaskUpdated, &task)
		c.JSON(http.StatusOK, gin.H{"ai_task": task})
	}
}

func deleteAiTask(pool *pgxpool.Pool, hub *ws.Hub) gin.HandlerFunc {
	return func(c *gin.Context) {
		taskID, ok := parseAiTaskID(c)
		if !ok {
			return
		}
		groupID := c.Param("id")
		if !ensureGroup(c, pool, groupID) {
			return
		}
		task, err := store.DeleteAiTask(c.Request.Context(), pool, groupID, taskID)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "ai task not found"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		hub.BroadcastAiTask(ws.EventAiTaskDeleted, &task)
		c.JSON(http.StatusOK, gin.H{"deleted": true, "ai_task": task})
	}
}

func runAiTask(pool *pgxpool.Pool, runner AiTaskRunner) gin.HandlerFunc {
	return func(c *gin.Context) {
		taskID, ok := parseAiTaskID(c)
		if !ok {
			return
		}
		groupID := c.Param("id")
		if !ensureGroup(c, pool, groupID) {
			return
		}
		var req runAiTaskRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json body"})
			return
		}
		user := strings.TrimSpace(req.User)
		if user == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "user is required"})
			return
		}
		if _, err := store.GetAiTask(c.Request.Context(), pool, groupID, taskID); errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "ai task not found"})
			return
		} else if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if runner == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "ai task runner is unavailable"})
			return
		}
		if err := runner.EnqueueRun(c.Request.Context(), taskID, store.ReportTriggerManual, user); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusAccepted, gin.H{"queued": true})
	}
}

func listReports(pool *pgxpool.Pool) gin.HandlerFunc {
	return func(c *gin.Context) {
		groupID := c.Param("id")
		if !ensureGroup(c, pool, groupID) {
			return
		}
		var aiTaskID int64
		if raw := c.Query("ai_task_id"); raw != "" {
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || n <= 0 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid ai_task_id"})
				return
			}
			aiTaskID = n
		}
		limit := defaultReportListLimit
		if raw := c.Query("limit"); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= maxReportListLimit {
				limit = n
			}
		}
		reports, err := store.ListReports(c.Request.Context(), pool, groupID, aiTaskID, limit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if reports == nil {
			reports = []store.Report{}
		}
		c.JSON(http.StatusOK, gin.H{"reports": reports})
	}
}

func getReport(pool *pgxpool.Pool) gin.HandlerFunc {
	return func(c *gin.Context) {
		groupID := c.Param("id")
		if !ensureGroup(c, pool, groupID) {
			return
		}
		reportID, ok := parseReportID(c)
		if !ok {
			return
		}
		report, err := store.GetReport(c.Request.Context(), pool, groupID, reportID)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "report not found"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"report": report})
	}
}

func deleteReport(pool *pgxpool.Pool) gin.HandlerFunc {
	return func(c *gin.Context) {
		groupID := c.Param("id")
		if !ensureGroup(c, pool, groupID) {
			return
		}
		reportID, ok := parseReportID(c)
		if !ok {
			return
		}
		report, err := store.DeleteReport(c.Request.Context(), pool, groupID, reportID)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "report not found"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"deleted": true, "report": report})
	}
}

// normalizeAiTaskSources validates and deduplicates the requested sources,
// defaulting to all sources when none are given.
func normalizeAiTaskSources(sources []string) ([]string, bool) {
	if len(sources) == 0 {
		return []string{
			store.AiTaskSourceMessages,
			store.AiTaskSourceTasks,
			store.AiTaskSourceRisks,
			store.AiTaskSourceFiles,
		}, true
	}
	seen := make(map[string]bool, len(sources))
	out := make([]string, 0, len(sources))
	for _, source := range sources {
		source = strings.ToLower(strings.TrimSpace(source))
		if !store.IsValidAiTaskSource(source) {
			return nil, false
		}
		if seen[source] {
			continue
		}
		seen[source] = true
		out = append(out, source)
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

func parseAiTaskID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("taskID"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid ai task id"})
		return 0, false
	}
	return id, true
}

func parseReportID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("reportID"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid report id"})
		return 0, false
	}
	return id, true
}
