package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const s4Tenant = "33333333-3333-4333-8333-333333333333"
const s4Org = "22222222-2222-4222-8222-222222222222"
const s4WarehouseEpoch = "44444444-4444-4444-8444-444444444444"

func TestS4ProcessRequiresScopedConfiguration(t *testing.T) {
	for _, key := range []string{"AUTH_INTERNAL_URL", "SERVING_AUTH_KEY", "AUTH_RUNTIME_KEY", "GATEWAY_SERVING_KEY", "SERVING_WAREHOUSE_KEY", "TZDATA_VERSION"} {
		t.Run(key, func(t *testing.T) {
			s4Configure(t, "http://127.0.0.1:1", "http://127.0.0.1:2")
			t.Setenv(key, "")
			app, err := handler()
			if app != nil {
				app.CloseIdleConnections()
			}
			if err == nil {
				t.Fatalf("missing %s started an unscoped process", key)
			}
		})
	}
}

func s4Configure(t *testing.T, auth, warehouse string) {
	t.Helper()
	for key, value := range map[string]string{"AUTH_INTERNAL_URL": auth, "WAREHOUSE_INTERNAL_URL": warehouse, "SERVING_AUTH_KEY": "s4-auth-client", "AUTH_RUNTIME_KEY": "s4-auth-runtime", "GATEWAY_SERVING_KEY": "s4-gateway", "SERVING_WAREHOUSE_KEY": "s4-warehouse", "TZDATA_VERSION": "2026b", "WAREHOUSE_TIMEOUT": "1s", "AUTH_TIMEOUT": "1s", "REQUEST_TIMEOUT": "2s", "REGISTRY_POLL_INTERVAL": "10ms"} {
		t.Setenv(key, value)
	}
}

func s4JSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Error(err)
	}
}

