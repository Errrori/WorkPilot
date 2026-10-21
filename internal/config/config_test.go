package config

import "testing"

func TestLoadDefaults(t *testing.T) {
	t.Setenv("APP_PORT", "")
	cfg := Load()
	if cfg.Port != "8080" {
		t.Fatalf("Port = %q, want 8080", cfg.Port)
	}
	if cfg.DatabaseURL == "" {
		t.Fatal("DatabaseURL is empty")
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("APP_PORT", "9090")
	t.Setenv("REDIS_ADDR", "redis:6379")
	cfg := Load()
	if cfg.Port != "9090" {
		t.Fatalf("Port = %q, want 9090", cfg.Port)
	}
	if cfg.RedisAddr != "redis:6379" {
		t.Fatalf("RedisAddr = %q, want redis:6379", cfg.RedisAddr)
	}
}

func TestLoadFileDefaults(t *testing.T) {
	t.Setenv("FILE_STORAGE_DIR", "")
	t.Setenv("MAX_UPLOAD_MB", "")
	cfg := Load()
	if cfg.FileStorageDir != "./data/files" {
		t.Fatalf("FileStorageDir = %q, want ./data/files", cfg.FileStorageDir)
	}
	if cfg.MaxUploadMB != 50 {
		t.Fatalf("MaxUploadMB = %d, want 50", cfg.MaxUploadMB)
	}
}

func TestLoadFileOverrides(t *testing.T) {
	t.Setenv("FILE_STORAGE_DIR", "D:/tmp/files")
	t.Setenv("MAX_UPLOAD_MB", "10")
	cfg := Load()
	if cfg.FileStorageDir != "D:/tmp/files" {
		t.Fatalf("FileStorageDir = %q, want D:/tmp/files", cfg.FileStorageDir)
	}
	if cfg.MaxUploadMB != 10 {
		t.Fatalf("MaxUploadMB = %d, want 10", cfg.MaxUploadMB)
	}
}

func TestLoadRetrievalDefaults(t *testing.T) {
	t.Setenv("RETRIEVAL_MODE", "")
	t.Setenv("RETRIEVAL_CANDIDATES", "")
	cfg := Load()
	if cfg.RetrievalMode != "hybrid" {
		t.Fatalf("RetrievalMode = %q, want hybrid", cfg.RetrievalMode)
	}
	if cfg.RetrievalCandidates != 30 {
		t.Fatalf("RetrievalCandidates = %d, want 30", cfg.RetrievalCandidates)
	}
	if cfg.HybridRRFK != 60 {
		t.Fatalf("HybridRRFK = %v, want 60", cfg.HybridRRFK)
	}
}

func TestLoadRetrievalOverrides(t *testing.T) {
	t.Setenv("RETRIEVAL_MODE", "dense")
	t.Setenv("RETRIEVAL_CANDIDATES", "12")
	t.Setenv("HYBRID_SPARSE_WEIGHT", "0.5")
	cfg := Load()
	if cfg.RetrievalMode != "dense" {
		t.Fatalf("RetrievalMode = %q, want dense", cfg.RetrievalMode)
	}
	if cfg.RetrievalCandidates != 12 {
		t.Fatalf("RetrievalCandidates = %d, want 12", cfg.RetrievalCandidates)
	}
	if cfg.HybridSparseWeight != 0.5 {
		t.Fatalf("HybridSparseWeight = %v, want 0.5", cfg.HybridSparseWeight)
	}
}

func TestLoadMaxUploadInvalidFallsBack(t *testing.T) {
	for _, raw := range []string{"abc", "0", "-3"} {
		t.Setenv("MAX_UPLOAD_MB", raw)
		if cfg := Load(); cfg.MaxUploadMB != 50 {
			t.Fatalf("MaxUploadMB(%s) = %d, want fallback 50", raw, cfg.MaxUploadMB)
		}
	}
}
