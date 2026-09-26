package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"newproject/internal/ws"
)

func NewRouter(pool *pgxpool.Pool, rdb *redis.Client, hub *ws.Hub) *gin.Engine {
	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())

	r.GET("/healthz", healthz(pool, rdb))

	api := r.Group("/api")
	api.GET("/groups", listGroups(pool))
	api.GET("/groups/:id/messages", listMessages(pool))

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