func TestS4ProcessFIN1PublicBoundaries(t *testing.T) {
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	servingEpoch := ""
	unavailable := false
	commits := 0
	calls := 0
	proposalID := ""
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Umir-Service-Key") != "s4-auth-client" {
			t.Error("wrong Auth transport credential")
			w.WriteHeader(401)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/internal/auth/v1/jwks":
			s4JSON(t, w, map[string]any{"keys": []any{map[string]any{"kty": "OKP", "crv": "Ed25519", "alg": "EdDSA", "use": "sig", "kid": "s4-key", "x": base64.RawURLEncoding.EncodeToString(public)}}})
		case "/internal/auth/v1/registry":
			if unavailable {
				w.WriteHeader(503)
				return
			}
			services := []any{map[string]any{"restaurantID": s4Tenant, "service": "warehouse", "epoch": s4WarehouseEpoch, "state": "ready", "lifecycleVersion": "1"}}
			if servingEpoch != "" {
				services = append(services, map[string]any{"restaurantID": s4Tenant, "service": "serving", "epoch": servingEpoch, "state": "ready", "lifecycleVersion": "1"})
			}
			s4JSON(t, w, map[string]any{"registryRevision": "1", "complete": true, "entries": []any{map[string]any{"restaurant": map[string]any{"restaurantID": s4Tenant, "orgID": s4Org, "name": "Fixture", "lifecycle": "ready", "lifecycleVersion": "1"}, "settings": map[string]any{"timeZone": "Europe/Moscow", "settingsVersion": "1"}, "services": services}}, "tombstones": []any{}})
		default:
			t.Errorf("unexpected Auth path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer auth.Close()
	at := "2026-10-13T12:00:00+03:00"
	menuFixture := map[string]any{"evaluatedAt": at, "stateVersion": "9007199254740993", "dishes": []any{map[string]any{"dishID": "1", "dishName": "Dish", "isAvailable": true, "deficits": []any{}, "allocations": []any{map[string]any{"productID": "1", "productName": "Flour", "batchID": "batch", "quantity": "9007199254740993", "baseUnit": "g", "expiresAt": "2026-10-14T12:00:00+03:00"}}}}}
	positions := []any{map[string]any{"lineNo": "1", "dishID": "1", "portions": "1"}}
	warehouse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if r.Header.Get("X-Umir-Service-Key") != "s4-warehouse" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Error("missing Warehouse credentials")
		}
		prefix := "/internal/warehouse/v2/r/" + s4Tenant
		path := strings.TrimPrefix(r.URL.Path, prefix)
		if path == r.URL.Path {
			t.Errorf("unscoped dispatch %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		var result any
		switch path {
		case "/products":
			result = map[string]any{"evaluatedAt": at, "stateVersion": "9007199254740993", "items": []any{map[string]any{"productID": "1", "name": "Flour", "baseUnit": "g"}}}
		case "/dish-catalog":
			result = map[string]any{"evaluatedAt": at, "stateVersion": "9007199254740993", "items": []any{map[string]any{"dishID": "1", "name": "Dish", "ingredients": []any{map[string]any{"productID": "1", "quantity": "9007199254740993", "baseUnit": "g"}}}}}
		case "/dish":
			result = map[string]any{"evaluatedAt": at, "stateVersion": "9007199254740993", "dish": map[string]any{"dishID": "1", "name": "Dish", "ingredients": []any{map[string]any{"productID": "1", "quantity": "9007199254740993", "baseUnit": "g"}}}}
		case "/menu-evaluation":
			result = menuFixture
		case "/recommendation-inputs":
			result = map[string]any{"evaluatedAt": at, "stateVersion": "9007199254740993", "horizonHours": 72, "window": map[string]any{"from": "2026-10-06T12:00:00+03:00", "to": at, "days": 7}, "coverage": map[string]any{"epoch": s4WarehouseEpoch, "coverageFrom": at, "complete": false, "requestedFrom": "2026-10-06T12:00:00+03:00", "availableFrom": at}, "menu": menuFixture, "riskBatches": []any{map[string]any{"productID": "1", "productName": "Flour", "batchID": "batch", "remainingQuantity": "9007199254740993", "baseUnit": "g", "expiresAt": "2026-10-14T12:00:00+03:00", "remainingSeconds": "86400"}}, "demand": []any{}}
		case "/commands":
			result = map[string]any{"found": false}
		case "/order-attempts":
			result = map[string]any{"state": "Unseen"}
		case "/order-evaluations":
			if r.Header.Get("X-Umir-Runtime-Epoch") != s4WarehouseEpoch {
				t.Error("wrong evaluation epoch")
			}
			result = map[string]any{"evaluatedAt": at, "stateVersion": "9007199254740993", "requestedPositions": positions, "acceptedPositions": positions, "changes": []any{}}
		case "/order-commits":
			commits++
			var input struct {
				CommandID         string          `json:"commandID"`
				ProposalID        string          `json:"proposalID"`
				AcceptedPositions json.RawMessage `json:"acceptedPositions"`
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
			}
			proposalID = input.ProposalID
			result = map[string]any{"kind": "OrderCommitted", "order": map[string]any{"orderID": "order", "proposalID": proposalID, "placedAt": at, "acceptedPositions": positions, "allocations": []any{map[string]any{"productID": "1", "batchID": "batch", "quantity": "9007199254740993"}}}}
		default:
			t.Errorf("unexpected Warehouse path %s", path)
			w.WriteHeader(404)
			return
		}
		s4JSON(t, w, map[string]any{"context": map[string]any{"orgID": s4Org, "tenantID": s4Tenant, "runtimeEpoch": s4WarehouseEpoch, "settings": map[string]any{"timeZone": "Europe/Moscow", "settingsVersion": "1"}, "evaluatedAt": at, "stateVersion": "9007199254740993"}, "result": result})
	}))
	defer warehouse.Close()
	s4Configure(t, auth.URL, warehouse.URL)
	app, err := handler()
	if err != nil {
		t.Fatal(err)
	}
	defer app.CloseIdleConnections()
	request := func(method, path, key, token, epoch, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if key != "" {
			r.Header.Set("X-Umir-Service-Key", key)
		}
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		if epoch != "" {
			r.Header.Set("X-Umir-Runtime-Epoch", epoch)
		}
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		app.ServeHTTP(w, r)
		return w
	}
	runtimePath := "/internal/serving/v2/runtime"
	w := request("GET", runtimePath, "", "", "", "")
	if w.Code != 401 {
		t.Fatalf("unprotected process runtime: %d %s", w.Code, w.Body)
	}
	w = request("GET", runtimePath, "s4-auth-runtime", "", "", "")
	var status struct {
		Epoch string `json:"epoch"`
		Ready bool   `json:"ready"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &status) != nil || status.Epoch == "" || status.Ready {
		t.Fatalf("startup status %d %s", w.Code, w.Body)
	}
	epoch := status.Epoch
	startup := append(json.RawMessage(nil), w.Body.Bytes()...)
	sign := func(operation string, permissions []string, lifecycle bool) string {
		t.Helper()
		now := time.Now().Unix()
		c := map[string]any{"iss": "urn:umir:auth", "aud": []string{"umir-serving", "umir-warehouse"}, "iat": now, "nbf": now, "exp": now + 60, "jti": s4WarehouseEpoch, "kind": "user", "sub": "66666666-6666-4666-8666-666666666666", "sid": "77777777-7777-4777-8777-777777777777", "session_version": "1", "credential_version": "1", "access_version": "1", "org_id": s4Org, "tenant_id": s4Tenant, "time_zone": "Europe/Moscow", "settings_version": "1", "lifecycle_version": "1", "operation": operation, "permissions": permissions, "azp": "umir-gateway"}
		if lifecycle {
			c = map[string]any{"iss": "urn:umir:auth", "aud": []string{"umir-serving"}, "iat": now, "nbf": now, "exp": now + 60, "jti": s4WarehouseEpoch, "kind": "service", "sub": "service:auth", "azp": "umir-auth", "org_id": s4Org, "tenant_id": s4Tenant, "lifecycle_version": "1", "runtime_epoch": epoch, "operation": operation, "permissions": permissions}
		}
		head, e := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "JWT", "kid": "s4-key"})
		if e != nil {
			t.Fatal(e)
		}
		payload, e := json.Marshal(c)
		if e != nil {
			t.Fatal(e)
		}
		input := base64.RawURLEncoding.EncodeToString(head) + "." + base64.RawURLEncoding.EncodeToString(payload)
		return input + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, []byte(input)))
	}
	registration := `{"operationID":"` + s4WarehouseEpoch + `","expectedEpoch":"` + epoch + `","lifecycleVersion":"1","settings":{"timeZone":"Europe/Moscow","settingsVersion":"1"}}`
	w = request("PUT", "/internal/serving/v2/r/"+s4Tenant+"/registry", "s4-auth-runtime", sign("runtime.register", []string{"runtime.register"}, true), "", registration)
	if w.Code != 200 {
		t.Fatalf("register=%d %s", w.Code, w.Body)
	}
	mu.Lock()
	servingEpoch = epoch
	mu.Unlock()
	deadline := time.Now().Add(3 * time.Second)
	for {
		w = request("GET", runtimePath, "s4-auth-runtime", "", "", "")
		if json.Unmarshal(w.Body.Bytes(), &status) != nil {
			t.Fatal(w.Body.String())
		}
		if status.Ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("complete Auth registry did not make the scoped process ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	outputs := map[string]json.RawMessage{"RuntimeStatus": startup}
	for _, tc := range []struct {
		path, operation string
		permissions     []string
		schema          string
	}{
		{"products", "servingProducts", []string{"menu.read", "catalog.read"}, "ServingScopedProducts"},
		{"dish-catalog", "servingDishes", []string{"menu.read", "catalog.read"}, "ServingScopedDishCatalog"},
		{"dish?dishID=1", "servingDish", []string{"menu.read", "catalog.read"}, "ServingScopedDishDetails"},
		{"menu", "menu", []string{"menu.read", "consumer.menu.evaluate"}, "ServingScopedMenu"},
		{"stop-list", "stopList", []string{"menu.read", "consumer.menu.evaluate"}, "ServingScopedStopList"},
		{"recommendations", "recommendations", []string{"recommendations.read", "consumer.recommendations.read"}, "ServingScopedRecommendations"},
	} {
		w = request("GET", "/internal/serving/v2/r/"+s4Tenant+"/"+tc.path, "s4-gateway", sign(tc.operation, tc.permissions, false), "", "")
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Body.String(), `"warehouseEpoch":"`+s4WarehouseEpoch+`"`) {
			t.Fatalf("%s=%d %s", tc.path, w.Code, w.Body)
		}
		outputs[tc.schema] = append(json.RawMessage(nil), w.Body.Bytes()...)
	}
	w = request("POST", "/internal/serving/v2/r/"+s4Tenant+"/order-proposals", "s4-gateway", sign("prepareOrder", []string{"order.prepare", "consumer.order.evaluate"}, false), epoch, `{"positions":[{"dishID":"1","portions":"1"}]}`)
	if w.Code != 200 {
		t.Fatalf("prepare=%d %s", w.Code, w.Body)
	}
	outputs["ServingScopedOrderProposal"] = append(json.RawMessage(nil), w.Body.Bytes()...)
	var proposal struct {
		Result struct {
			ID string `json:"proposalID"`
		} `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &proposal); err != nil {
		t.Fatal(err)
	}
	confirm := `{"commandID":"once","proposalID":"` + proposal.Result.ID + `"}`
	for range 2 {
		w = request("POST", "/internal/serving/v2/r/"+s4Tenant+"/order-confirmations", "s4-gateway", sign("confirmOrder", []string{"order.confirm", "consumer.order.evaluate", "consumer.order.commit", "consumer.order.resolve"}, false), epoch, confirm)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"quantity":"9007199254740993"`) || !strings.Contains(w.Body.String(), `"placedAt":"`+at+`"`) {
			t.Fatalf("confirm=%d %s", w.Code, w.Body)
		}
	}
	outputs["ServingScopedConfirmResult"] = append(json.RawMessage(nil), w.Body.Bytes()...)
	w = request("GET", "/internal/serving/v2/r/"+s4Tenant+"/order-confirmations/resolve?commandID=once&proposalID="+proposal.Result.ID+"&runtimeEpoch="+epoch, "s4-gateway", sign("resolveOrder", []string{"order.read", "consumer.order.resolve"}, false), epoch, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"kind":"OrderConfirmed"`) {
		t.Fatalf("passive resolve=%d %s", w.Code, w.Body)
	}
	mu.Lock()
	before := calls
	count := commits
	mu.Unlock()
	if count != 1 {
		t.Fatalf("commits=%d", count)
	}
	for _, path := range []string{"id-suggestions?kind=dish", "writeoff-confirmations"} {
		w = request("GET", "/internal/serving/v2/r/"+s4Tenant+"/"+path, "s4-gateway", sign("servingProducts", []string{"menu.read", "catalog.read"}, false), "", "")
		if w.Code == 200 {
			t.Fatal("Warehouse-owned route attached to Serving")
		}
	}
	w = request("GET", "/products", "s4-gateway", "", "", "")
	if w.Code == 200 {
		t.Fatal("legacy route remained attached")
	}
	w = request("POST", "/internal/serving/v2/r/"+s4Tenant+"/order-confirmations", "s4-gateway", sign("confirmOrder", []string{"order.confirm", "consumer.order.commit", "consumer.order.resolve"}, false), s4WarehouseEpoch, confirm)
	if w.Code != 409 || !strings.Contains(w.Body.String(), `"outcome":"indeterminate"`) {
		t.Fatalf("old epoch=%d %s", w.Code, w.Body)
	}
	mu.Lock()
	after := calls
	unavailable = true
	mu.Unlock()
	if after != before {
		t.Fatal("refused routes called Warehouse")
	}
	w = request("POST", "/internal/serving/v2/r/"+s4Tenant+"/order-confirmations", "s4-gateway", sign("confirmOrder", []string{"order.confirm", "consumer.order.commit", "consumer.order.resolve"}, false), epoch, confirm)
	if w.Code != 503 || !strings.Contains(w.Body.String(), `"outcome":"unknown"`) {
		t.Fatalf("Auth discovery failure invented outcome: %d %s", w.Code, w.Body)
	}
	outputs["Error"] = append(json.RawMessage(nil), w.Body.Bytes()...)
	deadline = time.Now().Add(3 * time.Second)
	for {
		w = request("GET", runtimePath, "s4-auth-runtime", "", "", "")
		if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
			t.Fatal(err)
		}
		if !status.Ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("unavailable Auth retained ready status")
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	unavailable = false
	mu.Unlock()
	second, err := handler()
	if err != nil {
		t.Fatal(err)
	}
	defer second.CloseIdleConnections()
	app = second
	w = request("GET", runtimePath, "s4-auth-runtime", "", "", "")
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Epoch == epoch || status.Ready {
		t.Fatalf("restart reused readiness/epoch: %s", w.Body)
	}
	oldEpoch := epoch
	epoch = status.Epoch
	registration = `{"operationID":"` + s4WarehouseEpoch + `","expectedEpoch":"` + epoch + `","lifecycleVersion":"1","settings":{"timeZone":"Europe/Moscow","settingsVersion":"1"}}`
	w = request("PUT", "/internal/serving/v2/r/"+s4Tenant+"/registry", "s4-auth-runtime", sign("runtime.register", []string{"runtime.register"}, true), "", registration)
	if w.Code != 200 {
		t.Fatalf("restart registration=%d %s", w.Code, w.Body)
	}
	outputs["RuntimeReadiness"] = append(json.RawMessage(nil), w.Body.Bytes()...)
	mu.Lock()
	servingEpoch = epoch
	mu.Unlock()
	w = request("POST", "/internal/serving/v2/r/"+s4Tenant+"/order-confirmations", "s4-gateway", sign("confirmOrder", []string{"order.confirm", "consumer.order.commit", "consumer.order.resolve"}, false), oldEpoch, confirm)
	if w.Code != 409 || !strings.Contains(w.Body.String(), `"outcome":"indeterminate"`) {
		t.Fatalf("restart fabricated old outcome: %d %s", w.Code, w.Body)
	}
	mu.Lock()
	count = commits
	mu.Unlock()
	if count != 1 {
		t.Fatal("restart redispatched old commit")
	}
	if dir := os.Getenv("S4_RESPONSE_DIR"); dir != "" {
		data, e := json.MarshalIndent(outputs, "", "  ")
		if e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(filepath.Join(dir, "fin1-wire-responses.json"), data, 0600); e != nil {
			t.Fatal(e)
		}
	}
}

