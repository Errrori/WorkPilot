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

	TaskExtractMax    int
	TaskExtractBudget int

	RiskExtractMax    int
	RiskExtractBudget int
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

		TaskExtractMax:    getenvInt("TASK_EXTRACT_MAX", 20),
		TaskExtractBudget: getenvInt("TASK_EXTRACT_BUDGET", 12000),

		RiskExtractMax:    getenvInt("RISK_EXTRACT_MAX", 10),
		RiskExtractBudget: getenvInt("RISK_EXTRACT_BUDGET", 12000),
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
