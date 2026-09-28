package gitsync

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Errrori/workpilot/internal/store"
	"github.com/Errrori/workpilot/internal/ws"
)

type markRecord struct {
	status   string
	errMsg   string
	syncedAt time.Time
	nextSync time.Time
}

type fakeSyncStore struct {
	repo      store.Repo
	items     []store.RepoItemInput
	marks     []markRecord
	upsertErr error
}

func (s *fakeSyncStore) ListDueRepos(context.Context, int) ([]store.Repo, error) {
	return nil, nil
}

func (s *fakeSyncStore) GetRepoByID(context.Context, int64) (store.Repo, error) {
	return s.repo, nil
}

func (s *fakeSyncStore) UpsertRepoItems(_ context.Context, _ int64, items []store.RepoItemInput) (int, error) {
	if s.upsertErr != nil {
		return 0, s.upsertErr
	}
	s.items = append(s.items, items...)
	return len(items), nil
}

func (s *fakeSyncStore) MarkRepoSync(_ context.Context, _ int64, status, errMessage string, syncedAt, nextSync time.Time) (store.Repo, error) {
	s.marks = append(s.marks, markRecord{status: status, errMsg: errMessage, syncedAt: syncedAt, nextSync: nextSync})
	repo := s.repo
	repo.LastStatus = status
	repo.LastError = errMessage
	repo.LastSyncedAt = &syncedAt
	repo.NextSyncAt = nextSync
	s.repo = repo
	return repo, nil
}

type fakeNotifier struct {
	events []string
	repos  []store.Repo
}

func (n *fakeNotifier) BroadcastRepo(event string, r *store.Repo) {
	n.events = append(n.events, event)
	if r != nil {
		n.repos = append(n.repos, *r)
	}
}

func testRepo() store.Repo {
	return store.Repo{
		ID:         7,
		GroupID:    "group-1",
		Provider:   store.RepoProviderGitHub,
		Owner:      "acme",
		Name:       "app",
		Enabled:    true,
		NextSyncAt: time.Now(),
	}
}

func newTestScheduler(t *testing.T, handler http.HandlerFunc, st SyncStore, notifier Notifier) *Scheduler {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	sched, err := NewScheduler(SchedulerConfig{
		RedisAddr:    "localhost:6379",
		Concurrency:  1,
		Interval:     10 * time.Minute,
		LookbackDays: 7,
		Client:       NewClient(server.URL, "", time.Minute),
		Store:        st,
		Notifier:     notifier,
	})
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}
	t.Cleanup(func() { _ = sched.client.Close() })
	return sched
}

func fullActivityHandler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/pulls"):
			writeJSON(t, w, []map[string]any{
				pullJSON(3, "新增登录", "open", "alice", time.Now().Add(-time.Hour), false, false),
			})
		case strings.HasSuffix(r.URL.Path, "/issues"):
			writeJSON(t, w, []map[string]any{
				map[string]any{
					"number":     7,
					"title":      "登录偶发 500",
					"state":      "open",
					"user":       map[string]any{"login": "bob"},
					"html_url":   "https://example.com/issues/7",
					"updated_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
				},
			})
		case strings.HasSuffix(r.URL.Path, "/commits"):
			writeJSON(t, w, []map[string]any{
				commitJSON("abcdef0123456789", "fix login", "bob", time.Now().Add(-30*time.Minute)),
			})
		default:
			http.NotFound(w, r)
		}
	}
}

func TestSyncRepoSuccess(t *testing.T) {
	st := &fakeSyncStore{repo: testRepo()}
	notifier := &fakeNotifier{}
	sched := newTestScheduler(t, fullActivityHandler(t), st, notifier)

	if err := sched.SyncRepo(context.Background(), st.repo); err != nil {
		t.Fatalf("SyncRepo: %v", err)
	}
	if len(st.items) != 3 {
		t.Fatalf("items = %#v", st.items)
	}
	kinds := map[string]bool{}
	for _, it := range st.items {
		kinds[it.Kind] = true
	}
	for _, want := range []string{store.RepoKindPullRequest, store.RepoKindIssue, store.RepoKindCommit} {
		if !kinds[want] {
			t.Fatalf("missing kind %q in %#v", want, st.items)
		}
	}
	if len(st.marks) != 1 || st.marks[0].status != store.RepoLastStatusSucceeded || st.marks[0].errMsg != "" {
		t.Fatalf("marks = %#v", st.marks)
	}
	if !st.marks[0].nextSync.After(st.marks[0].syncedAt) {
		t.Fatalf("next sync = %s, synced at = %s", st.marks[0].nextSync, st.marks[0].syncedAt)
	}
	if len(notifier.events) != 1 || notifier.events[0] != ws.EventRepoSynced || notifier.repos[0].LastStatus != store.RepoLastStatusSucceeded {
		t.Fatalf("notifier = %#v %#v", notifier.events, notifier.repos)
	}
}

func TestSyncRepoFailureMarksRepo(t *testing.T) {
	st := &fakeSyncStore{repo: testRepo()}
	notifier := &fakeNotifier{}
	sched := newTestScheduler(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}, st, notifier)

	err := sched.SyncRepo(context.Background(), st.repo)
	if err == nil || !strings.Contains(err.Error(), "status 500") {
		t.Fatalf("err = %v", err)
	}
	if len(st.items) != 0 {
		t.Fatalf("items = %#v", st.items)
	}
	if len(st.marks) != 1 || st.marks[0].status != store.RepoLastStatusFailed || !strings.Contains(st.marks[0].errMsg, "status 500") {
		t.Fatalf("marks = %#v", st.marks)
	}
	if len(notifier.events) != 1 || notifier.events[0] != ws.EventRepoSynced || notifier.repos[0].LastStatus != store.RepoLastStatusFailed {
		t.Fatalf("notifier = %#v %#v", notifier.events, notifier.repos)
	}
}

func TestSyncRepoUpsertFailureMarksRepo(t *testing.T) {
	st := &fakeSyncStore{repo: testRepo(), upsertErr: errors.New("db down")}
	notifier := &fakeNotifier{}
	sched := newTestScheduler(t, fullActivityHandler(t), st, notifier)

	err := sched.SyncRepo(context.Background(), st.repo)
	if err == nil || !strings.Contains(err.Error(), "db down") {
		t.Fatalf("err = %v", err)
	}
	if len(st.marks) != 1 || st.marks[0].status != store.RepoLastStatusFailed || !strings.Contains(st.marks[0].errMsg, "db down") {
		t.Fatalf("marks = %#v", st.marks)
	}
}
