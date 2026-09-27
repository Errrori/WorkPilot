package rag

import (
	"strings"
	"testing"
)

func TestChunkerEmpty(t *testing.T) {
	c := NewChunker(800, 100)
	if chunks := c.Split("   \n\n \n"); len(chunks) != 0 {
		t.Fatalf("chunks = %#v, want none", chunks)
	}
}

func TestChunkerPacksParagraphs(t *testing.T) {
	c := NewChunker(30, 0)
	text := strings.Repeat("a", 20) + "\n\n" + strings.Repeat("b", 20)

	chunks := c.Split(text)

	if len(chunks) != 2 {
		t.Fatalf("chunks = %d, want 2", len(chunks))
	}
	if chunks[0] != strings.Repeat("a", 20) || chunks[1] != strings.Repeat("b", 20) {
		t.Fatalf("unexpected chunks: %#v", chunks)
	}
}

func TestChunkerSplitsLongParagraph(t *testing.T) {
	c := NewChunker(10, 0)

	chunks := c.Split(strings.Repeat("x", 25))

	if len(chunks) != 3 {
		t.Fatalf("chunks = %d, want 3", len(chunks))
	}
	if len([]rune(chunks[0])) != 10 || len([]rune(chunks[1])) != 10 || len([]rune(chunks[2])) != 5 {
		t.Fatalf("unexpected chunk sizes: %#v", chunks)
	}
}

func TestChunkerOverlap(t *testing.T) {
	c := NewChunker(20, 5)
	first := strings.Repeat("a", 15)
	second := strings.Repeat("b", 15)

	chunks := c.Split(first + "\n\n" + second)

	if len(chunks) != 2 {
		t.Fatalf("chunks = %d, want 2", len(chunks))
	}
	wantPrefix := strings.Repeat("a", 5) + "\n\n" + second
	if chunks[1] != wantPrefix {
		t.Fatalf("second chunk = %q, want %q", chunks[1], wantPrefix)
	}
}

func TestChunkerKeepsCodeFenceTogether(t *testing.T) {
	c := NewChunker(15, 0)
	fence := "```\na\n\nb\n```"

	chunks := c.Split("intro\n\n" + fence + "\n\noutro")

	if len(chunks) != 3 {
		t.Fatalf("chunks = %d, want 3: %#v", len(chunks), chunks)
	}
	if chunks[1] != fence {
		t.Fatalf("fence chunk = %q, want %q", chunks[1], fence)
	}
}

func TestChunkerNormalizesCRLF(t *testing.T) {
	c := NewChunker(5, 0)

	chunks := c.Split("line1\r\n\r\nline2")

	if len(chunks) != 2 || chunks[0] != "line1" || chunks[1] != "line2" {
		t.Fatalf("unexpected chunks: %#v", chunks)
	}
}

func TestChunkerHeadingIsBlockBoundary(t *testing.T) {
	c := NewChunker(100, 0)

	chunks := c.Split("# Title\n\nbody text")

	if len(chunks) != 1 || chunks[0] != "# Title\n\nbody text" {
		t.Fatalf("unexpected chunks: %#v", chunks)
	}

	c = NewChunker(10, 0)
	chunks = c.Split("body text\n\n# Title")

	if len(chunks) != 2 || chunks[1] != "# Title" {
		t.Fatalf("unexpected chunks: %#v", chunks)
	}
}
