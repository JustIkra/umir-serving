package bootstrap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/JustIkra/umir-serving/internal/adapters/warehousehttp"
)

// pendingCommandsPeer is a Warehouse whose /order-attempts answers with a
// configurable state while /commands answers as configured. It counts commits.
type pendingCommandsPeer struct {
	attemptState string
	commands     string // "none", "found", "other-fingerprint", "error"
	commits      int
}

func confirmFingerprintForTest(proposalID string) string {
	digest := sha256.Sum256([]byte("ConfirmOrder\x00" + `{"proposalID":"` + proposalID + `"}`))
	return hex.EncodeToString(digest[:])
}

type retainedCall func(request func(method, path, token, body string) *httptest.ResponseRecorder) *httptest.ResponseRecorder

func TestRetainedAttemptConsultsRecordedCommandBeforeReportingPending(t *testing.T) {
	const at = "2026-10-13T12:00:00+03:00"
	calls := map[string]retainedCall{
		"resolve": func(request func(method, path, token, body string) *httptest.ResponseRecorder) *httptest.ResponseRecorder {
			return request("GET", "/order-confirmations/resolve?commandID=command&proposalID=proposal&runtimeEpoch="+epochA, "resolve", "")
		},
		"confirm retry": func(request func(method, path, token, body string) *httptest.ResponseRecorder) *httptest.ResponseRecorder {
			return request("POST", "/order-confirmations", "retry", `{"commandID":"command","proposalID":"proposal"}`)
		},
	}
	for callName, call := range calls {
		for _, owner := range []string{"Pending", "Unseen"} {
			for _, mode := range []string{"found", "other-fingerprint", "none", "error"} {
				if callName == "confirm retry" && owner == "Unseen" && mode == "none" {
					continue // nothing recorded: the single saved-command retry commit runs, covered by TestS3HTTPUnknownRetryResolutionAndServingReset
				}
				t.Run(callName+"/"+owner+"/"+mode, func(t *testing.T) {
					state := &pendingCommandsPeer{attemptState: "Unseen", commands: "none"}
					peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Content-Type", "application/json")
						contextWire := map[string]any{"orgID": orgA, "tenantID": tenantA, "runtimeEpoch": peerEpoch, "settings": map[string]any{"timeZone": "Europe/Moscow", "settingsVersion": "1"}, "evaluatedAt": at, "stateVersion": "9007199254740993"}
						orderWire := map[string]any{"orderID": "order", "proposalID": "proposal", "placedAt": at, "acceptedPositions": []any{map[string]any{"lineNo": "1", "dishID": "dish", "portions": "2"}}, "allocations": []any{map[string]any{"productID": "p", "batchID": "b", "quantity": "2"}}}
						var result any
						switch strings.TrimPrefix(r.URL.Path, "/internal/warehouse/v2/r/"+tenantA) {
						case "/order-evaluations":
							result = map[string]any{"evaluatedAt": at, "stateVersion": "9007199254740993", "requestedPositions": []any{map[string]any{"lineNo": "1", "dishID": "dish", "portions": "2"}}, "acceptedPositions": []any{map[string]any{"lineNo": "1", "dishID": "dish", "portions": "2"}}, "changes": []any{}}
						case "/commands":
							switch state.commands {
							case "none":
								result = map[string]any{"found": false}
							case "error":
								w.WriteHeader(502)
								if err := json.NewEncoder(w).Encode(map[string]any{"code": "UpstreamUnavailable", "message": "lookup lost", "outcome": "unknown", "retryable": true}); err != nil {
									t.Error(err)
								}
								return
							default:
								fingerprint := confirmFingerprintForTest("proposal")
								if state.commands == "other-fingerprint" {
									fingerprint = confirmFingerprintForTest("another-proposal")
								}
								result = map[string]any{"found": true, "record": map[string]any{"commandID": "command", "commandType": "ConfirmOrder", "commandFingerprint": fingerprint, "result": map[string]any{"kind": "OrderPlaced", "order": orderWire}}}
							}
						case "/order-attempts":
							result = map[string]any{"state": state.attemptState}
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
					if err = r.Register(context.Background(), s3Registration(tenantA)); err != nil {
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
					if first := request("POST", "/order-confirmations", "confirm", `{"commandID":"command","proposalID":"proposal"}`); first.Code != 502 || state.commits != 1 {
						t.Fatalf("first=%d %s commits=%d", first.Code, first.Body, state.commits)
					}

					// The owner reports no finished attempt; only /commands may tell more.
					state.attemptState = owner
					state.commands = mode
					got := call(request)
					if state.commits != 1 {
						t.Fatalf("%s sent a commit: commits=%d", callName, state.commits)
					}
					switch mode {
					case "found":
						if got.Code != 200 || !strings.Contains(got.Body.String(), `"kind":"OrderConfirmed"`) || !strings.Contains(got.Body.String(), `"orderID":"order"`) {
							t.Fatalf("found=%d %s", got.Code, got.Body)
						}
						// The attempt is Completed: a later call replays it without Warehouse reads.
						state.commands = "error"
						again := call(request)
						if again.Code != 200 || again.Body.String() != got.Body.String() || state.commits != 1 {
							t.Fatalf("replay=%d %s commits=%d", again.Code, again.Body, state.commits)
						}
					case "other-fingerprint":
						if got.Code != 409 || !strings.Contains(got.Body.String(), `"code":"CommandConflict"`) || !strings.Contains(got.Body.String(), `"outcome":"not_committed"`) {
							t.Fatalf("conflict=%d %s", got.Code, got.Body)
						}
						state.commands = "error"
						again := call(request)
						if again.Code != 409 || !strings.Contains(again.Body.String(), `"code":"CommandConflict"`) || state.commits != 1 {
							t.Fatalf("conflict replay=%d %s commits=%d", again.Code, again.Body, state.commits)
						}
					case "none":
						if got.Code != 200 || !strings.Contains(got.Body.String(), `"kind":"ConfirmationPending"`) {
							t.Fatalf("none=%d %s", got.Code, got.Body)
						}
					case "error":
						if got.Code != 502 || !strings.Contains(got.Body.String(), `"outcome":"unknown"`) {
							t.Fatalf("error=%d %s", got.Code, got.Body)
						}
					}
					if mode == "none" || mode == "error" {
						// The attempt did not change: once the owner records the order it completes.
						state.commands = "found"
						done := call(request)
						if done.Code != 200 || !strings.Contains(done.Body.String(), `"kind":"OrderConfirmed"`) || state.commits != 1 {
							t.Fatalf("later found=%d %s commits=%d", done.Code, done.Body, state.commits)
						}
					}
				})
			}
		}
	}
}
