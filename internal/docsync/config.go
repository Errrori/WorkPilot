package docsync

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/Errrori/workpilot/internal/store"
)

const (
	DefaultMaxFileMB = 10
	DefaultMaxFiles  = 200
	maxMaxFileMB     = 50
	maxMaxFiles      = 1000
	maxPatterns      = 50
	maxPatternRunes  = 200
)

var supportedExtensions = map[string]bool{
	"md": true, "markdown": true, "txt": true, "pdf": true, "docx": true,
}

var contentTypeByExtension = map[string]string{
	"md":       "text/markdown",
	"markdown": "text/markdown",
	"txt":      "text/plain",
	"pdf":      "application/pdf",
	"docx":     "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
}

var safeExcludePatterns = []string{
	"**/.env",
	"**/.env.*",
	"**/*.pem",
	"**/*.key",
	"**/*.p12",
	"**/*.pfx",
	"**/id_rsa*",
	"**/id_ed25519*",
	"**/secrets*",
	"**/credentials*",
	".git/**",
	"node_modules/**",
	"vendor/**",
}

// NormalizeConfig validates user config and fills defaults.
func NormalizeConfig(cfg store.DocSourceConfig) (store.DocSourceConfig, error) {
	cfg.Owner = strings.TrimSpace(cfg.Owner)
	cfg.Name = strings.TrimSpace(cfg.Name)
	cfg.Ref = strings.TrimSpace(cfg.Ref)
	if !validRepoPart(cfg.Owner) || !validRepoPart(cfg.Name) {
		return store.DocSourceConfig{}, fmt.Errorf("invalid owner or name")
	}
	if cfg.Ref != "" && !validRef(cfg.Ref) {
		return store.DocSourceConfig{}, fmt.Errorf("invalid ref")
	}

	include, err := normalizePatterns(cfg.Include)
	if err != nil {
		return store.DocSourceConfig{}, fmt.Errorf("include: %w", err)
	}
	exclude, err := normalizePatterns(cfg.Exclude)
	if err != nil {
		return store.DocSourceConfig{}, fmt.Errorf("exclude: %w", err)
	}
	cfg.Include = include
	cfg.Exclude = exclude

	exts, err := normalizeExtensions(cfg.Extensions)
	if err != nil {
		return store.DocSourceConfig{}, err
	}
	cfg.Extensions = exts

	if cfg.MaxFileMB == 0 {
		cfg.MaxFileMB = DefaultMaxFileMB
	}
	if cfg.MaxFileMB < 1 || cfg.MaxFileMB > maxMaxFileMB {
		return store.DocSourceConfig{}, fmt.Errorf("max_file_mb must be between 1 and %d", maxMaxFileMB)
	}
	if cfg.MaxFiles == 0 {
		cfg.MaxFiles = DefaultMaxFiles
	}
	if cfg.MaxFiles < 1 || cfg.MaxFiles > maxMaxFiles {
		return store.DocSourceConfig{}, fmt.Errorf("max_files must be between 1 and %d", maxMaxFiles)
	}
	return cfg, nil
}

func normalizePatterns(patterns []string) ([]string, error) {
	out := make([]string, 0, len(patterns))
	seen := map[string]bool{}
	for _, raw := range patterns {
		normalized := strings.TrimSpace(strings.ReplaceAll(raw, "\\", "/"))
		if strings.HasPrefix(normalized, "/") {
			return nil, fmt.Errorf("pattern %q must be a relative path", normalized)
		}
		pattern := strings.Trim(normalized, "/")
		if pattern == "" {
			continue
		}
		if len([]rune(pattern)) > maxPatternRunes {
			return nil, fmt.Errorf("pattern too long")
		}
		if strings.Contains(pattern, "..") {
			return nil, fmt.Errorf("pattern %q must not contain '..'", pattern)
		}
		if err := validateGlob(pattern); err != nil {
			return nil, fmt.Errorf("pattern %q: %w", pattern, err)
		}
		if !seen[pattern] {
			seen[pattern] = true
			out = append(out, pattern)
		}
		if len(out) > maxPatterns {
			return nil, fmt.Errorf("at most %d patterns are allowed", maxPatterns)
		}
	}
	sort.Strings(out)
	return out, nil
}

