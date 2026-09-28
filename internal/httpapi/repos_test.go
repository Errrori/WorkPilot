package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/Errrori/workpilot/internal/store"
)

type fakeRepoRunner struct {
	repoID int64
	calls  int
}

func (r *fakeRepoRunner) EnqueueSync(_ context.Context, repoID int64) error {
	r.calls++
	r.repoID = repoID
	return nil
}

func doRunSync(runner RepoRunner, body string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/groups/:id/repos/:repoID/sync", func(c *gin.Context) {
		repoID, ok := parseRepoID(c)
		if !ok {
			return
		}
		runSyncRepo(c, nil, runner, c.Param("id"), repoID)
	})
	req := httptest.NewRequest(http.MethodPost, "/groups/group-1/repos/7/sync", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestParseRepoRef(t *testing.T) {
	valid := map[string][2]string{
		"gin-gonic/gin":  {"gin-gonic", "gin"},
		" acme/app ":     {"acme", "app"},
		"user/repo.v2":   {"user", "repo.v2"},
		"user/repo_name": {"user", "repo_name"},
	}
	for input, want := range valid {
		owner, name, ok := parseRepoRef(input)
		if !ok || owner != want[0] || name != want[1] {
			t.Fatalf("parseRepoRef(%q) = %q %q %v", input, owner, name, ok)
		}
	}
	invalid := []string{"", "gin", "/gin", "gin/", "a/b/c", "a b/c", "a/b c", strings.Repeat("a", repoPartMaxRunes+1) + "/b"}
	for _, input := range invalid {
		if _, _, ok := parseRepoRef(input); ok {
			t.Fatalf("parseRepoRef(%q) unexpectedly ok", input)
		}
	}
}

func TestIsValidRepoItemKind(t *testing.T) {
	for _, kind := range []string{store.RepoKindPullRequest, store.RepoKindIssue, store.RepoKindCommit} {
		if !isValidRepoItemKind(kind) {
			t.Fatalf("%q rejected", kind)
		}
	}
	for _, kind := range []string{"", "pr", "pull_requests"} {
		if isValidRepoItemKind(kind) {
			t.Fatalf("%q accepted", kind)
		}
	}
}

func TestRunSyncRepoRejectsInvalidRequests(t *testing.T) {
	cases := map[string]string{
		"bad json":     `{`,
		"missing user": `{}`,
	}
	for name, body := range cases {
		w := doRunSync(&fakeRepoRunner{}, body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, body = %s", name, w.Code, w.Body.String())
		}
	}
}

func TestRunSyncRepoUnavailableWithoutRunner(t *testing.T) {
	w := doRunSync(nil, `{"user":"alice"}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
}
