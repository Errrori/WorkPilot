package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Errrori/workpilot/internal/config"
	"github.com/Errrori/workpilot/internal/httpapi"
	"github.com/Errrori/workpilot/internal/storage"
	"github.com/Errrori/workpilot/internal/store"
	"github.com/Errrori/workpilot/internal/ws"
)

func main() {
	cfg := config.Load()
	ctx := context.Background()

	pool, err := store.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("connect postgres: %v", err)
	}
	defer pool.Close()

	rdb := redis.NewClient(&redis.Options{Addr: cfg.RedisAddr, Password: cfg.RedisPassword})
	defer rdb.Close()

	hub := ws.NewHub(ctx, pool)

	files, err := storage.New(cfg.FileStorageDir)
	if err != nil {
		log.Fatalf("init file storage: %v", err)
	}
	log.Printf("file storage at %s (max upload %d MB)", files.Root(), cfg.MaxUploadMB)

	router := httpapi.NewRouter(pool, rdb, hub, files, cfg.MaxUploadMB)

	srv := &http.Server{Addr: ":" + cfg.Port, Handler: router}

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()
	log.Printf("server listening on :%s", cfg.Port)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}
