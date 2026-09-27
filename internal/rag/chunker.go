// Package rag builds and stores embeddings for parsed group documents.
package rag

import "strings"

const (
	DefaultChunkSize    = 800
	DefaultChunkOverlap = 100

	paragraphSeparator = "\n\n"
)

// Chunker splits Markdown into overlapping chunks sized in runes.
type Chunker struct {
	size    int
	overlap int
}

func NewChunker(size, overlap int) *Chunker {
	if size <= 0 {
		size = DefaultChunkSize
	}
	if overlap < 0 {
		overlap = 0
	}
	if overlap >= size {
		overlap = size / 4
	}
	return &Chunker{size: size, overlap: overlap}
}

// Split packs Markdown blocks into chunks and prepends the tail of the
// previous chunk to each following chunk for context.
func (c *Chunker) Split(markdown string) []string {
	raw := c.pack(splitBlocks(markdown))
	if c.overlap == 0 || len(raw) < 2 {
		return raw
	}
	out := make([]string, len(raw))
	out[0] = raw[0]
	for i := 1; i < len(raw); i++ {
		if tail := tailRunes(raw[i-1], c.overlap); tail != "" {
			out[i] = tail + paragraphSeparator + raw[i]
		} else {
			out[i] = raw[i]
		}
	}
	return out
}

func (c *Chunker) pack(blocks []string) []string {
	var chunks []string
	var current []string
	currentLen := 0

	flush := func() {
		if len(current) == 0 {
			return
		}
		chunks = append(chunks, strings.Join(current, paragraphSeparator))
		current = nil
		currentLen = 0
	}

	for _, block := range blocks {
		runes := []rune(block)
		if len(runes) > c.size {
			flush()
			for start := 0; start < len(runes); start += c.size {
				end := min(start+c.size, len(runes))
				chunks = append(chunks, string(runes[start:end]))
			}
			continue
		}
		if currentLen > 0 && currentLen+len(paragraphSeparator)+len(runes) > c.size {
			flush()
		}
		current = append(current, block)
		currentLen += len(runes)
		if len(current) > 1 {
			currentLen += len(paragraphSeparator)
		}
	}
	flush()
	return chunks
}

// splitBlocks normalizes line endings and splits Markdown into paragraphs,
// keeping fenced code blocks intact and treating headings as block boundaries.
func splitBlocks(markdown string) []string {
	markdown = strings.ReplaceAll(markdown, "\r\n", "\n")
	markdown = strings.ReplaceAll(markdown, "\r", "\n")

	var blocks []string
	var current []string
	inFence := false

	flush := func() {
		if len(current) == 0 {
			return
		}
		blocks = append(blocks, strings.Join(current, "\n"))
		current = nil
	}

	for _, line := range strings.Split(markdown, "\n") {
		trimmed := strings.TrimSpace(line)
		if isFence(trimmed) {
			current = append(current, trimmed)
			inFence = !inFence
			continue
		}
		if inFence {
			current = append(current, line)
			continue
		}
		if trimmed == "" {
			flush()
			continue
		}
		if isHeading(trimmed) {
			flush()
			blocks = append(blocks, trimmed)
			continue
		}
		current = append(current, trimmed)
	}
	flush()
	return blocks
}

func isFence(line string) bool {
	return strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~")
}

func isHeading(line string) bool {
	hashes := 0
	for _, r := range line {
		if r != '#' {
			break
		}
		hashes++
	}
	return hashes >= 1 && hashes <= 6 && (len(line) == hashes || line[hashes] == ' ')
}

func tailRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[len(runes)-n:])
}
