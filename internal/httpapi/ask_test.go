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

type fakeAskService struct {
	groupID  string
	user     string
	question string
	sources  []store.Citation
	deltas   []string
	answer   store.Message
	err      error
}

func (s *fakeAskService) Stream(_ context.Context, groupID, user, question string, onSources func([]store.Citation) error, onDelta func(string) error) (store.Message, error) {
	s.groupID, s.user, s.question = groupID, user, question
	if s.err != nil {
		return store.Message{}, s.err
	}
	if onSources != nil {
		if err := onSources(s.sources); err != nil {
			return store.Message{}, err
		}
	}
	for _, delta := range s.deltas {
		if onDelta != nil {
			if err := onDelta(delta); err != nil {
				return store.Message{}, err
			}
		}
	}
	return s.answer, nil
}

func doAsk(svc AskService, body string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/groups/:id/ask", func(c *gin.Context) { streamAnswer(c, svc, c.Param("id")) })
	req := httptest.NewRequest(http.MethodPost, "/groups/group-1/ask", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestAskStreamsEventsInOrder(t *testing.T) {
	svc := &fakeAskService{
		sources: []store.Citation{{Index: 1, FileID: 7, FileName: "PRD.md", ChunkIndex: 0, Score: 0.9}},
		deltas:  []string{"根据 [1] ", "十月上线。"},
		answer:  store.Message{ID: 9, GroupID: "group-1", SenderName: "WorkPilot AI", Content: "根据 [1] 十月上线。"},
	}

	w := doAsk(svc, `{"user":"alice","question":"进度如何？"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("content type = %q", got)
	}
	if svc.groupID != "group-1" || svc.user != "alice" || svc.question != "进度如何？" {
		t.Fatalf("service input = %q %q %q", svc.groupID, svc.user, svc.question)
	}

	body := w.Body.String()
	sourcesAt := strings.Index(body, "event:sources")
	deltaAt := strings.Index(body, "event:delta")
	doneAt := strings.Index(body, "event:done")
	if sourcesAt < 0 || deltaAt < 0 || doneAt < 0 {
		t.Fatalf("missing events in %q", body)
	}
	if !(sourcesAt < deltaAt && deltaAt < doneAt) {
		t.Fatalf("events out of order: %q", body)
	}
	for _, want := range []string{`"file_id":7`, `"text":"根据 [1] "`, `"id":9`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q: %q", want, body)
		}
	}
}

func TestAskReportsServiceError(t *testing.T) {
	w := doAsk(&fakeAskService{err: errors.New("llm down")}, `{"user":"alice","question":"q"}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "event:error") || !strings.Contains(w.Body.String(), "llm down") {
		t.Fatalf("body = %q", w.Body.String())
	}
}

func TestAskRejectsInvalidRequests(t *testing.T) {
	long := strings.Repeat("问", maxQuestionRunes+1)
	cases := map[string]string{
		"bad json":      `{`,
		"missing user":  `{"question":"q"}`,
		"missing quest": `{"user":"alice"}`,
		"too long":      `{"user":"alice","question":"` + long + `"}`,
	}
	for name, body := range cases {
		w := doAsk(&fakeAskService{}, body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, body = %s", name, w.Code, w.Body.String())
		}
	}
}

func TestAskUnavailableWithoutService(t *testing.T) {
	w := doAsk(nil, `{"user":"alice","question":"q"}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
}
