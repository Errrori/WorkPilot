package config

import (
	"os"
	"strconv"

	"github.com/joho/godotenv"
)

type Config struct {
	Port                 string
	DatabaseURL          string
	RedisAddr            string
	RedisPassword        string
	SidecarURL           string
	FileStorageDir       string
	MaxUploadMB          int
	ParserTimeoutSeconds int

	EmbeddingProvider       string
	EmbeddingModel          string
	EmbeddingBaseURL        string
	EmbeddingDim            int
	EmbeddingTimeoutSeconds int
	ChunkSize               int
	ChunkOverlap            int
	IndexWorkers            int

	LLMProvider       string
	LLMModel          string
	LLMBaseURL        string
	LLMAPIKey         string
	LLMTimeoutSeconds int
	RetrievalTopK     int

	LLMPriceInputPerMTok  float64
	LLMPriceOutputPerMTok float64
	LLMPriceCurrency      string

	LogLevel  string
	LogFormat string

	TaskExtractMax    int
	TaskExtractBudget int

	RiskExtractMax    int
	RiskExtractBudget int

	AiTaskWorkers  int
	AiTaskTimezone string
	AiTaskBudget   int

	GitHubBaseURL          string
	GitHubToken            string
	GitSyncIntervalMinutes int
	GitSyncLookbackDays    int
	GitSyncWorkers         int
}

func Load() Config {
	// Optional .env in the working directory; real environment wins.
	_ = godotenv.Load()

	return Config{
		Port:                 getenv("APP_PORT", "8080"),
		DatabaseURL:          getenv("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/app?sslmode=disable"),
		RedisAddr:            getenv("REDIS_ADDR", "localhost:6379"),
		RedisPassword:        os.Getenv("REDIS_PASSWORD"),
		SidecarURL:           getenv("SIDECAR_URL", "http://localhost:8000"),
		FileStorageDir:       getenv("FILE_STORAGE_DIR", "./data/files"),
		MaxUploadMB:          getenvInt("MAX_UPLOAD_MB", 50),
		ParserTimeoutSeconds: getenvInt("PARSER_TIMEOUT_SECONDS", 120),

		EmbeddingProvider:       getenv("EMBEDDING_PROVIDER", "ollama"),
		EmbeddingModel:          getenv("EMBEDDING_MODEL", "bge-m3"),
		EmbeddingBaseURL:        getenv("EMBEDDING_BASE_URL", "http://localhost:11434"),
		EmbeddingDim:            getenvInt("EMBEDDING_DIM", 1024),
		EmbeddingTimeoutSeconds: getenvInt("EMBEDDING_TIMEOUT_SECONDS", 120),
		ChunkSize:               getenvInt("CHUNK_SIZE", 800),
		ChunkOverlap:            getenvIntAllowZero("CHUNK_OVERLAP", 100),
		IndexWorkers:            getenvInt("INDEX_WORKERS", 1),

		LLMProvider:       getenv("LLM_PROVIDER", "openai"),
		LLMModel:          getenv("LLM_MODEL", "qwen2.5:7b"),
		LLMBaseURL:        getenv("LLM_BASE_URL", "http://localhost:11434/v1"),
		LLMAPIKey:         getenv("LLM_API_KEY", "ollama"),
		LLMTimeoutSeconds: getenvInt("LLM_TIMEOUT_SECONDS", 120),
		RetrievalTopK:     getenvInt("RETRIEVAL_TOP_K", 6),

		LLMPriceInputPerMTok:  getenvFloat("LLM_PRICE_INPUT_PER_MTOK", 0),
		LLMPriceOutputPerMTok: getenvFloat("LLM_PRICE_OUTPUT_PER_MTOK", 0),
		LLMPriceCurrency:      getenv("LLM_PRICE_CURRENCY", "CNY"),

		LogLevel:  getenv("LOG_LEVEL", "info"),
		LogFormat: getenv("LOG_FORMAT", "text"),

		TaskExtractMax:    getenvInt("TASK_EXTRACT_MAX", 20),
		TaskExtractBudget: getenvInt("TASK_EXTRACT_BUDGET", 12000),

		RiskExtractMax:    getenvInt("RISK_EXTRACT_MAX", 10),
		RiskExtractBudget: getenvInt("RISK_EXTRACT_BUDGET", 12000),

		AiTaskWorkers:  getenvInt("AI_TASK_WORKERS", 1),
		AiTaskTimezone: getenv("AI_TASK_TIMEZONE", "Asia/Shanghai"),
		AiTaskBudget:   getenvInt("AI_TASK_BUDGET", 12000),

		GitHubBaseURL:          getenv("GITHUB_BASE_URL", "https://api.github.com"),
		GitHubToken:            os.Getenv("GITHUB_TOKEN"),
		GitSyncIntervalMinutes: getenvInt("GIT_SYNC_INTERVAL_MINUTES", 10),
		GitSyncLookbackDays:    getenvInt("GIT_SYNC_LOOKBACK_DAYS", 7),
		GitSyncWorkers:         getenvInt("GIT_SYNC_WORKERS", 1),
	}
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getenvInt(key string, fallback int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return fallback
	}
	return v
}

func getenvIntAllowZero(key string, fallback int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < 0 {
		return fallback
	}
	return v
}

func getenvFloat(key string, fallback float64) float64 {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v < 0 {
		return fallback
	}
	return v
}
