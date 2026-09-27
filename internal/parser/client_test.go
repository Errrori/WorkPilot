package parser

import (
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseSuccess(t *testing.T) {
	var gotFile string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/parse" {
			t.Errorf("path = %q, want /parse", r.URL.Path)
		}
		mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != "multipart/form-data" {
			t.Errorf("content type = %q err = %v", r.Header.Get("Content-Type"), err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		part, err := multipart.NewReader(r.Body, params["boundary"]).NextPart()
		if err != nil {
			t.Errorf("next part: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if part.FormName() != "file" {
			t.Errorf("form name = %q, want file", part.FormName())
		}
		if part.FileName() != "PRD.md" {
			t.Errorf("file name = %q, want PRD.md", part.FileName())
		}
		data, _ := io.ReadAll(part)
		gotFile = string(data)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"filename":"PRD.md","markdown":"# Hello"}`))
	}))
	defer srv.Close()

	md, err := NewClient(srv.URL, 5*time.Second).Parse(context.Background(), "PRD.md", strings.NewReader("source"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if md != "# Hello" {
		t.Fatalf("markdown = %q, want %q", md, "# Hello")
	}
	if gotFile != "source" {
		t.Fatalf("uploaded file = %q, want source", gotFile)
	}
}

func TestParseUnsupported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnsupportedMediaType)
		_, _ = w.Write([]byte(`{"detail":"unsupported extension: .png"}`))
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL, 5*time.Second).Parse(context.Background(), "logo.png", strings.NewReader("x"))
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
	if !strings.Contains(err.Error(), ".png") {
		t.Fatalf("err = %v, want detail in message", err)
	}
}

func TestParseFailed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"detail":"no text extracted"}`))
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL, 5*time.Second).Parse(context.Background(), "scan.pdf", strings.NewReader("x"))
	if !errors.Is(err, ErrParseFailed) {
		t.Fatalf("err = %v, want ErrParseFailed", err)
	}
	if errors.Is(err, ErrUnsupported) {
		t.Fatal("ErrParseFailed must not match ErrUnsupported")
	}
}

func TestParseServerErrorIsNotClassified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL, 5*time.Second).Parse(context.Background(), "PRD.md", strings.NewReader("x"))
	if err == nil {
		t.Fatal("expected error")
	}
	if errors.Is(err, ErrUnsupported) || errors.Is(err, ErrParseFailed) {
		t.Fatalf("err = %v, want unclassified", err)
	}
}

func TestParseEmptyMarkdown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"filename":"a.md","markdown":"  "}`))
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL, 5*time.Second).Parse(context.Background(), "a.md", strings.NewReader("x"))
	if !errors.Is(err, ErrParseFailed) {
		t.Fatalf("err = %v, want ErrParseFailed", err)
	}
}

func TestParseTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte(`{"markdown":"late"}`))
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL, 50*time.Millisecond).Parse(context.Background(), "PRD.md", strings.NewReader("x"))
	if err == nil {
		t.Fatal("expected timeout error")
	}
}
