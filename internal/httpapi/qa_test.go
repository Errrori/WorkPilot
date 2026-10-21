package httpapi

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestQALimit(t *testing.T) {
	cases := map[string]int{
		"":    50,
		"10":  10,
		"0":   50,
		"-1":  50,
		"201": 50,
		"abc": 50,
		"200": 200,
	}
	for raw, want := range cases {
		if got := qaLimit(raw, 50, 200); got != want {
			t.Fatalf("qaLimit(%q) = %d, want %d", raw, got, want)
		}
	}
}

func TestParseQAConversationID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		param string
		ok    bool
		id    int64
	}{
		{"12", true, 12},
		{"0", false, 0},
		{"-3", false, 0},
		{"abc", false, 0},
	}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Params = gin.Params{{Key: "conversationID", Value: tc.param}}
		id, ok := parseQAConversationID(c)
		if ok != tc.ok || id != tc.id {
			t.Fatalf("parseQAConversationID(%q) = (%d, %v), want (%d, %v)", tc.param, id, ok, tc.id, tc.ok)
		}
	}
}
