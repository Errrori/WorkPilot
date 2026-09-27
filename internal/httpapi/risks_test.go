package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/Errrori/workpilot/internal/risks"
	"github.com/Errrori/workpilot/internal/store"
	"github.com/Errrori/workpilot/internal/ws"
)

type fakeRiskService struct {
	groupID string
	user    string
	fileID  int64
	created []store.Risk
	err     error
}

func (s *fakeRiskService) Extract(_ context.Context, groupID, user string, fileID int64) ([]store.Risk, error) {
	s.groupID, s.user, s.fileID = groupID, user, fileID
	if s.err != nil {
		return nil, s.err
	}
	return s.created, nil
}

func doExtractRisks(svc RiskService, body string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	hub := ws.NewHub(context.Background(), nil)
	r.POST("/groups/:id/risks/extract", func(c *gin.Context) {
		runExtractRisks(c, nil, hub, svc, c.Param("id"))
	})
	req := httptest.NewRequest(http.MethodPost, "/groups/group-1/risks/extract", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestExtractRisksReturnsSuggestions(t *testing.T) {
	svc := &fakeRiskService{created: []store.Risk{{
		ID:       3,
		GroupID:  "group-1",
		Title:    "第三方接口延期阻塞联调",
		Severity: store.RiskSeverityHigh,
		Status:   store.RiskStatusSuggested,
		Source:   store.RiskSourceExtracted,
	}}}

	w := doExtractRisks(svc, `{"user":"alice"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if svc.groupID != "group-1" || svc.user != "alice" || svc.fileID != 0 {
		t.Fatalf("service input = %q %q %d", svc.groupID, svc.user, svc.fileID)
	}
	for _, want := range []string{`"title":"第三方接口延期阻塞联调"`, `"count":1`, `"status":"suggested"`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Fatalf("body missing %q: %s", want, w.Body.String())
		}
	}
}

func TestExtractRisksNoMaterial(t *testing.T) {
	w := doExtractRisks(&fakeRiskService{err: risks.ErrNoMaterial}, `{"user":"alice"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
}

func TestExtractRisksRejectsInvalidRequests(t *testing.T) {
	cases := map[string]string{
		"bad json":     `{`,
		"missing user": `{"file_id":1}`,
		"negative id":  `{"user":"alice","file_id":-1}`,
	}
	for name, body := range cases {
		w := doExtractRisks(&fakeRiskService{}, body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, body = %s", name, w.Code, w.Body.String())
		}
	}
}

func TestExtractRisksUnavailableWithoutService(t *testing.T) {
	w := doExtractRisks(nil, `{"user":"alice"}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
}
