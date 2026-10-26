package docsync

import (
	"strings"
	"testing"

	"github.com/Errrori/workpilot/internal/store"
)

func TestNormalizeConfigDefaultsAndValidation(t *testing.T) {
	cfg, err := NormalizeConfig(store.DocSourceConfig{Owner: "acme", Name: "docs"})
	if err != nil {
		t.Fatalf("NormalizeConfig: %v", err)
	}
	if cfg.MaxFileMB != DefaultMaxFileMB || cfg.MaxFiles != DefaultMaxFiles {
		t.Fatalf("limits = %d %d", cfg.MaxFileMB, cfg.MaxFiles)
	}
	if len(cfg.Extensions) != 5 || cfg.Extensions[0] != "docx" {
		t.Fatalf("extensions = %#v", cfg.Extensions)
	}

	invalid := []store.DocSourceConfig{
		{Owner: "/bad", Name: "docs"},
		{Owner: "acme", Name: "docs", Ref: "bad ref"},
		{Owner: "acme", Name: "docs", Include: []string{"/abs/**"}},
		{Owner: "acme", Name: "docs", Include: []string{"../secret"}},
		{Owner: "acme", Name: "docs", Extensions: []string{"go"}},
		{Owner: "acme", Name: "docs", MaxFileMB: 100},
		{Owner: "acme", Name: "docs", MaxFiles: 5000},
	}
	for _, cfg := range invalid {
		if _, err := NormalizeConfig(cfg); err == nil {
			t.Fatalf("NormalizeConfig(%#v) unexpectedly ok", cfg)
		}
	}
}

func TestMatchesPath(t *testing.T) {
	cfg, err := NormalizeConfig(store.DocSourceConfig{
		Owner:   "acme",
		Name:    "docs",
		Include: []string{"docs/**", "*.md"},
		Exclude: []string{"docs/private/**"},
	})
	if err != nil {
		t.Fatalf("NormalizeConfig: %v", err)
	}
	cases := map[string]bool{
		"docs/PRD.md":           true,
		"docs/design/api.txt":   true,
		"README.md":             true,
		"docs/private/plan.md":  false,
		".env":                  false,
		"node_modules/a.md":     false,
		"docs/archive/old.pdf":  true,
		"docs/archive/data.csv": false,
	}
	for name, want := range cases {
		if got := MatchesPath(name, cfg); got != want {
			t.Fatalf("MatchesPath(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestMatchesPathWithoutIncludeAllowsSupportedExtensions(t *testing.T) {
	cfg, err := NormalizeConfig(store.DocSourceConfig{Owner: "acme", Name: "docs"})
	if err != nil {
		t.Fatalf("NormalizeConfig: %v", err)
	}
	if !MatchesPath("any/where/plan.docx", cfg) {
		t.Fatal("supported docx should match")
	}
	if MatchesPath("any/where/main.go", cfg) {
		t.Fatal("unsupported extension should not match")
	}
}

func TestValidateGlobRejectsBrokenPattern(t *testing.T) {
	if err := validateGlob("docs/["); err == nil {
		t.Fatal("expected invalid glob")
	}
	if err := validateGlob("docs/**/*.md"); err != nil {
		t.Fatalf("valid glob rejected: %v", err)
	}
	if !strings.Contains(sourceURL(store.DocSourceConfig{Owner: "acme", Name: "docs"}, "main", "docs/PRD.md"), "blob/main/docs/PRD.md") {
		t.Fatal("source URL did not include path")
	}
}
