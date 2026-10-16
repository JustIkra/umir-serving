package httpapi

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/JustIkra/umir-serving/contract"
)

func TestS2WarehouseRefusalStatusPreserved(t *testing.T) {
	for code, status := range map[string]int{
		"InvalidArgument": 400, "InvalidRequest": 400,
		"Unauthenticated": 401, "InvalidToken": 401, "Forbidden": 403,
		"ScopeDeleted": 410, "CommandConflict": 409, "RuntimeReset": 409,
		"SettingsChanged": 409, "RefreshRequired": 409, "ProductConflict": 409, "DishConflict": 409,
		"NotFound": 404, "ProposalNotFound": 404,
		"RuntimeNotReady": 503, "InventoryUnavailable": 503, "CalculationUnavailable": 503,
		"AuthUnavailable": 503, "ServiceUnavailable": 503, "TimeZoneDataUnavailable": 503,
		"TechnicalError": 500, "UpstreamUnavailable": 502, "UpstreamTimeout": 504,
		"RateLimited": 429, "LimitExceeded": 429,
	} {
		t.Run(code, func(t *testing.T) {
			handler := &Handler{scopedErrors: true}
			response := httptest.NewRecorder()
			handler.writeError(response, &contract.OperationError{Code: code, Message: "warehouse refusal", Outcome: "unavailable", Retryable: true}, false)
			if response.Code != status {
				t.Fatalf("%s HTTP %d, want %d", code, response.Code, status)
			}
			var body struct {
				Code      string `json:"code"`
				Outcome   string `json:"outcome"`
				Retryable bool   `json:"retryable"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Code != code || body.Outcome != "unavailable" || !body.Retryable {
				t.Fatalf("lost refusal semantics: %+v", body)
			}
		})
	}
}

// The public v2 Error.code enum has CommandConflict and no
// CommandIdentityConflict; a Serving-side identity conflict must use it.
func TestServingCommandIdentityConflictIsEmittedAsCommandConflict(t *testing.T) {
	for _, scoped := range []bool{true, false} {
		handler := &Handler{scopedErrors: scoped}
		response := httptest.NewRecorder()
		handler.writeError(response, contract.ErrCommandIdentityConflict, true)
		var body struct {
			Code    string `json:"code"`
			Outcome string `json:"outcome"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if response.Code != 409 || body.Code != "CommandConflict" || body.Outcome != "not_committed" {
			t.Fatalf("scoped=%v: HTTP %d %+v", scoped, response.Code, body)
		}
	}
}
