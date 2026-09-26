package main

import (
	"context"
	"log"

	"newproject/internal/config"
	"newproject/internal/store"
)

func main() {
	cfg := config.Load()
	if err := store.Migrate(context.Background(), cfg.DatabaseURL); err != nil {
		log.Fatalf("migrate: %v", err)
	}
	log.Println("migrations applied")
}
