package httpapi

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFINRecommendationReplyCapacityIsTypedFailure(t *testing.T) {
	w := httptest.NewRecorder()
	h := &Handler{scopedErrors: true}
	h.writeJSONLimited(w, 200, strings.Repeat("x", 16<<20), 16<<20)
	if w.Code != 503 || !strings.Contains(w.Body.String(), `"code":"CalculationUnavailable"`) || strings.Contains(w.Body.String(), strings.Repeat("x", 100)) {
		t.Fatalf("oversized reply became partial success: %d %s", w.Code, w.Body)
	}
}
