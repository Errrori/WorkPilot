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
	"github.com/Errrori/workpilot/internal/parser"
	"github.com/Errrori/workpilot/internal/qa"
	"github.com/Errrori/workpilot/internal/rag"
	"github.com/Errrori/workpilot/internal/storage"
	"github.com/Errrori/workpilot/internal/store"
	"github.com/Errrori/workpilot/internal/tasks"
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

	embedder, err := rag.NewEmbedder(ctx, rag.EmbedderConfig{
		Provider: cfg.EmbeddingProvider,
		BaseURL:  cfg.EmbeddingBaseURL,
		Model:    cfg.EmbeddingModel,
		Timeout:  time.Duration(cfg.EmbeddingTimeoutSeconds) * time.Second,
	})
	if err != nil {
		log.Fatalf("init embedder: %v", err)
	}
	log.Printf("embedding via %s %s (%d dims) at %s", cfg.EmbeddingProvider, cfg.EmbeddingModel, cfg.EmbeddingDim, cfg.EmbeddingBaseURL)

	ragStore := store.RagStore{Pool: pool}
	indexWorker := rag.NewWorker(ctx, rag.WorkerConfig{
		Chunker:  rag.NewChunker(cfg.ChunkSize, cfg.ChunkOverlap),
		Embedder: embedder,
		Dim:      cfg.EmbeddingDim,
		Indexer:  rag.NewPgVectorIndexer(pool),
		Store:    ragStore,
		Notifier: hub,
		Workers:  cfg.IndexWorkers,
	})

	chatModel, err := qa.NewChatModel(ctx, qa.ModelConfig{
		Provider: cfg.LLMProvider,
		BaseURL:  cfg.LLMBaseURL,
		Model:    cfg.LLMModel,
		APIKey:   cfg.LLMAPIKey,
		Timeout:  time.Duration(cfg.LLMTimeoutSeconds) * time.Second,
	})
	if err != nil {
		log.Fatalf("init chat model: %v", err)
	}
	log.Printf("llm via %s %s at %s", cfg.LLMProvider, cfg.LLMModel, cfg.LLMBaseURL)

	qaService := qa.NewService(qa.Config{
		Retriever: rag.NewPgVectorRetriever(embedder, ragStore, cfg.RetrievalTopK),
		ChatModel: chatModel,
		Store:     store.QaStore{Pool: pool},
		Timeout:   time.Duration(cfg.LLMTimeoutSeconds) * time.Second,
	})

	taskService := tasks.NewService(tasks.Config{
		ChatModel:      chatModel,
		Store:          store.TaskStore{Pool: pool},
		MaxSuggestions: cfg.TaskExtractMax,
		CharBudget:     cfg.TaskExtractBudget,
		Timeout:        time.Duration(cfg.LLMTimeoutSeconds) * time.Second,
	})

	parseClient := parser.NewClient(cfg.SidecarURL, time.Duration(cfg.ParserTimeoutSeconds)*time.Second)
	parseWorker := parser.NewWorker(ctx, parseClient, store.FileParseStore{Pool: pool}, files, hub, parser.DefaultWorkers)
	parseWorker.SetOnParsed(indexWorker.Enqueue)

	interrupted, err := store.ResetParsingFiles(ctx, pool)
	if err != nil {
		log.Printf("reset interrupted parses: %v", err)
	}
	for _, f := range interrupted {
		parseWorker.Enqueue(f)
	}
	if len(interrupted) > 0 {
		log.Printf("re-enqueued %d interrupted parses", len(interrupted))
	}

	reset, err := store.ResetIndexingFiles(ctx, pool)
	if err != nil {
		log.Printf("reset interrupted indexes: %v", err)
	}
	pendingIndex, err := store.ListPendingIndexFiles(ctx, pool)
	if err != nil {
		log.Printf("list pending indexes: %v", err)
	}
	for _, f := range pendingIndex {
		indexWorker.Enqueue(f)
	}
	if reset > 0 || len(pendingIndex) > 0 {
		log.Printf("enqueued %d files for indexing (reset %d interrupted)", len(pendingIndex), reset)
	}

	router := httpapi.NewRouter(pool, rdb, hub, files, parseWorker, indexWorker, qaService, taskService, cfg.MaxUploadMB)

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
