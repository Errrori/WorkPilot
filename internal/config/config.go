package config

import (
	"os"
	"strconv"
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
}

func Load() Config {
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
