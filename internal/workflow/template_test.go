package workflow

import (
	"strings"
	"testing"
)

func TestTemplateRefs(t *testing.T) {
	refs := TemplateRefs("先看 {{r1}}，再看 {{ r1 }} 和 {{a2}}")
	if strings.Join(refs, ",") != "r1,a2" {
		t.Fatalf("refs = %v", refs)
	}
	if got := TemplateRefs("没有引用"); len(got) != 0 {
		t.Fatalf("refs = %v", got)
	}
}

func TestRenderTemplate(t *testing.T) {
	values := map[string]string{"r1": "资料文本", "a1": "回答文本"}
	got := RenderTemplate("资料：{{r1}}\n回答：{{ a1 }}\n未知：{{nope}}", values)
	want := "资料：资料文本\n回答：回答文本\n未知："
	if got != want {
		t.Fatalf("rendered = %q, want %q", got, want)
	}
	if plain := RenderTemplate("无引用", values); plain != "无引用" {
		t.Fatalf("plain = %q", plain)
	}
}
