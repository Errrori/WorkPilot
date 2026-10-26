package gitsync

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func pullJSON(number int, title, state, author string, updated time.Time, merged bool, draft bool) map[string]any {
	item := map[string]any{
		"number":     number,
		"title":      title,
		"state":      state,
		"user":       map[string]any{"login": author},
		"html_url":   fmt.Sprintf("https://example.com/pull/%d", number),
		"updated_at": updated.UTC().Format(time.RFC3339),
		"draft":      draft,
	}
	if merged {
		item["merged_at"] = updated.UTC().Format(time.RFC3339)
	} else {
		item["merged_at"] = nil
	}
	return item
}

func commitJSON(sha, message, author string, date time.Time) map[string]any {
	return map[string]any{
		"sha":      sha,
		"html_url": "https://example.com/commit/" + sha,
		"commit": map[string]any{
			"message":   message,
			"author":    map[string]any{"name": author, "date": date.UTC().Format(time.RFC3339)},
			"committer": map[string]any{"date": date.UTC().Format(time.RFC3339)},
		},
		"author": map[string]any{"login": author},
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}

func TestPullRequestsMapsItemsAndStopsAtWindow(t *testing.T) {
	now := time.Now()
	var gotAuth string
	var gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/app/pulls" {
			t.Errorf("path = %q", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		gotQuery = r.URL.RawQuery
		writeJSON(t, w, []map[string]any{
			pullJSON(3, "新增登录", "open", "alice", now.Add(-time.Hour), false, false),
			pullJSON(2, "修复导出", "closed", "bob", now.Add(-2*time.Hour), true, false),
			pullJSON(1, "远古 PR", "closed", "carol", now.Add(-30*24*time.Hour), false, false),
		})
	}))
	defer server.Close()

	client := NewClient(server.URL, "tok-123", time.Minute)
	items, err := client.PullRequests(t.Context(), "acme", "app", now.Add(-24*time.Hour), 0)
	if err != nil {
		t.Fatalf("PullRequests: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %#v", items)
	}
	if items[0].Kind != "pull_request" || items[0].ExternalID != "3" || items[0].State != "open" {
		t.Fatalf("first = %#v", items[0])
	}
	if items[0].Number == nil || *items[0].Number != 3 || items[0].Author != "alice" {
		t.Fatalf("first = %#v", items[0])
	}
	if items[1].State != "merged" {
		t.Fatalf("second state = %q", items[1].State)
	}
	if gotAuth != "Bearer tok-123" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if !strings.Contains(gotQuery, "state=all") || !strings.Contains(gotQuery, "sort=updated") {
		t.Fatalf("query = %q", gotQuery)
	}
}

func TestPullRequestsPaginatesUntilLimit(t *testing.T) {
	pages := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		pages[page]++
		if page == "1" {
			items := make([]map[string]any, 0, perPage)
			for i := 0; i < perPage; i++ {
				items = append(items, pullJSON(i+1, "pr", "open", "alice", time.Now().Add(-time.Minute), false, false))
			}
			writeJSON(t, w, items)
			return
		}
		writeJSON(t, w, []map[string]any{pullJSON(999, "last", "open", "bob", time.Now(), false, false)})
	}))
	defer server.Close()

	client := NewClient(server.URL, "", time.Minute)
	items, err := client.PullRequests(t.Context(), "acme", "app", time.Now().Add(-24*time.Hour), 0)
	if err != nil {
		t.Fatalf("PullRequests: %v", err)
	}
	if len(items) != perPage+1 {
		t.Fatalf("items = %d, want %d", len(items), perPage+1)
	}
	if pages["2"] != 1 {
		t.Fatalf("page calls = %#v", pages)
	}

	pages = map[string]int{}
	items, err = client.PullRequests(t.Context(), "acme", "app", time.Now().Add(-24*time.Hour), 5)
	if err != nil {
		t.Fatalf("PullRequests capped: %v", err)
	}
	if len(items) != 5 || pages["2"] != 0 {
		t.Fatalf("capped items = %d, pages = %#v", len(items), pages)
	}
}

func TestIssuesSkipsPullRequests(t *testing.T) {
	var gotSince string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSince = r.URL.Query().Get("since")
		issue := map[string]any{
			"number":     7,
			"title":      "登录偶发 500",
			"state":      "open",
			"user":       map[string]any{"login": "bob"},
			"html_url":   "https://example.com/issues/7",
			"updated_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		}
		pr := map[string]any{
			"number":       8,
			"title":        "同号 PR",
			"state":        "open",
			"pull_request": map[string]any{"url": "https://example.com/pulls/8"},
			"user":         map[string]any{"login": "alice"},
			"html_url":     "https://example.com/issues/8",
			"updated_at":   time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		}
		writeJSON(t, w, []map[string]any{issue, pr})
	}))
	defer server.Close()

	client := NewClient(server.URL, "", time.Minute)
	items, err := client.Issues(t.Context(), "acme", "app", time.Now().Add(-24*time.Hour), 0)
	if err != nil {
		t.Fatalf("Issues: %v", err)
	}
	if len(items) != 1 || items[0].ExternalID != "7" || items[0].State != "open" {
		t.Fatalf("items = %#v", items)
	}
	if gotSince == "" {
		t.Fatal("since query param missing")
	}
}

