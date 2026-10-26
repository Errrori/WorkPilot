package rag

import (
	"fmt"
	"strings"
	"testing"
)

func TestCleanMarkdownRemovesNoise(t *testing.T) {
	input := "\ufeff# 项目计划\r\n\r\n<!-- 内部注释 -->\r\n\r\n正文\u200b内容   \r\n\r\n\r\n\r\n第 2 页 / 共 10 页\r\n\r\nPage 3 of 10\r\n\r\n- 4 -\r\n\r\n结尾。"
	want := "# 项目计划\n\n正文内容\n\n结尾。"

	if got := cleanMarkdown(input); got != want {
		t.Fatalf("cleaned = %q, want %q", got, want)
	}
}

func TestCleanMarkdownRemovesMultilineComment(t *testing.T) {
	input := "开头\n\n<!--\n隐藏内容\n-->\n\n结尾"
	want := "开头\n\n结尾"

	if got := cleanMarkdown(input); got != want {
		t.Fatalf("cleaned = %q, want %q", got, want)
	}
}

func TestCleanMarkdownKeepsCodeFenceContent(t *testing.T) {
	input := "```\n<!-- keep -->\n第 1 页\n\n```"

	if got := cleanMarkdown(input); got != input {
		t.Fatalf("cleaned = %q, want %q", got, input)
	}
}

func TestCleanMarkdownDropsOnlyPageNumbers(t *testing.T) {
	input := "第 1 页\n\nPage 2 of 3\n\n- 3 -\n\n正文"

	if got := cleanMarkdown(input); got != "正文" {
		t.Fatalf("cleaned = %q, want %q", got, "正文")
	}
}

func TestCleanMarkdownDropsRepeatedBoilerplate(t *testing.T) {
	var b strings.Builder
	b.WriteString("WorkPilot 内部资料\n\n")
	for i := 0; i < 31; i++ {
		fmt.Fprintf(&b, "正文第 %d 段\n\n", i)
	}
	b.WriteString("WorkPilot 内部资料\n\n中间正文\n\nWorkPilot 内部资料")

	got := cleanMarkdown(b.String())

	if count := strings.Count(got, "WorkPilot 内部资料"); count != 1 {
		t.Fatalf("boilerplate count = %d, want 1: %q", count, got)
	}
}
