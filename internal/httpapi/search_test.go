package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/Errrori/workpilot/internal/store"
)

type fakeSearchService struct {
	groupID string
	query   string
	topK    int
	sources []store.Citation
	err     error
}

func (s *fakeSearchService) Search(_ context.Context, groupID, query string, topK int) ([]store.Citation, error) {
	s.groupID, s.query, s.topK = groupID, query, topK
	if s.err != nil {
		return nil, s.err
	}
	return s.sources, nil
}

func doSearch(svc SearchService, body string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/groups/:id/search", func(c *gin.Context) { performSearch(c, svc) })
	req := httptest.NewRequest(http.MethodPost, "/groups/group-1/search", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestSearchReturnsSources(t *testing.T) {
	svc := &fakeSearchService{sources: []store.Citation{{Index: 1, FileID: 7, FileName: "PRD.md", ChunkIndex: 0, Score: 0.9}}}

	w := doSearch(svc, `{"query":"进度？","top_k":3}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if svc.groupID != "group-1" || svc.query != "进度？" || svc.topK != 3 {
		t.Fatalf("service input = %q %q %d", svc.groupID, svc.query, svc.topK)
	}
	if !strings.Contains(w.Body.String(), `"file_id":7`) || !strings.Contains(w.Body.String(), `"score":0.9`) {
		t.Fatalf("body = %q", w.Body.String())
	}
}

func TestSearchEmptyResultReturnsEmptyArray(t *testing.T) {
	w := doSearch(&fakeSearchService{}, `{"query":"进度？"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"sources":[]`) {
		t.Fatalf("body = %q", w.Body.String())
	}
}

func TestSearchReportsServiceError(t *testing.T) {
	w := doSearch(&fakeSearchService{err: errors.New("embedding down")}, `{"query":"q"}`)
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "embedding down") {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
}

func TestSearchRejectsInvalidRequests(t *testing.T) {
	long := strings.Repeat("查", maxQuestionRunes+1)
	cases := map[string]string{
		"bad json":      `{`,
		"missing query": `{"top_k":3}`,
		"too long":      `{"query":"` + long + `"}`,
		"top_k high":    `{"query":"q","top_k":21}`,
		"top_k low":     `{"query":"q","top_k":-1}`,
	}
	for name, body := range cases {
		w := doSearch(&fakeSearchService{}, body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, body = %s", name, w.Code, w.Body.String())
		}
	}
}

func TestSearchUnavailableWithoutService(t *testing.T) {
	w := doSearch(nil, `{"query":"q"}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
}
