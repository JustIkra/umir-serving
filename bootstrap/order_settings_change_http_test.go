package bootstrap

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/JustIkra/umir-serving/internal/adapters/warehousehttp"
)

// echoSettingsPeer is a Warehouse that, like the real scoped writer, puts the
// settings of the presented token into every response envelope, whatever
// settings the attempt was first made under.
type echoSettingsPeer struct {
	attemptState string
	commands     string // "none" or "found"
	commits      int
}

func TestPassiveReadsOfKnownOutcomeSurviveSettingsChange(t *testing.T) {
	const at = "2026-10-13T12:00:00+03:00"
	type readCall struct {
		method, path, token, body string
	}
	resolve := readCall{"GET", "/order-confirmations/resolve?commandID=command&proposalID=proposal&runtimeEpoch=" + epochA, "resolve", ""}
	retry := readCall{"POST", "/order-confirmations", "retry", `{"commandID":"command","proposalID":"proposal"}`}
	cases := []struct {
		name         string
		call         readCall
		attemptState string
		commands     string
		wantKind     string
	}{
		{"resolve completed", resolve, "Completed", "none", "OrderConfirmed"},
		{"retry completed", retry, "Completed", "none", "OrderConfirmed"},
		{"resolve unseen", resolve, "Unseen", "none", "ConfirmationPending"},
		{"retry unseen", retry, "Unseen", "none", "ConfirmationPending"},
		{"resolve pending with recorded command", resolve, "Pending", "found", "OrderConfirmed"},
		{"resolve unseen with recorded command", resolve, "Unseen", "found", "OrderConfirmed"},
		{"retry unseen with recorded command", retry, "Unseen", "found", "OrderConfirmed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := &echoSettingsPeer{attemptState: "Unseen", commands: "none"}
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
				scope, err := s3HTTPVerifier{}.Verify(r.Context(), token)
				if err != nil {
					t.Errorf("unknown token: %v", err)
					w.WriteHeader(401)
					return
				}
				contextWire := map[string]any{"orgID": orgA, "tenantID": tenantA, "runtimeEpoch": peerEpoch, "settings": map[string]any{"timeZone": scope.TimeZone, "settingsVersion": scope.SettingsVersion}, "evaluatedAt": at, "stateVersion": "9007199254740993"}
				orderWire := map[string]any{"orderID": "order", "proposalID": "proposal", "placedAt": at, "acceptedPositions": []any{map[string]any{"lineNo": "1", "dishID": "dish", "portions": "2"}}, "allocations": []any{map[string]any{"productID": "p", "batchID": "b", "quantity": "2"}}}
				var result any
				switch strings.TrimPrefix(r.URL.Path, "/internal/warehouse/v2/r/"+tenantA) {
				case "/order-evaluations":
					result = map[string]any{"evaluatedAt": at, "stateVersion": "9007199254740993", "requestedPositions": []any{map[string]any{"lineNo": "1", "dishID": "dish", "portions": "2"}}, "acceptedPositions": []any{map[string]any{"lineNo": "1", "dishID": "dish", "portions": "2"}}, "changes": []any{}}
				case "/commands":
					result = map[string]any{"found": false}
					if state.commands == "found" {
						result = map[string]any{"found": true, "record": map[string]any{"commandID": "command", "commandType": "ConfirmOrder", "commandFingerprint": confirmFingerprintForTest("proposal"), "result": map[string]any{"kind": "OrderPlaced", "order": orderWire}}}
					}
				case "/order-attempts":
					result = map[string]any{"state": state.attemptState}
					if state.attemptState == "Completed" {
						result = map[string]any{"state": "Completed", "result": map[string]any{"kind": "OrderCommitted", "order": orderWire}}
					}
				case "/order-commits":
					state.commits++
					w.WriteHeader(502)
					if err := json.NewEncoder(w).Encode(map[string]any{"code": "UpstreamUnavailable", "message": "lost reply", "outcome": "unknown", "retryable": true}); err != nil {
						t.Error(err)
					}
					return
				default:
					t.Errorf("unexpected Warehouse request: %s", r.URL)
					w.WriteHeader(500)
					return
				}
				if err := json.NewEncoder(w).Encode(map[string]any{"context": contextWire, "result": result}); err != nil {
					t.Error(err)
				}
			}))
			defer peer.Close()
			r, err := NewRegistry(RegistryOptions{Epoch: epochA, Now: time.Now, NewID: func() string { return "proposal" }, WarehouseFactory: func(context.Context, string) (WarehousePorts, error) {
				c, e := warehousehttp.NewScopedClient(peer.URL, peer.Client(), time.Second, warehousehttp.ScopedOptions{TenantID: tenantA, Epoch: peerEpoch, ServiceKey: "synthetic-serving", Verifier: s3HTTPVerifier{}})
				return WarehousePorts{Orders: c, Attempts: c, Catalog: c, Menu: c, Epoch: peerEpoch}, e
			}})
			if err != nil {
				t.Fatal(err)
			}
			registration := s3Registration(tenantA)
			registration.TimeZone, registration.SettingsVersion = "Europe/Moscow", "1"
			if err = r.Register(context.Background(), registration); err != nil {
				t.Fatal(err)
			}
			h, err := NewScopedHTTP(ScopedHTTPOptions{Registry: r, Verifier: s3HTTPVerifier{}, GatewayKey: "synthetic-gateway", Now: time.Now, RequestTimeout: time.Second, MaxRequestBytes: 1 << 20})
			if err != nil {
				t.Fatal(err)
			}
			request := func(method, path, token, body string) *httptest.ResponseRecorder {
				t.Helper()
				req := httptest.NewRequest(method, "/internal/serving/v2/r/"+tenantA+path, strings.NewReader(body))
				req.Header.Set("Authorization", "Bearer "+token)
				req.Header.Set("X-Umir-Service-Key", "synthetic-gateway")
				req.Header.Set("X-Umir-Runtime-Epoch", epochA)
				req.Header.Set("Content-Type", "application/json")
				out := httptest.NewRecorder()
				h.ServeHTTP(out, req)
				return out
			}
			if p := request("POST", "/order-proposals", "prepare", `{"positions":[{"dishID":"dish","portions":"2"}]}`); p.Code != 200 {
				t.Fatalf("prepare=%d %s", p.Code, p.Body)
			}
			if first := request("POST", "/order-confirmations", "confirm", `{"commandID":"command","proposalID":"proposal"}`); first.Code != 502 || !strings.Contains(first.Body.String(), `"outcome":"unknown"`) || state.commits != 1 {
				t.Fatalf("first=%d %s commits=%d", first.Code, first.Body, state.commits)
			}

			// Auth publishes new settings; the tokens now carry them and the
			// Warehouse echoes them, while the attempt keeps the first ones.
			changed := registration
			changed.TimeZone, changed.SettingsVersion = "Europe/Berlin", "2"
			if err = r.Register(context.Background(), changed); err != nil {
				t.Fatal(err)
			}
			state.attemptState, state.commands = tc.attemptState, tc.commands

			got := request(tc.call.method, tc.call.path, tc.call.token, tc.call.body)
			if got.Code != 200 || !strings.Contains(got.Body.String(), `"kind":"`+tc.wantKind+`"`) || state.commits != 1 {
				t.Fatalf("after settings change=%d %s commits=%d", got.Code, got.Body, state.commits)
			}
			if !strings.Contains(got.Body.String(), `"settingsVersion":"1"`) || !strings.Contains(got.Body.String(), `"timeZone":"Europe/Moscow"`) {
				t.Fatalf("response lost the saved settings: %s", got.Body)
			}
		})
	}
}
