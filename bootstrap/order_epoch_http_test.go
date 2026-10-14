package bootstrap

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/JustIkra/umir-serving/contract"
	"github.com/JustIkra/umir-serving/internal/adapters/warehousehttp"
)

type s3HTTPVerifier struct{}

func (s3HTTPVerifier) Verify(_ context.Context, token string) (contract.RequestScope, error) {
	scope := s3Scope(tenantA)
	scope.UserID = tenantB
	scope.Permissions = append(scope.Permissions, "consumer.order.evaluate", "consumer.order.commit", "consumer.order.resolve")
	switch token {
	case "prepare":
		scope.Operation = "prepareOrder"
	case "stale-prepare":
		scope.Operation = "prepareOrder"
		scope.TimeZone = "Europe/Berlin"
		scope.SettingsVersion = "2"
	case "confirm":
		scope.Operation = "confirmOrder"
	case "resolve":
		scope.Operation = "resolveOrder"
		scope.TimeZone = "Europe/Berlin"
		scope.SettingsVersion = "2"
	case "retry":
		scope.Operation = "confirmOrder"
		scope.TimeZone = "Europe/Berlin"
		scope.SettingsVersion = "2"
	default:
		return contract.RequestScope{}, fmt.Errorf("invalid token")
	}
	return scope.WithToken(token), nil
}
func TestS3HTTPUnknownRetryResolutionAndServingReset(t *testing.T) {
	at := "2026-10-13T12:00:00+03:00"
	complete := false
	commits := 0
	oldEpoch := peerEpoch
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		contextWire := map[string]any{"orgID": orgA, "tenantID": tenantA, "runtimeEpoch": oldEpoch, "settings": map[string]any{"timeZone": "Europe/Moscow", "settingsVersion": "1"}, "evaluatedAt": at, "stateVersion": "9007199254740993"}
		var result any
		switch strings.TrimPrefix(r.URL.Path, "/internal/warehouse/v2/r/"+tenantA) {
		case "/order-evaluations":
			if r.Header.Get("Authorization") == "Bearer stale-prepare" {
				w.WriteHeader(409)
				if err := json.NewEncoder(w).Encode(map[string]any{"code": "SettingsChanged", "message": "stale fresh settings", "outcome": "not_committed", "retryable": false}); err != nil {
					t.Error(err)
				}
				return
			}
			result = map[string]any{"evaluatedAt": at, "stateVersion": "9007199254740993", "requestedPositions": []any{map[string]any{"lineNo": "1", "dishID": "dish", "portions": "2"}}, "acceptedPositions": []any{map[string]any{"lineNo": "1", "dishID": "dish", "portions": "2"}}, "changes": []any{}}
		case "/commands":
			result = map[string]any{"found": false}
		case "/order-attempts":
			if r.Header.Get("X-Umir-Runtime-Epoch") != peerEpoch || len(r.URL.Query()) != 1 {
				t.Errorf("wrong passive identity: %s %v", r.URL, r.Header)
			}
			result = map[string]any{"state": "Unseen"}
			if complete {
				result = map[string]any{"state": "Completed", "result": map[string]any{"kind": "OrderCommitted", "order": map[string]any{"orderID": "order", "proposalID": "proposal", "placedAt": at, "acceptedPositions": []any{map[string]any{"lineNo": "1", "dishID": "dish", "portions": "2"}}, "allocations": []any{map[string]any{"productID": "p", "batchID": "b", "quantity": "9007199254740993"}}}}}
			}
		case "/order-commits":
			commits++
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
	peerUnavailable := false
	ids := 0
	r, err := NewRegistry(RegistryOptions{Epoch: epochA, Now: time.Now, NewID: func() string {
		ids++
		if ids == 1 {
			return "proposal"
		}
		return "new-proposal"
	}, WarehouseFactory: func(context.Context, string) (WarehousePorts, error) {
		if peerUnavailable {
			return WarehousePorts{}, contract.ErrInventoryUnavailable
		}
		c, e := warehousehttp.NewScopedClient(peer.URL, peer.Client(), time.Second, warehousehttp.ScopedOptions{TenantID: tenantA, Epoch: oldEpoch, ServiceKey: "synthetic-serving", Verifier: s3HTTPVerifier{}})
		return WarehousePorts{Orders: c, Attempts: c, Catalog: c, Menu: c, Epoch: oldEpoch}, e
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
	p := request("POST", "/order-proposals", "prepare", `{"positions":[{"dishID":"dish","portions":"2"}]}`)
	if p.Code != 200 || !strings.Contains(p.Body.String(), `"warehouseEpoch":"`+peerEpoch+`"`) {
		t.Fatalf("prepare=%d %s", p.Code, p.Body)
	}
	body := `{"commandID":"command","proposalID":"proposal"}`
	first := request("POST", "/order-confirmations", "confirm", body)
	if first.Code != 502 || !strings.Contains(first.Body.String(), `"outcome":"unknown"`) {
		t.Fatalf("first=%d %s", first.Code, first.Body)
	}
	retry := request("POST", "/order-confirmations", "retry", body)
	if commits != 1 || retry.Code != 200 {
		t.Fatalf("retry=%d %s commits=%d", retry.Code, retry.Body, commits)
	}
	stale := request("POST", "/order-proposals", "stale-prepare", `{"positions":[{"dishID":"dish","portions":"2"}]}`)
	if stale.Code != 409 || !strings.Contains(stale.Body.String(), `"code":"SettingsChanged"`) || commits != 1 {
		t.Fatalf("fresh stale=%d %s", stale.Code, stale.Body)
	}
	complete = true
	resolved := request("GET", "/order-confirmations/resolve?commandID=command&proposalID=proposal&runtimeEpoch="+epochA, "resolve", "")
	if resolved.Code != 200 || !strings.Contains(resolved.Body.String(), `"settingsVersion":"1"`) || !strings.Contains(resolved.Body.String(), `"quantity":"9007199254740993"`) {
		t.Fatalf("resolved=%d %s", resolved.Code, resolved.Body)
	}
	repeated := request("POST", "/order-confirmations", "retry", body)
	if repeated.Code != 200 || commits != 1 || repeated.Body.String() != resolved.Body.String() {
		t.Fatalf("terminal retry=%d %s commits=%d", repeated.Code, repeated.Body, commits)
	}
	oldEpoch = tenantB
	lost := request("POST", "/order-confirmations", "retry", body)
	if lost.Code != 409 || !strings.Contains(lost.Body.String(), `"outcome":"indeterminate"`) || commits != 1 {
		t.Fatalf("cached old epoch=%d %s commits=%d", lost.Code, lost.Body, commits)
	}
	fresh := request("POST", "/order-proposals", "prepare", `{"positions":[{"dishID":"dish","portions":"2"}]}`)
	if fresh.Code != 200 || !strings.Contains(fresh.Body.String(), `"warehouseEpoch":"`+tenantB+`"`) || !strings.Contains(fresh.Body.String(), `"proposalID":"new-proposal"`) {
		t.Fatalf("new epoch fresh=%d %s", fresh.Code, fresh.Body)
	}
	peerUnavailable = true
	resetReq := httptest.NewRequest("GET", "/internal/serving/v2/r/"+tenantA+"/order-confirmations/resolve?commandID=command&proposalID=proposal&runtimeEpoch="+peerEpoch, nil)
	resetReq.Header.Set("Authorization", "Bearer resolve")
	resetReq.Header.Set("X-Umir-Service-Key", "synthetic-gateway")
	reset := httptest.NewRecorder()
	h.ServeHTTP(reset, resetReq)
	if reset.Code != 409 || !strings.Contains(reset.Body.String(), `"outcome":"indeterminate"`) || commits != 1 {
		t.Fatalf("reset=%d %s", reset.Code, reset.Body)
	}
}
