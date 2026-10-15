package warehousehttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/JustIkra/umir-serving/contract"
)

func finInputsFixture(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile("testdata/fin1-recommendation-inputs.json")
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func finMap(t *testing.T, value any) map[string]any {
	t.Helper()
	result, ok := value.(map[string]any)
	if !ok {
		t.Fatal("invalid fixture object")
	}
	return result
}

func finArray(t *testing.T, value any) []any {
	t.Helper()
	result, ok := value.([]any)
	if !ok {
		t.Fatal("invalid fixture array")
	}
	return result
}

func TestFINRecommendationInputsStrictBoundary(t *testing.T) {
	for name, mutate := range map[string]func(map[string]any){
		"legacy-context-field": func(v map[string]any) { finMap(t, v["context"])["warehouseEpoch"] = s2Epoch },
		"valid":                func(map[string]any) {},
		"unknown":              func(v map[string]any) { finMap(t, v["result"])["extra"] = true },
		"epoch":                func(v map[string]any) { finMap(t, finMap(t, v["result"])["coverage"])["epoch"] = s2Other },
		"missing-complete":     func(v map[string]any) { delete(finMap(t, finMap(t, v["result"])["coverage"]), "complete") },
		"menu-version":         func(v map[string]any) { finMap(t, finMap(t, v["result"])["menu"])["stateVersion"] = "43" },
		"quantity-overflow": func(v map[string]any) {
			finMap(t, finArray(t, finMap(t, v["result"])["riskBatches"])[0])["remainingQuantity"] = "9223372036854775808"
		},
		"quantity-number": func(v map[string]any) {
			finMap(t, finArray(t, finMap(t, v["result"])["riskBatches"])[0])["remainingQuantity"] = 1
		},
		"unknown-risk-field": func(v map[string]any) {
			finMap(t, finArray(t, finMap(t, v["result"])["riskBatches"])[0])["pending"] = false
		},
	} {
		t.Run(name, func(t *testing.T) {
			value := finInputsFixture(t)
			mutate(value)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/internal/warehouse/v2/r/"+s2Tenant+"/recommendation-inputs" || r.Method != "GET" || r.Header.Get("Authorization") != "Bearer synthetic-a" || r.Header.Get("X-Umir-Service-Key") != "synthetic-serving-key" {
					t.Error("wrong recommendation trust forwarding")
				}
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(value); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			scope := s2Scope("recommendations", "consumer.recommendations.read")
			scope.TimeZone = "Europe/Moscow"
			client := s2Client(t, server, scope)
			observation := &s2Observation{}
			ctx := contract.WithWarehouseObserver(contract.WithRequestScope(context.Background(), scope), observation)
			input, err := client.RecommendationInputs(ctx)
			if name == "valid" {
				if err != nil || input.StateVersion != 42 || len(input.RiskBatches) != 2 || len(observation.values) != 1 {
					t.Fatalf("valid facts %+v %v", input, err)
				}
			} else if err == nil || len(observation.values) != 0 {
				t.Fatalf("invalid capture admitted/observed: %v, observations=%d", err, len(observation.values))
			}
		})
	}
}

func TestFINRecommendationInputsCapacityMappingAndRefusal(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(503)
		if err := json.NewEncoder(w).Encode(map[string]any{"code": "LimitExceeded", "message": "Capacity exceeded.", "outcome": "unavailable", "retryable": false}); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	scope := s2Scope("recommendations", "consumer.recommendations.read")
	client := s2Client(t, server, scope)
	_, err := client.RecommendationInputs(contract.WithRequestScope(context.Background(), scope))
	var failure *contract.OperationError
	if !errors.As(err, &failure) || failure.Code != "CalculationUnavailable" || failure.Outcome != "unavailable" {
		t.Fatalf("capacity mapping: %v", err)
	}
	wrong := s2Scope("menu", "consumer.menu.evaluate")
	client = s2Client(t, server, wrong)
	if _, err := client.RecommendationInputs(contract.WithRequestScope(context.Background(), wrong)); err == nil || calls != 1 {
		t.Fatalf("menu authority reached recommendation facts: %v, calls=%d", err, calls)
	}
}
