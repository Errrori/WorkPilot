package httpapi

import (
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Errrori/workpilot/internal/store"
)

const (
	defaultUsageWindow = 7 * 24 * time.Hour
	maxUsageWindow     = 90 * 24 * time.Hour
	defaultUsageLimit  = 20
	maxUsageLimit      = 100
)

// UsagePricing estimates LLM cost from token counts.
type UsagePricing struct {
	InputPerMTok  float64
	OutputPerMTok float64
	Currency      string
}

type usageSummaryBody struct {
	store.LLMUsageSummary
	Cost float64 `json:"cost"`
}

type usageSourceBody struct {
	Source string `json:"source"`
	store.LLMUsageSummary
	Cost float64 `json:"cost"`
}

func usageGroup(pool *pgxpool.Pool, pricing UsagePricing) gin.HandlerFunc {
	return func(c *gin.Context) {
		groupID := c.Param("id")
		if !ensureGroup(c, pool, groupID) {
			return
		}
		since, until, ok := usageWindow(c)
		if !ok {
			return
		}
		limit := defaultUsageLimit
		if raw := c.Query("limit"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 || n > maxUsageLimit {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid limit"})
				return
			}
			limit = n
		}

		ctx := c.Request.Context()
		summary, err := store.LLMUsageSummaryForGroup(ctx, pool, groupID, since, until)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		bySource, err := store.LLMUsageBySource(ctx, pool, groupID, since, until)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		recent, err := store.ListLLMUsage(ctx, pool, groupID, since, until, limit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		sources := make([]usageSourceBody, 0, len(bySource))
		for _, s := range bySource {
			sources = append(sources, usageSourceBody{
				Source:          s.Source,
				LLMUsageSummary: s.LLMUsageSummary,
				Cost:            estimateUsageCost(s.LLMUsageSummary, pricing),
			})
		}
		if recent == nil {
			recent = []store.LLMUsage{}
		}

		c.JSON(http.StatusOK, gin.H{
			"window": gin.H{"from": since, "to": until},
			"pricing": gin.H{
				"input_per_mtok":  pricing.InputPerMTok,
				"output_per_mtok": pricing.OutputPerMTok,
				"currency":        pricing.Currency,
			},
			"summary": usageSummaryBody{
				LLMUsageSummary: summary,
				Cost:            estimateUsageCost(summary, pricing),
			},
			"by_source": sources,
			"recent":    recent,
		})
	}
}

func usageWindow(c *gin.Context) (time.Time, time.Time, bool) {
	until := time.Now()
	if raw := c.Query("to"); raw != "" {
		t, err := parseUsageTime(raw, true)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid to (want RFC3339 or YYYY-MM-DD)"})
			return time.Time{}, time.Time{}, false
		}
		until = t
	}
	since := until.Add(-defaultUsageWindow)
	if raw := c.Query("from"); raw != "" {
		t, err := parseUsageTime(raw, false)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid from (want RFC3339 or YYYY-MM-DD)"})
			return time.Time{}, time.Time{}, false
		}
		since = t
	}
	if !since.Before(until) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "from must be before to"})
		return time.Time{}, time.Time{}, false
	}
	if until.Sub(since) > maxUsageWindow {
		c.JSON(http.StatusBadRequest, gin.H{"error": "time window exceeds 90 days"})
		return time.Time{}, time.Time{}, false
	}
	return since, until, true
}

func parseUsageTime(raw string, upper bool) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, nil
	}
	t, err := time.ParseInLocation("2006-01-02", raw, time.Local)
	if err != nil {
		return time.Time{}, err
	}
	if upper {
		return t.AddDate(0, 0, 1), nil
	}
	return t, nil
}

func estimateUsageCost(s store.LLMUsageSummary, p UsagePricing) float64 {
	cost := (float64(s.PromptTokens)*p.InputPerMTok + float64(s.CompletionTokens)*p.OutputPerMTok) / 1e6
	return math.Round(cost*1e4) / 1e4
}
