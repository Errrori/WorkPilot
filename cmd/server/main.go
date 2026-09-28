package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Errrori/workpilot/internal/aitasks"
	"github.com/Errrori/workpilot/internal/config"
	"github.com/Errrori/workpilot/internal/gitsync"
	"github.com/Errrori/workpilot/internal/httpapi"
	"github.com/Errrori/workpilot/internal/llmtrack"
	"github.com/Errrori/workpilot/internal/logging"
	"github.com/Errrori/workpilot/internal/parser"
	"github.com/Errrori/workpilot/internal/qa"
	"github.com/Errrori/workpilot/internal/rag"
	"github.com/Errrori/workpilot/internal/risks"
	"github.com/Errrori/workpilot/internal/storage"
	"github.com/Errrori/workpilot/internal/store"
	"github.com/Errrori/workpilot/internal/tasks"
	"github.com/Errrori/workpilot/internal/ws"
	"github.com/Errrori/workpilot/webui"
)

func main() {
	cfg := config.Load()
	logging.Setup(cfg.LogLevel, cfg.LogFormat)
	ctx := context.Background()

	pool, err := store.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		fatal("connect postgres", "error", err)
	}
	defer pool.Close()

	rdb := redis.NewClient(&redis.Options{Addr: cfg.RedisAddr, Password: cfg.RedisPassword})
	defer rdb.Close()

	hub := ws.NewHub(ctx, pool)

	files, err := storage.New(cfg.FileStorageDir)
	if err != nil {
		fatal("init file storage", "error", err)
	}
	slog.Info("file storage ready", "root", files.Root(), "max_upload_mb", cfg.MaxUploadMB)

	embedder, err := rag.NewEmbedder(ctx, rag.EmbedderConfig{
		Provider: cfg.EmbeddingProvider,
		BaseURL:  cfg.EmbeddingBaseURL,
		Model:    cfg.EmbeddingModel,
		Timeout:  time.Duration(cfg.EmbeddingTimeoutSeconds) * time.Second,
	})
	if err != nil {
		fatal("init embedder", "error", err)
	}
	slog.Info("embedding ready",
		"provider", cfg.EmbeddingProvider,
		"model", cfg.EmbeddingModel,
		"dims", cfg.EmbeddingDim,
		"base_url", cfg.EmbeddingBaseURL,
	)

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
		fatal("init chat model", "error", err)
	}
	slog.Info("llm ready", "provider", cfg.LLMProvider, "model", cfg.LLMModel, "base_url", cfg.LLMBaseURL)
	trackedModel := llmtrack.Wrap(chatModel, store.UsageStore{Pool: pool}, cfg.LLMProvider, cfg.LLMModel)

	qaService := qa.NewService(qa.Config{
		Retriever: rag.NewPgVectorRetriever(embedder, ragStore, cfg.RetrievalTopK),
		ChatModel: trackedModel,
		Store:     store.QaStore{Pool: pool},
		Timeout:   time.Duration(cfg.LLMTimeoutSeconds) * time.Second,
	})

	taskService := tasks.NewService(tasks.Config{
		ChatModel:      trackedModel,
		Store:          store.TaskStore{Pool: pool},
		MaxSuggestions: cfg.TaskExtractMax,
		CharBudget:     cfg.TaskExtractBudget,
		Timeout:        time.Duration(cfg.LLMTimeoutSeconds) * time.Second,
	})

	riskService := risks.NewService(risks.Config{
		ChatModel:  trackedModel,
		Store:      store.RiskStore{Pool: pool},
		MaxRisks:   cfg.RiskExtractMax,
		CharBudget: cfg.RiskExtractBudget,
		Timeout:    time.Duration(cfg.LLMTimeoutSeconds) * time.Second,
	})

	aiTaskService := aitasks.NewService(aitasks.Config{
		ChatModel:  trackedModel,
		Store:      store.AiTaskStore{Pool: pool},
		CharBudget: cfg.AiTaskBudget,
		Timeout:    time.Duration(cfg.LLMTimeoutSeconds) * time.Second,
	})

	var aiRunner httpapi.AiTaskRunner
	aiScheduler, err := aitasks.NewScheduler(aitasks.SchedulerConfig{
		RedisAddr:     cfg.RedisAddr,
		RedisPassword: cfg.RedisPassword,
		Concurrency:   cfg.AiTaskWorkers,
		Store:         store.AiTaskStore{Pool: pool},
		Service:       aiTaskService,
		Notifier:      hub,
	})
	if err != nil {
		slog.Error("init ai task scheduler", "error", err)
	} else if err := aiScheduler.Start(); err != nil {
		slog.Error("start ai task scheduler", "error", err, "note", "scheduled reports disabled")
	} else {
		aiRunner = aiScheduler
	}

	gitClient := gitsync.NewClient(cfg.GitHubBaseURL, cfg.GitHubToken, 30*time.Second)
	var repoRunner httpapi.RepoRunner
	gitScheduler, err := gitsync.NewScheduler(gitsync.SchedulerConfig{
		RedisAddr:     cfg.RedisAddr,
		RedisPassword: cfg.RedisPassword,
		Concurrency:   cfg.GitSyncWorkers,
		Interval:      time.Duration(cfg.GitSyncIntervalMinutes) * time.Minute,
		LookbackDays:  cfg.GitSyncLookbackDays,
		Client:        gitClient,
		Store:         store.RepoStore{Pool: pool},
		Notifier:      hub,
	})
	if err != nil {
		slog.Error("init git sync scheduler", "error", err)
	} else if err := gitScheduler.Start(); err != nil {
		slog.Error("start git sync scheduler", "error", err, "note", "scheduled repo syncs disabled")
	} else {
		repoRunner = gitScheduler
	}
	slog.Info("github sync ready",
		"base_url", cfg.GitHubBaseURL,
		"token_configured", cfg.GitHubToken != "",
		"interval_minutes", cfg.GitSyncIntervalMinutes,
		"lookback_days", cfg.GitSyncLookbackDays,
	)

	parseClient := parser.NewClient(cfg.SidecarURL, time.Duration(cfg.ParserTimeoutSeconds)*time.Second)
	parseWorker := parser.NewWorker(ctx, parseClient, store.FileParseStore{Pool: pool}, files, hub, parser.DefaultWorkers)
	parseWorker.SetOnParsed(indexWorker.Enqueue)

	interrupted, err := store.ResetParsingFiles(ctx, pool)
	if err != nil {
		slog.Warn("reset interrupted parses", "error", err)
	}
	for _, f := range interrupted {
		parseWorker.Enqueue(f)
	}
	if len(interrupted) > 0 {
		slog.Info("re-enqueued interrupted parses", "count", len(interrupted))
	}

	reset, err := store.ResetIndexingFiles(ctx, pool)
	if err != nil {
		slog.Warn("reset interrupted indexes", "error", err)
	}
	pendingIndex, err := store.ListPendingIndexFiles(ctx, pool)
	if err != nil {
		slog.Warn("list pending indexes", "error", err)
	}
	for _, f := range pendingIndex {
		indexWorker.Enqueue(f)
	}
	if reset > 0 || len(pendingIndex) > 0 {
		slog.Info("enqueued files for indexing", "count", len(pendingIndex), "reset", reset)
	}

	router := httpapi.NewRouter(
		pool, rdb, hub, files, parseWorker, indexWorker,
		qaService, qaService, taskService, riskService, aiRunner, repoRunner,
		cfg.AiTaskTimezone, cfg.MaxUploadMB,
		httpapi.UsagePricing{
			InputPerMTok:  cfg.LLMPriceInputPerMTok,
			OutputPerMTok: cfg.LLMPriceOutputPerMTok,
			Currency:      cfg.LLMPriceCurrency,
		},
	)
	webui.Mount(router)

	srv := &http.Server{Addr: ":" + cfg.Port, Handler: router}

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fatal("listen", "error", err)
		}
	}()
	slog.Info("server listening", "addr", ":"+cfg.Port)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("shutdown", "error", err)
	}
	if aiScheduler != nil {
		aiScheduler.Shutdown()
	}
	if gitScheduler != nil {
		gitScheduler.Shutdown()
	}
}

func fatal(msg string, args ...any) {
	slog.Error(msg, args...)
	os.Exit(1)
}
