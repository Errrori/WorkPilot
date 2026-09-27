package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/Errrori/workpilot/internal/store"
	"github.com/Errrori/workpilot/internal/tasks"
	"github.com/Errrori/workpilot/internal/ws"
)

type fakeTaskService struct {
	groupID string
	user    string
	fileID  int64
	created []store.Task
	err     error
}

func (s *fakeTaskService) Extract(_ context.Context, groupID, user string, fileID int64) ([]store.Task, error) {
	s.groupID, s.user, s.fileID = groupID, user, fileID
	if s.err != nil {
		return nil, s.err
	}
	return s.created, nil
}

func doExtract(svc TaskService, body string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	hub := ws.NewHub(context.Background(), nil)
	r.POST("/groups/:id/tasks/extract", func(c *gin.Context) {
		runExtractTasks(c, nil, hub, svc, c.Param("id"))
	})
	req := httptest.NewRequest(http.MethodPost, "/groups/group-1/tasks/extract", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestExtractTasksReturnsSuggestions(t *testing.T) {
	svc := &fakeTaskService{created: []store.Task{{
		ID:      3,
		GroupID: "group-1",
		Title:   "补齐测试环境",
		Status:  store.TaskStatusSuggested,
		Source:  store.TaskSourceExtracted,
	}}}

	w := doExtract(svc, `{"user":"alice"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if svc.groupID != "group-1" || svc.user != "alice" || svc.fileID != 0 {
		t.Fatalf("service input = %q %q %d", svc.groupID, svc.user, svc.fileID)
	}
	for _, want := range []string{`"title":"补齐测试环境"`, `"count":1`, `"status":"suggested"`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Fatalf("body missing %q: %s", want, w.Body.String())
		}
	}
}

func TestExtractTasksNoMaterial(t *testing.T) {
	w := doExtract(&fakeTaskService{err: tasks.ErrNoMaterial}, `{"user":"alice"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
}

func TestExtractTasksRejectsInvalidRequests(t *testing.T) {
	cases := map[string]string{
		"bad json":     `{`,
		"missing user": `{"file_id":1}`,
		"negative id":  `{"user":"alice","file_id":-1}`,
	}
	for name, body := range cases {
		w := doExtract(&fakeTaskService{}, body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, body = %s", name, w.Code, w.Body.String())
		}
	}
}

func TestExtractTasksUnavailableWithoutService(t *testing.T) {
	w := doExtract(nil, `{"user":"alice"}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
}
