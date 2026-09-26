package config

import (
	"os"
	"strconv"
)

type Config struct {
	Port           string
	DatabaseURL    string
	RedisAddr      string
	RedisPassword  string
	SidecarURL     string
	FileStorageDir string
	MaxUploadMB    int
}

func Load() Config {
	return Config{
		Port:           getenv("APP_PORT", "8080"),
		DatabaseURL:    getenv("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/app?sslmode=disable"),
		RedisAddr:      getenv("REDIS_ADDR", "localhost:6379"),
		RedisPassword:  os.Getenv("REDIS_PASSWORD"),
		SidecarURL:     getenv("SIDECAR_URL", "http://localhost:8000"),
		FileStorageDir: getenv("FILE_STORAGE_DIR", "./data/files"),
		MaxUploadMB:    getenvInt("MAX_UPLOAD_MB", 50),
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
