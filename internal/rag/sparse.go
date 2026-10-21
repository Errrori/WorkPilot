package rag

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/Errrori/workpilot/internal/store"
)

const (
	DefaultSparseCacheMaxChunks    = 20000
	DefaultSparseCacheMaxGroups    = 32
	DefaultSparseMaxChunksPerGroup = 20000
	sparseBM25K1                   = 1.5
	sparseBM25B                    = 0.75
)

var ErrSparseIndexTooLarge = errors.New("sparse index exceeds configured limit")

// SparseSource supplies the chunks a lexical index is built from.
type SparseSource interface {
	GroupChunkVersion(ctx context.Context, groupID string) (store.ChunkVersion, error)
	ListGroupChunks(ctx context.Context, groupID string, limit int) ([]store.GroupChunk, error)
}

// SparseSearcher runs BM25 over a group's chunks.
type SparseSearcher interface {
	Search(ctx context.Context, groupID, query string, limit int) ([]store.SparseHit, error)
}

type SparseConfig struct {
	MaxChunks         int
	MaxGroups         int
	MaxChunksPerGroup int
}

type sparsePosting struct {
	doc int32
	tf  int32
}

type sparseIndex struct {
	chunks    []store.GroupChunk
	lengths   []int
	avgLength float64
	docFreq   map[string]int
	postings  map[string][]sparsePosting
}

// tokenize mirrors the dependency-free character-bigram baseline used by the
// offline evaluation runner so dense/hybrid numbers stay comparable.
func tokenize(text string) []string {
	normalized := normalizeSparseText(text)
	runes := []rune(normalized)
	if len(runes) == 0 {
		return nil
	}
	if len(runes) == 1 {
		return []string{normalized}
	}
	tokens := make([]string, 0, len(runes)-1)
	for i := 0; i+1 < len(runes); i++ {
		tokens = append(tokens, string(runes[i:i+2]))
	}
	return tokens
}

