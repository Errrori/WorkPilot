package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/Errrori/workpilot/internal/agent"
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
	"github.com/Errrori/workpilot/internal/workflow"
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
		Provider:  cfg.EmbeddingProvider,
		BaseURL:   cfg.EmbeddingBaseURL,
		Model:     cfg.EmbeddingModel,
		Timeout:   time.Duration(cfg.EmbeddingTimeoutSeconds) * time.Second,
		KeepAlive: time.Duration(cfg.EmbeddingKeepAliveSeconds) * time.Second,
	})
	if err != nil {
		fatal("init embedder", "error", err)
	}
	slog.Info("embedding ready",
		"provider", cfg.EmbeddingProvider,
		"model", cfg.EmbeddingModel,
		"dims", cfg.EmbeddingDim,
		"base_url", cfg.EmbeddingBaseURL,
		"keep_alive_s", cfg.EmbeddingKeepAliveSeconds,
	)

	if cfg.EmbeddingWarmupEnabled {
		warmupCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.EmbeddingTimeoutSeconds)*time.Second)
		elapsed, err := rag.Warmup(warmupCtx, embedder, cfg.EmbeddingDim)
		cancel()
		if err != nil {
			slog.Warn("embedding warmup failed; first request may be slow", "error", err, "elapsed_ms", elapsed.Milliseconds())
		} else {
			slog.Info("embedding warmed up", "elapsed_ms", elapsed.Milliseconds())
		}
	}

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

	retriever := rag.NewPgVectorRetriever(embedder, ragStore, cfg.RetrievalTopK)
	qaService := qa.NewService(qa.Config{
		Retriever: retriever,
		ChatModel: trackedModel,
		Store:     store.QAStore{Pool: pool},
		Timeout:   time.Duration(cfg.LLMTimeoutSeconds) * time.Second,
		MinScore:  cfg.RetrievalMinScore,
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

	workflowStore := store.WorkflowStore{Pool: pool}
	workflowService := workflow.NewService(workflow.ServiceConfig{
		Store: workflowStore,
		Engine: workflow.NewEngine(workflow.EngineConfig{
			Handlers: workflow.NewHandlers(workflow.Deps{
				Retriever: retriever,
				ChatModel: trackedModel,
				Extractor: taskService,
				Material:  store.TaskStore{Pool: pool},
				Reports:   aiTaskService,
				Notifier:  hub,
				MinScore:  cfg.RetrievalMinScore,
			}),
			Store:       workflowStore,
			NodeTimeout: time.Duration(cfg.WorkflowNodeTimeoutSeconds) * time.Second,
		}),
		Notifier: hub,
		NextRun:  aitasks.NextRun,
	})
	var workflowRunner httpapi.WorkflowRunner
	workflowScheduler, err := workflow.NewScheduler(workflow.SchedulerConfig{
		RedisAddr:     cfg.RedisAddr,
		RedisPassword: cfg.RedisPassword,
		Concurrency:   cfg.WorkflowWorkers,
		Store:         workflowStore,
		Service:       workflowService,
		Notifier:      hub,
	})
	if err != nil {
		slog.Error("init workflow scheduler", "error", err)
	} else if err := workflowScheduler.Start(); err != nil {
		slog.Error("start workflow scheduler", "error", err, "note", "scheduled workflows disabled")
	} else {
		workflowRunner = workflowScheduler
	}
	slog.Info("workflow engine ready", "workers", cfg.WorkflowWorkers, "node_timeout_s", cfg.WorkflowNodeTimeoutSeconds)

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

	var agentScheduler *agent.Scheduler
	agentService, err := agent.NewService(agent.Config{
		ChatModel:       trackedModel,
		Extractor:       taskService,
		Retriever:       retriever,
		Store:           store.AgentStore{Pool: pool},
		FileSink:        agentFileSink{files: files, pool: pool, enqueuer: parseWorker, maxBytes: int64(cfg.MaxUploadMB) << 20},
		Notifier:        hub,
		ContextMessages: cfg.AgentContextMessages,
		MaxSteps:        cfg.AgentMaxSteps,
		CharBudget:      cfg.AgentBudget,
		Timeout:         time.Duration(cfg.LLMTimeoutSeconds) * time.Second,
		MinScore:        cfg.RetrievalMinScore,
	})
	if err != nil {
		slog.Error("init agent service", "error", err)
	} else if agentScheduler, err = agent.NewScheduler(agent.SchedulerConfig{
		RedisAddr:     cfg.RedisAddr,
		RedisPassword: cfg.RedisPassword,
		Concurrency:   cfg.AgentWorkers,
		Service:       agentService,
	}); err != nil {
		slog.Error("init agent scheduler", "error", err)
	} else if err := agentScheduler.Start(); err != nil {
		slog.Error("start agent scheduler", "error", err, "note", "@WorkPilot disabled")
		agentScheduler = nil
	} else {
		hub.SetAgentTrigger(agentScheduler)
		slog.Info("agent ready", "mention", agent.Mention, "workers", cfg.AgentWorkers, "max_steps", cfg.AgentMaxSteps)
	}

	router := httpapi.NewRouter(
		pool, rdb, hub, files, parseWorker, indexWorker,
		qaService, qaService, taskService, riskService, aiRunner, repoRunner, workflowRunner,
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
	if workflowScheduler != nil {
		workflowScheduler.Shutdown()
	}
	if gitScheduler != nil {
		gitScheduler.Shutdown()
	}
	if agentScheduler != nil {
		agentScheduler.Shutdown()
	}
}

// agentFileSink persists archived chat text as a group file and enqueues it
// for parsing (indexing follows automatically).
type agentFileSink struct {
	files    *storage.Store
	pool     *pgxpool.Pool
	enqueuer interface{ Enqueue(store.File) }
	maxBytes int64
}

func (s agentFileSink) Save(ctx context.Context, groupID, uploader, fileName, content string) (store.File, error) {
	rel, size, err := s.files.Save(groupID, fileName, strings.NewReader(content), s.maxBytes)
	if err != nil {
		return store.File{}, err
	}
	rec, err := store.InsertFile(ctx, s.pool, groupID, uploader, fileName, "text/markdown", rel, size)
	if err != nil {
		_ = s.files.Remove(rel)
		return store.File{}, err
	}
	s.enqueuer.Enqueue(rec)
	return rec, nil
}

func fatal(msg string, args ...any) {
	slog.Error(msg, args...)
	os.Exit(1)
}
