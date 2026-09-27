package httpapi

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Errrori/workpilot/internal/store"
	"github.com/Errrori/workpilot/internal/tasks"
	"github.com/Errrori/workpilot/internal/ws"
)

const (
	maxTaskTitleRunes       = 200
	maxTaskDescriptionRunes = 2000
	maxTaskAssigneeRunes    = 64
	defaultTaskListLimit    = 100
	maxTaskListLimit        = 500
)

// TaskService extracts AI task suggestions for a group.
type TaskService interface {
	Extract(ctx context.Context, groupID, user string, fileID int64) ([]store.Task, error)
}

type createTaskRequest struct {
	User        string `json:"user"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Assignee    string `json:"assignee"`
	Priority    string `json:"priority"`
}

type updateTaskRequest struct {
	User        string  `json:"user"`
	Title       *string `json:"title"`
	Description *string `json:"description"`
	Assignee    *string `json:"assignee"`
	Status      *string `json:"status"`
	Priority    *string `json:"priority"`
}

type extractTasksRequest struct {
	User   string `json:"user"`
	FileID int64  `json:"file_id"`
}

func listTasks(pool *pgxpool.Pool) gin.HandlerFunc {
	return func(c *gin.Context) {
		groupID := c.Param("id")
		if !ensureGroup(c, pool, groupID) {
			return
		}
		status := strings.TrimSpace(c.Query("status"))
		if status != "" && !store.IsValidTaskStatus(status) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid status"})
			return
		}
		limit := defaultTaskListLimit
		if raw := c.Query("limit"); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= maxTaskListLimit {
				limit = n
			}
		}
		list, err := store.ListTasks(c.Request.Context(), pool, groupID, status, limit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if list == nil {
			list = []store.Task{}
		}
		c.JSON(http.StatusOK, gin.H{"tasks": list})
	}
}

func createTask(pool *pgxpool.Pool, hub *ws.Hub) gin.HandlerFunc {
	return func(c *gin.Context) {
		groupID := c.Param("id")
		if !ensureGroup(c, pool, groupID) {
			return
		}
		var req createTaskRequest
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
		if utf8.RuneCountInString(title) > maxTaskTitleRunes {
			c.JSON(http.StatusBadRequest, gin.H{"error": "title is too long"})
			return
		}
		description := strings.TrimSpace(req.Description)
		if utf8.RuneCountInString(description) > maxTaskDescriptionRunes {
			c.JSON(http.StatusBadRequest, gin.H{"error": "description is too long"})
			return
		}
		assignee := strings.TrimSpace(req.Assignee)
		if utf8.RuneCountInString(assignee) > maxTaskAssigneeRunes {
			c.JSON(http.StatusBadRequest, gin.H{"error": "assignee is too long"})
			return
		}
		priority := strings.ToLower(strings.TrimSpace(req.Priority))
		if priority == "" {
			priority = store.TaskPriorityMedium
		}
		if !store.IsValidTaskPriority(priority) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid priority"})
			return
		}

		task, err := store.InsertTask(c.Request.Context(), pool, store.TaskInsert{
			GroupID:     groupID,
			Title:       title,
			Description: description,
			Assignee:    assignee,
			Status:      store.TaskStatusTodo,
			Priority:    priority,
			Source:      store.TaskSourceManual,
			CreatedBy:   user,
		})
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		hub.BroadcastTask(ws.EventTaskCreated, &task)
		c.JSON(http.StatusCreated, gin.H{"task": task})
	}
}

func updateTask(pool *pgxpool.Pool, hub *ws.Hub) gin.HandlerFunc {
	return func(c *gin.Context) {
		taskID, ok := parseTaskID(c)
		if !ok {
			return
		}
		groupID := c.Param("id")
		if !ensureGroup(c, pool, groupID) {
			return
		}
		var req updateTaskRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json body"})
			return
		}
		user := strings.TrimSpace(req.User)
		if user == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "user is required"})
			return
		}
		if req.Title == nil && req.Description == nil && req.Assignee == nil && req.Status == nil && req.Priority == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "nothing to update"})
			return
		}

		upd := store.TaskUpdate{Actor: user}
		if req.Title != nil {
			title := strings.TrimSpace(*req.Title)
			if title == "" {
				c.JSON(http.StatusBadRequest, gin.H{"error": "title cannot be empty"})
				return
			}
			if utf8.RuneCountInString(title) > maxTaskTitleRunes {
				c.JSON(http.StatusBadRequest, gin.H{"error": "title is too long"})
				return
			}
			upd.Title = &title
		}
		if req.Description != nil {
			description := strings.TrimSpace(*req.Description)
			if utf8.RuneCountInString(description) > maxTaskDescriptionRunes {
				c.JSON(http.StatusBadRequest, gin.H{"error": "description is too long"})
				return
			}
			upd.Description = &description
		}
		if req.Assignee != nil {
			assignee := strings.TrimSpace(*req.Assignee)
			if utf8.RuneCountInString(assignee) > maxTaskAssigneeRunes {
				c.JSON(http.StatusBadRequest, gin.H{"error": "assignee is too long"})
				return
			}
			upd.Assignee = &assignee
		}
		if req.Status != nil {
			status := strings.TrimSpace(*req.Status)
			if !store.IsValidTaskStatus(status) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid status"})
				return
			}
			upd.Status = &status
		}
		if req.Priority != nil {
			priority := strings.ToLower(strings.TrimSpace(*req.Priority))
			if !store.IsValidTaskPriority(priority) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid priority"})
				return
			}
			upd.Priority = &priority
		}

		task, err := store.UpdateTask(c.Request.Context(), pool, groupID, taskID, upd)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		hub.BroadcastTask(ws.EventTaskUpdated, &task)
		c.JSON(http.StatusOK, gin.H{"task": task})
	}
}

func deleteTask(pool *pgxpool.Pool, hub *ws.Hub) gin.HandlerFunc {
	return func(c *gin.Context) {
		taskID, ok := parseTaskID(c)
		if !ok {
			return
		}
		groupID := c.Param("id")
		if !ensureGroup(c, pool, groupID) {
			return
		}
		task, err := store.DeleteTask(c.Request.Context(), pool, groupID, taskID)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		hub.BroadcastTask(ws.EventTaskDeleted, &task)
		c.JSON(http.StatusOK, gin.H{"deleted": true, "task": task})
	}
}

func extractTasks(pool *pgxpool.Pool, hub *ws.Hub, svc TaskService) gin.HandlerFunc {
	return func(c *gin.Context) {
		groupID := c.Param("id")
		if !ensureGroup(c, pool, groupID) {
			return
		}
		runExtractTasks(c, pool, hub, svc, groupID)
	}
}

// runExtractTasks validates the request and stores AI suggested tasks.
func runExtractTasks(c *gin.Context, pool *pgxpool.Pool, hub *ws.Hub, svc TaskService, groupID string) {
	if svc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "task service is unavailable"})
		return
	}
	var req extractTasksRequest
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
	if errors.Is(err, tasks.ErrNoMaterial) {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	if err != nil {
		log.Printf("extract tasks for group %s: %v", groupID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	for i := range created {
		hub.BroadcastTask(ws.EventTaskSuggested, &created[i])
	}
	c.JSON(http.StatusOK, gin.H{"tasks": created, "count": len(created)})
}

func parseTaskID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("taskID"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid task id"})
		return 0, false
	}
	return id, true
}