func normalizeSparseText(text string) string {
	text = strings.ToLower(text)
	var b strings.Builder
	b.Grow(len(text))
	for _, r := range text {
		if !unicode.IsSpace(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func buildSparseIndex(chunks []store.GroupChunk) *sparseIndex {
	index := &sparseIndex{
		chunks:   chunks,
		lengths:  make([]int, len(chunks)),
		docFreq:  make(map[string]int),
		postings: make(map[string][]sparsePosting),
	}
	total := 0
	for i, chunk := range chunks {
		tokens := tokenize(chunk.Content)
		index.lengths[i] = len(tokens)
		total += len(tokens)
		if len(tokens) == 0 {
			continue
		}
		termFreq := make(map[string]int, len(tokens))
		for _, token := range tokens {
			termFreq[token]++
		}
		for token, tf := range termFreq {
			index.docFreq[token]++
			index.postings[token] = append(index.postings[token], sparsePosting{
				doc: int32(i),
				tf:  int32(tf),
			})
		}
	}
	if len(chunks) > 0 {
		index.avgLength = float64(total) / float64(len(chunks))
	}
	return index
}

func (i *sparseIndex) search(query string, limit int) []store.SparseHit {
	if i == nil || limit <= 0 || len(i.chunks) == 0 {
		return nil
	}
	queryTokens := uniqueTokens(tokenize(query))
	if len(queryTokens) == 0 {
		return nil
	}

	scores := make([]float64, len(i.chunks))
	total := float64(len(i.chunks))
	for _, token := range queryTokens {
		postings := i.postings[token]
		if len(postings) == 0 {
			continue
		}
		df := float64(i.docFreq[token])
		idf := math.Log(1 + (total-df+0.5)/(df+0.5))
		for _, posting := range postings {
			length := i.lengths[posting.doc]
			if length == 0 {
				continue
			}
			tf := float64(posting.tf)
			denominator := tf + sparseBM25K1*(1-sparseBM25B+sparseBM25B*float64(length)/i.avgLength)
			scores[posting.doc] += idf * tf * (sparseBM25K1 + 1) / denominator
		}
	}

	type scoredChunk struct {
		index int
		score float64
	}
	scored := make([]scoredChunk, 0, len(i.chunks))
	for index, score := range scores {
		if score > 0 {
			scored = append(scored, scoredChunk{index: index, score: score})
		}
	}
	sort.Slice(scored, func(a, b int) bool {
		if scored[a].score != scored[b].score {
			return scored[a].score > scored[b].score
		}
		return scored[a].index < scored[b].index
	})
	if len(scored) > limit {
		scored = scored[:limit]
	}

	hits := make([]store.SparseHit, 0, len(scored))
	for _, item := range scored {
		hits = append(hits, store.SparseHit{
			GroupChunk: i.chunks[item.index],
			Score:      item.score,
		})
	}
	return hits
}

func uniqueTokens(tokens []string) []string {
	if len(tokens) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(tokens))
	unique := make([]string, 0, len(tokens))
	for _, token := range tokens {
		if _, ok := seen[token]; ok {
			continue
		}
		seen[token] = struct{}{}
		unique = append(unique, token)
	}
	sort.Strings(unique)
	return unique
}

type sparseCacheEntry struct {
	version  store.ChunkVersion
	index    *sparseIndex
	lastUsed time.Time
}

// Bm25Searcher caches a per-group lexical index and rebuilds it when the
// group's chunk fingerprint changes.
type Bm25Searcher struct {
	source SparseSource
	cfg    SparseConfig

	mu          sync.Mutex
	entries     map[string]*sparseCacheEntry
	totalChunks int
}

var _ SparseSearcher = (*Bm25Searcher)(nil)

func NewBm25Searcher(source SparseSource, cfg SparseConfig) *Bm25Searcher {
	if cfg.MaxChunks <= 0 {
		cfg.MaxChunks = DefaultSparseCacheMaxChunks
	}
	if cfg.MaxGroups <= 0 {
		cfg.MaxGroups = DefaultSparseCacheMaxGroups
	}
	if cfg.MaxChunksPerGroup <= 0 {
		cfg.MaxChunksPerGroup = DefaultSparseMaxChunksPerGroup
	}
	if cfg.MaxChunksPerGroup > cfg.MaxChunks {
		cfg.MaxChunksPerGroup = cfg.MaxChunks
	}
	return &Bm25Searcher{
		source:  source,
		cfg:     cfg,
		entries: make(map[string]*sparseCacheEntry),
	}
}

func (s *Bm25Searcher) Search(ctx context.Context, groupID, query string, limit int) ([]store.SparseHit, error) {
	if s == nil || s.source == nil {
		return nil, errors.New("sparse searcher is not configured")
	}
	if groupID == "" {
		return nil, errors.New("group id is required for sparse search")
	}

	version, err := s.source.GroupChunkVersion(ctx, groupID)
	if err != nil {
		return nil, fmt.Errorf("load chunk version: %w", err)
	}

	s.mu.Lock()
	if entry := s.entries[groupID]; entry != nil && sameChunkVersion(entry.version, version) {
		entry.lastUsed = time.Now()
		index := entry.index
		s.mu.Unlock()
		return index.search(query, limit), nil
	}
	s.mu.Unlock()

	chunks, err := s.source.ListGroupChunks(ctx, groupID, s.cfg.MaxChunksPerGroup+1)
	if err != nil {
		return nil, fmt.Errorf("load group chunks: %w", err)
	}
	if len(chunks) > s.cfg.MaxChunksPerGroup {
		return nil, fmt.Errorf("%w: group has more than %d chunks", ErrSparseIndexTooLarge, s.cfg.MaxChunksPerGroup)
	}

	index := buildSparseIndex(chunks)
	s.store(groupID, version, index)
	return index.search(query, limit), nil
}

func (s *Bm25Searcher) store(groupID string, version store.ChunkVersion, index *sparseIndex) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()

	if previous := s.entries[groupID]; previous != nil {
		s.totalChunks -= len(previous.index.chunks)
	}
	s.entries[groupID] = &sparseCacheEntry{
		version:  version,
		index:    index,
		lastUsed: now,
	}
	s.totalChunks += len(index.chunks)

	for len(s.entries) > s.cfg.MaxGroups || s.totalChunks > s.cfg.MaxChunks {
		victim := ""
		var oldest time.Time
		for id, entry := range s.entries {
			if victim == "" || entry.lastUsed.Before(oldest) {
				victim = id
				oldest = entry.lastUsed
			}
		}
		if victim == "" {
			return
		}
		s.totalChunks -= len(s.entries[victim].index.chunks)
		delete(s.entries, victim)
	}
}

func sameChunkVersion(a, b store.ChunkVersion) bool {
	return a.Count == b.Count &&
		a.MaxID == b.MaxID &&
		a.MaxCreatedAt.UnixNano() == b.MaxCreatedAt.UnixNano()
}
