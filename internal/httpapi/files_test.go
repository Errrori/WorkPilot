package httpapi

import (
	"strings"
	"testing"
)

func TestSanitizeFileName(t *testing.T) {
	cases := map[string]string{
		"PRD.md":                         "PRD.md",
		"dir/sub/PRD.md":                 "PRD.md",
		`C:\Users\a\会议纪要.docx`:           "会议纪要.docx",
		"../../etc/passwd":               "passwd",
		"..":                             "",
		"":                               "",
		"  spaced name .txt  ":           "spaced name .txt",
		"bad\x00name\x1f.txt":            "badname.txt",
		strings.Repeat("a", 300) + ".md": strings.Repeat("a", maxFileNameRunes),
	}
	for in, want := range cases {
		if got := sanitizeFileName(in); got != want {
			t.Fatalf("sanitizeFileName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestContentDisposition(t *testing.T) {
	got := contentDisposition("需求 说明.md")
	want := `attachment; filename="__ __.md"; filename*=UTF-8''%E9%9C%80%E6%B1%82%20%E8%AF%B4%E6%98%8E.md`
	if got != want {
		t.Fatalf("contentDisposition = %q, want %q", got, want)
	}

	got = contentDisposition("PRD.md")
	if !strings.Contains(got, `filename="PRD.md"`) || !strings.Contains(got, "filename*=UTF-8''PRD.md") {
		t.Fatalf("contentDisposition(PRD.md) = %q", got)
	}
}
