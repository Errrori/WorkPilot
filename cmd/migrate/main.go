package main

import (
	"context"
	"log"

	"github.com/Errrori/workpilot/internal/config"
	"github.com/Errrori/workpilot/internal/store"
)

func main() {
	cfg := config.Load()
	if err := store.Migrate(context.Background(), cfg.DatabaseURL); err != nil {
		log.Fatalf("migrate: %v", err)
	}
	log.Println("migrations applied")
}
