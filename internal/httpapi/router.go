package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/Errrori/workpilot/internal/storage"
	"github.com/Errrori/workpilot/internal/ws"
)

func NewRouter(pool *pgxpool.Pool, rdb *redis.Client, hub *ws.Hub, files *storage.Store, parseEnqueuer ParseEnqueuer, indexEnqueuer IndexEnqueuer, asker AskService, searcher SearchService, taskSvc TaskService, riskSvc RiskService, aiRunner AiTaskRunner, repoRunner RepoRunner, aiTaskTimezone string, maxUploadMB int, pricing UsagePricing) *gin.Engine {
	r := gin.New()
	r.Use(accessLog(), gin.Recovery())

	r.GET("/healthz", healthz(pool, rdb))

	api := r.Group("/api")
	api.GET("/groups", listGroups(pool))
	api.POST("/groups", createGroup(pool))
	api.GET("/groups/:id/messages", listMessages(pool))
	api.GET("/groups/:id/files", listFiles(pool))
	api.POST("/groups/:id/files", uploadFile(pool, files, hub, parseEnqueuer, int64(maxUploadMB)<<20))
	api.GET("/groups/:id/files/:fileID/download", downloadFile(pool, files))
	api.GET("/groups/:id/files/:fileID/content", getFileContent(pool))
	api.GET("/groups/:id/files/:fileID/chunks", listFileChunks(pool))
	api.POST("/groups/:id/files/:fileID/parse", retryFileParse(pool, parseEnqueuer))
	api.POST("/groups/:id/files/:fileID/index", reindexFile(pool, indexEnqueuer))
	api.DELETE("/groups/:id/files/:fileID", deleteFile(pool, files, hub))
	api.POST("/groups/:id/ask", askGroup(pool, asker))
	api.POST("/groups/:id/search", searchGroup(pool, searcher))
	api.GET("/groups/:id/usage", usageGroup(pool, pricing))
	api.GET("/groups/:id/tasks", listTasks(pool))
	api.POST("/groups/:id/tasks", createTask(pool, hub))
	api.POST("/groups/:id/tasks/extract", extractTasks(pool, hub, taskSvc))
	api.PATCH("/groups/:id/tasks/:taskID", updateTask(pool, hub))
	api.DELETE("/groups/:id/tasks/:taskID", deleteTask(pool, hub))
	api.GET("/groups/:id/risks", listRisks(pool))
	api.POST("/groups/:id/risks", createRisk(pool, hub))
	api.POST("/groups/:id/risks/extract", extractRisks(pool, hub, riskSvc))
	api.PATCH("/groups/:id/risks/:riskID", updateRisk(pool, hub))
	api.DELETE("/groups/:id/risks/:riskID", deleteRisk(pool, hub))
	api.GET("/groups/:id/ai-tasks", listAiTasks(pool))
	api.POST("/groups/:id/ai-tasks", createAiTask(pool, hub, aiTaskTimezone))
	api.PATCH("/groups/:id/ai-tasks/:taskID", updateAiTask(pool, hub))
	api.DELETE("/groups/:id/ai-tasks/:taskID", deleteAiTask(pool, hub))
	api.POST("/groups/:id/ai-tasks/:taskID/run", runAiTask(pool, aiRunner))
	api.GET("/groups/:id/reports", listReports(pool))
	api.GET("/groups/:id/reports/:reportID", getReport(pool))
	api.DELETE("/groups/:id/reports/:reportID", deleteReport(pool))
	api.GET("/groups/:id/repos", listRepos(pool))
	api.POST("/groups/:id/repos", createRepo(pool, hub))
	api.PATCH("/groups/:id/repos/:repoID", updateRepo(pool, hub))
	api.DELETE("/groups/:id/repos/:repoID", deleteRepo(pool, hub))
	api.POST("/groups/:id/repos/:repoID/sync", syncRepo(pool, repoRunner))
	api.GET("/groups/:id/repos/:repoID/items", listRepoItems(pool))

	r.GET("/ws", gin.WrapF(hub.ServeWS))

	return r
}

func healthz(pool *pgxpool.Pool, rdb *redis.Client) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		defer cancel()

		body := gin.H{"status": "ok", "postgres": "ok", "redis": "ok"}
		code := http.StatusOK
		if err := pool.Ping(ctx); err != nil {
			body["postgres"] = err.Error()
			body["status"] = "degraded"
			code = http.StatusServiceUnavailable
		}
		if err := rdb.Ping(ctx).Err(); err != nil {
			body["redis"] = err.Error()
			body["status"] = "degraded"
			code = http.StatusServiceUnavailable
		}
		c.JSON(code, body)
	}
}