func TestCommitsUseFirstLineAndFallbackAuthor(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, []map[string]any{
			commitJSON("abcdef0123456789", "fix login redirect\n\nmore details", "alice", time.Now().Add(-time.Hour)),
		})
	}))
	defer server.Close()

	client := NewClient(server.URL, "", time.Minute)
	items, err := client.Commits(t.Context(), "acme", "app", time.Now().Add(-24*time.Hour), 0)
	if err != nil {
		t.Fatalf("Commits: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %#v", items)
	}
	if items[0].Kind != "commit" || items[0].ExternalID != "abcdef0123456789" {
		t.Fatalf("item = %#v", items[0])
	}
	if items[0].Title != "fix login redirect" || items[0].State != "" || items[0].Number != nil {
		t.Fatalf("item = %#v", items[0])
	}
	if items[0].Author != "alice" || items[0].UpdatedAt == nil {
		t.Fatalf("item = %#v", items[0])
	}
}

func TestTreeReturnsEntriesAndETag(t *testing.T) {
	var gotETag string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/docs/git/trees/main" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.URL.Query().Get("recursive") != "1" {
			t.Errorf("query = %q", r.URL.RawQuery)
		}
		gotETag = r.Header.Get("If-None-Match")
		w.Header().Set("ETag", `W/"tree-1"`)
		writeJSON(t, w, map[string]any{
			"truncated": false,
			"tree": []map[string]any{
				{"path": "docs/PRD.md", "type": "blob", "sha": "abc", "size": 128},
				{"path": "docs", "type": "tree", "sha": "def"},
			},
		})
	}))
	defer server.Close()

	client := NewClient(server.URL, "", time.Minute)
	tree, err := client.Tree(t.Context(), "acme", "docs", "main", `W/"old"`)
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	if gotETag != `W/"old"` {
		t.Fatalf("If-None-Match = %q", gotETag)
	}
	if tree.ETag != `W/"tree-1"` || tree.Truncated || tree.NotModified {
		t.Fatalf("tree = %#v", tree)
	}
	if len(tree.Entries) != 2 || tree.Entries[0].Path != "docs/PRD.md" || tree.Entries[0].Size != 128 {
		t.Fatalf("entries = %#v", tree.Entries)
	}
}

func TestTreeHandlesNotModified(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != `W/"same"` {
			t.Errorf("If-None-Match = %q", r.Header.Get("If-None-Match"))
		}
		w.WriteHeader(http.StatusNotModified)
	}))
	defer server.Close()

	tree, err := NewClient(server.URL, "", time.Minute).Tree(t.Context(), "acme", "docs", "main", `W/"same"`)
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	if !tree.NotModified || tree.ETag != `W/"same"` || len(tree.Entries) != 0 {
		t.Fatalf("tree = %#v", tree)
	}
}

func TestBlobDecodesBase64AndEnforcesLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/docs/git/blobs/sha1" {
			t.Errorf("path = %q", r.URL.Path)
		}
		writeJSON(t, w, map[string]any{
			"encoding": "base64",
			"content":  "aGVsbG8=",
		})
	}))
	defer server.Close()

	client := NewClient(server.URL, "", time.Minute)
	content, err := client.Blob(t.Context(), "acme", "docs", "sha1", 16)
	if err != nil || string(content) != "hello" {
		t.Fatalf("Blob = %q %v", content, err)
	}
	if _, err := client.Blob(t.Context(), "acme", "docs", "sha1", 4); !errors.Is(err, ErrBlobTooLarge) {
		t.Fatalf("Blob limit err = %v", err)
	}
}

func TestRepoReturnsDefaultBranch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/docs" {
			t.Errorf("path = %q", r.URL.Path)
		}
		writeJSON(t, w, map[string]any{"default_branch": "trunk"})
	}))
	defer server.Close()

	info, err := NewClient(server.URL, "", time.Minute).Repo(t.Context(), "acme", "docs")
	if err != nil || info.DefaultBranch != "trunk" {
		t.Fatalf("Repo = %#v %v", info, err)
	}
}

func TestClientErrorsIncludeStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	}))
	defer server.Close()

	client := NewClient(server.URL, "", time.Minute)
	_, err := client.PullRequests(t.Context(), "acme", "missing", time.Now(), 0)
	if err == nil || !strings.Contains(err.Error(), "status 404") || !strings.Contains(err.Error(), "Not Found") {
		t.Fatalf("err = %v", err)
	}
}
