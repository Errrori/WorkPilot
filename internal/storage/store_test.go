package storage

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestSaveOpenRemoveRoundtrip(t *testing.T) {
	s := newTestStore(t)
	const groupID = "00000000-0000-0000-0000-000000000001"
	content := "hello 世界"

	rel, size, err := s.Save(groupID, "需求 PRD.md", strings.NewReader(content), 1024)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if size != int64(len(content)) {
		t.Fatalf("size = %d, want %d", size, len(content))
	}
	if !strings.HasPrefix(rel, groupID+"/") {
		t.Fatalf("rel = %q, want prefix %q", rel, groupID+"/")
	}
	if !strings.HasSuffix(rel, ".md") {
		t.Fatalf("rel = %q, want .md suffix", rel)
	}

	f, err := s.Open(rel)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, err := io.ReadAll(f)
	_ = f.Close()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != content {
		t.Fatalf("content = %q, want %q", got, content)
	}

	if err := s.Remove(rel); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := s.Open(rel); !errors.Is(err, ErrNoStorage) {
		t.Fatalf("Open after Remove err = %v, want ErrNoStorage", err)
	}
	if err := s.Remove(rel); err != nil {
		t.Fatalf("Remove missing: %v", err)
	}
}

func TestSaveTooLarge(t *testing.T) {
	s := newTestStore(t)
	rel, _, err := s.Save("g", "big.txt", strings.NewReader("0123456789"), 5)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if rel != "" {
		t.Fatalf("rel = %q, want empty", rel)
	}
	entries, err := os.ReadDir(filepath.Join(s.Root(), "g"))
	if err != nil {
		t.Fatalf("read group dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("partial file left on disk: %v", entries)
	}
}

func TestSaveExactlyAtLimit(t *testing.T) {
	s := newTestStore(t)
	const content = "0123456789"
	if _, size, err := s.Save("g", "a.txt", strings.NewReader(content), int64(len(content))); err != nil || size != int64(len(content)) {
		t.Fatalf("Save size=%d err=%v, want %d", size, err, len(content))
	}
}

func TestPathRejectsTraversal(t *testing.T) {
	s := newTestStore(t)
	for _, rel := range []string{"", "../evil", "..\\evil", "a/../../evil", "../../../Windows/System32/x"} {
		if _, err := s.Path(rel); !errors.Is(err, ErrBadPath) {
			t.Fatalf("Path(%q) err = %v, want ErrBadPath", rel, err)
		}
		if _, err := s.Open(rel); !errors.Is(err, ErrBadPath) {
			t.Fatalf("Open(%q) err = %v, want ErrBadPath", rel, err)
		}
		if err := s.Remove(rel); !errors.Is(err, ErrBadPath) {
			t.Fatalf("Remove(%q) err = %v, want ErrBadPath", rel, err)
		}
	}
}

func TestRandomNameIsUnique(t *testing.T) {
	seen := make(map[string]struct{}, 64)
	for i := 0; i < 64; i++ {
		name, err := randomName("a.txt")
		if err != nil {
			t.Fatalf("randomName: %v", err)
		}
		if _, ok := seen[name]; ok {
			t.Fatalf("duplicate name %q", name)
		}
		seen[name] = struct{}{}
	}
}

func TestSafeExt(t *testing.T) {
	cases := map[string]string{
		"a.PDF":         ".pdf",
		"a.b.md":        ".md",
		"archive.tar":   ".tar",
		"noext":         "",
		".hidden":       "",
		"a.verylongext": "",
		"a.p n g":       "",
		"a.中文":          "",
	}
	for in, want := range cases {
		if got := safeExt(in); got != want {
			t.Fatalf("safeExt(%q) = %q, want %q", in, got, want)
		}
	}
}