func normalizeExtensions(extensions []string) ([]string, error) {
	if len(extensions) == 0 {
		return []string{"docx", "markdown", "md", "pdf", "txt"}, nil
	}
	out := make([]string, 0, len(extensions))
	seen := map[string]bool{}
	for _, raw := range extensions {
		ext := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(raw), "."))
		if ext == "" {
			continue
		}
		if !supportedExtensions[ext] {
			return nil, fmt.Errorf("unsupported extension %q", ext)
		}
		if !seen[ext] {
			seen[ext] = true
			out = append(out, ext)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("extensions must not be empty")
	}
	sort.Strings(out)
	return out, nil
}

func validateGlob(pattern string) error {
	for _, segment := range strings.Split(pattern, "/") {
		if segment == "" {
			continue
		}
		if segment == "**" {
			continue
		}
		if _, err := path.Match(segment, "x"); err != nil {
			return err
		}
	}
	return nil
}

func validRepoPart(part string) bool {
	if part == "" || len([]rune(part)) > 100 {
		return false
	}
	for _, r := range part {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

func validRef(ref string) bool {
	if ref == "" || len([]rune(ref)) > 200 {
		return false
	}
	for _, r := range ref {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.', r == '/':
		default:
			return false
		}
	}
	return true
}

// MatchesPath reports whether a repository path is in scope for a source.
func MatchesPath(filename string, cfg store.DocSourceConfig) bool {
	filename = strings.TrimPrefix(strings.ReplaceAll(filename, "\\", "/"), "/")
	if filename == "" {
		return false
	}
	for _, pattern := range safeExcludePatterns {
		if matchGlob(pattern, filename) {
			return false
		}
	}
	for _, pattern := range cfg.Exclude {
		if matchGlob(pattern, filename) {
			return false
		}
	}
	ext := extensionOf(filename)
	if !containsString(cfg.Extensions, ext) {
		return false
	}
	if len(cfg.Include) == 0 {
		return true
	}
	for _, pattern := range cfg.Include {
		if matchGlob(pattern, filename) {
			return true
		}
	}
	return false
}

func matchGlob(pattern, name string) bool {
	pattern = strings.TrimPrefix(strings.ReplaceAll(pattern, "\\", "/"), "/")
	name = strings.TrimPrefix(strings.ReplaceAll(name, "\\", "/"), "/")
	if pattern == "" || name == "" {
		return false
	}
	if !strings.Contains(pattern, "/") {
		if ok, _ := path.Match(pattern, path.Base(name)); ok {
			return true
		}
	}
	return matchSegments(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegments(pattern, name []string) bool {
	if len(pattern) == 0 {
		return len(name) == 0
	}
	if pattern[0] == "**" {
		if matchSegments(pattern[1:], name) {
			return true
		}
		if len(name) > 0 {
			return matchSegments(pattern, name[1:])
		}
		return false
	}
	if len(name) == 0 {
		return false
	}
	ok, err := path.Match(pattern[0], name[0])
	if err != nil || !ok {
		return false
	}
	return matchSegments(pattern[1:], name[1:])
}

func extensionOf(filename string) string {
	ext := path.Ext(filename)
	return strings.ToLower(strings.TrimPrefix(ext, "."))
}

func contentTypeFor(filename string) string {
	if ct, ok := contentTypeByExtension[extensionOf(filename)]; ok {
		return ct
	}
	return "application/octet-stream"
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func sourceURL(cfg store.DocSourceConfig, ref, filename string) string {
	ref = strings.TrimPrefix(ref, "/")
	filename = strings.TrimPrefix(filename, "/")
	return fmt.Sprintf("https://github.com/%s/%s/blob/%s/%s", cfg.Owner, cfg.Name, ref, filename)
}
