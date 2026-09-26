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

func NewRouter(pool *pgxpool.Pool, rdb *redis.Client, hub *ws.Hub, files *storage.Store, maxUploadMB int) *gin.Engine {
	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())

	r.GET("/healthz", healthz(pool, rdb))

	api := r.Group("/api")
	api.GET("/groups", listGroups(pool))
	api.GET("/groups/:id/messages", listMessages(pool))
	api.GET("/groups/:id/files", listFiles(pool))
	api.POST("/groups/:id/files", uploadFile(pool, files, hub, int64(maxUploadMB)<<20))
	api.GET("/groups/:id/files/:fileID/download", downloadFile(pool, files))
	api.DELETE("/groups/:id/files/:fileID", deleteFile(pool, files, hub))

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
