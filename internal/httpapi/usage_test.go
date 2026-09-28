package httpapi

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Errrori/workpilot/internal/store"
)

func TestEstimateUsageCost(t *testing.T) {
	pricing := UsagePricing{InputPerMTok: 2, OutputPerMTok: 8, Currency: "CNY"}
	summary := store.LLMUsageSummary{PromptTokens: 1_000_000, CompletionTokens: 500_000}
	if got := estimateUsageCost(summary, pricing); got != 6 {
		t.Fatalf("cost = %v, want 6", got)
	}
	if got := estimateUsageCost(store.LLMUsageSummary{}, pricing); got != 0 {
		t.Fatalf("empty cost = %v", got)
	}
	if got := estimateUsageCost(store.LLMUsageSummary{PromptTokens: 123}, UsagePricing{InputPerMTok: 1}); got != 0.0001 {
		t.Fatalf("rounded cost = %v", got)
	}
}

func usageWindowContext(query string) *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	url := "/"
	if query != "" {
		url += "?" + query
	}
	c.Request = httptest.NewRequest("GET", url, nil)
	return c
}

func TestUsageWindowDefaultsTo7Days(t *testing.T) {
	since, until, ok := usageWindow(usageWindowContext(""))
	if !ok {
		t.Fatal("expected ok")
	}
	if d := until.Sub(since); d != defaultUsageWindow {
		t.Fatalf("window = %v, want %v", d, defaultUsageWindow)
	}
}

func TestUsageWindowParsesDates(t *testing.T) {
	since, until, ok := usageWindow(usageWindowContext("from=2026-09-01&to=2026-09-10"))
	if !ok {
		t.Fatal("expected ok")
	}
	if since.Format(time.DateOnly) != "2026-09-01" {
		t.Fatalf("since = %v", since)
	}
	if until.Format(time.DateOnly) != "2026-09-11" {
		t.Fatalf("until = %v (date-only to should cover the whole day)", until)
	}
}

func TestUsageWindowRejectsBadRange(t *testing.T) {
	for _, query := range []string{
		"from=2026-09-10&to=2026-09-01",
		"from=bad",
		"to=bad",
		"from=2026-01-01&to=2026-09-01",
	} {
		if _, _, ok := usageWindow(usageWindowContext(query)); ok {
			t.Fatalf("%s: expected rejection", query)
		}
	}
}