func TestS4ProcessShutdownCancelsRegistryDiscovery(t *testing.T) {
	entered := make(chan struct{})
	cancelled := make(chan struct{})
	auth := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done(); close(cancelled) }))
	defer auth.Close()
	s4Configure(t, auth.URL, "http://127.0.0.1:1")
	app, err := handler()
	if err != nil {
		t.Fatal(err)
	}
	defer app.CloseIdleConnections()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("startup did not request reconciliation")
	}
	done := make(chan struct{})
	go func() { app.CloseIdleConnections(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not join cancelled discovery")
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("peer request not cancelled")
	}
}

func TestS4ProcessReadinessRequiresSigningDiscovery(t *testing.T) {
	public, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	unavailable := true
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/internal/auth/v1/jwks" {
			if unavailable {
				w.WriteHeader(503)
				return
			}
			s4JSON(t, w, map[string]any{"keys": []any{map[string]any{"kty": "OKP", "crv": "Ed25519", "alg": "EdDSA", "use": "sig", "kid": "fixture", "x": base64.RawURLEncoding.EncodeToString(public)}}})
			return
		}
		s4JSON(t, w, map[string]any{"registryRevision": "1", "complete": true, "entries": []any{}, "tombstones": []any{}})
	}))
	defer auth.Close()
	s4Configure(t, auth.URL, "http://127.0.0.1:1")
	app, err := handler()
	if err != nil {
		t.Fatal(err)
	}
	defer app.CloseIdleConnections()
	status := func() bool {
		t.Helper()
		request := httptest.NewRequest("GET", "/internal/serving/v2/runtime", nil)
		request.Header.Set("X-Umir-Service-Key", "s4-auth-runtime")
		out := httptest.NewRecorder()
		app.ServeHTTP(out, request)
		var value struct {
			Ready bool `json:"ready"`
		}
		if out.Code != 200 || json.Unmarshal(out.Body.Bytes(), &value) != nil {
			t.Fatalf("status %d %s", out.Code, out.Body)
		}
		return value.Ready
	}
	deadline := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(deadline) {
		if status() {
			t.Fatal("missing Auth signing discovery was reported ready")
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	unavailable = false
	mu.Unlock()
	deadline = time.Now().Add(2 * time.Second)
	for !status() {
		if time.Now().After(deadline) {
			t.Fatal("signing discovery recovery did not restore readiness")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
