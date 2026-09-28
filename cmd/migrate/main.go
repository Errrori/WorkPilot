package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/Errrori/workpilot/internal/config"
	"github.com/Errrori/workpilot/internal/logging"
	"github.com/Errrori/workpilot/internal/store"
)

func main() {
	cfg := config.Load()
	logging.Setup(cfg.LogLevel, cfg.LogFormat)
	if err := store.Migrate(context.Background(), cfg.DatabaseURL); err != nil {
		slog.Error("migrate", "error", err)
		os.Exit(1)
	}
	slog.Info("migrations applied")
}
