package bootstrap_test

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/JustIkra/umir-serving/bootstrap"
	"github.com/JustIkra/umir-serving/contract"
)

func TestS2ScopedCompositionAndCommittedExpiryFailure(t *testing.T) {
	const tenant = "33333333-3333-4333-8333-333333333333"
	const org = "22222222-2222-4222-8222-222222222222"
	const epoch = "44444444-4444-4444-8444-444444444444"
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.FixedZone("MSK", 3*3600))
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := bootstrap.NewJWTVerifier(bootstrap.JWTOptions{Now: func() time.Time { return now }, Keys: func(context.Context, string) (ed25519.PublicKey, error) { return public, nil }})
	if err != nil {
		t.Fatal(err)
	}
	encode := func(v any) string {
		b, e := json.Marshal(v)
		if e != nil {
			t.Fatal(e)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	claims := map[string]any{"iss": "urn:umir:auth", "aud": []string{"umir-serving", "umir-warehouse"}, "iat": now.Unix(), "nbf": now.Unix(), "exp": now.Unix() + 60, "jti": epoch, "operation": "recommendations", "permissions": []string{"recommendations.read", "consumer.menu.evaluate"}, "kind": "user", "sub": "66666666-6666-4666-8666-666666666666", "sid": "77777777-7777-4777-8777-777777777777", "session_version": "1", "credential_version": "1", "access_version": "1", "org_id": org, "tenant_id": tenant, "time_zone": "Europe/Moscow", "settings_version": "1", "lifecycle_version": "1", "azp": "umir-gateway"}
	token := func() string {
		head := encode(map[string]string{"alg": "EdDSA", "typ": "JWT", "kid": "test-key"})
		body := head + "." + encode(claims)
		return body + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, []byte(body)))
	}
	calls := 0
	fail := false
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("X-Umir-Service-Key") != "serving-key" || r.URL.Path != "/internal/warehouse/v2/r/"+tenant+"/menu-evaluation" {
			t.Errorf("incorrect consumer call: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Umir-Committed-Version", "7")
		w.Header().Set("X-Umir-Invalidation-Required", "true")
		w.Header().Set("X-Umir-Runtime-Epoch", epoch)
		if fail {
			w.WriteHeader(http.StatusServiceUnavailable)
			writeTestBody(t, w, `{"code":"CalculationUnavailable","message":"calculation failed","outcome":"unavailable","retryable":true}`)
			return
		}
		writeTestBody(t, w, `{"context":{"orgID":"`+org+`","tenantID":"`+tenant+`","runtimeEpoch":"`+epoch+`","settings":{"timeZone":"Europe/Moscow","settingsVersion":"1"},"evaluatedAt":"2026-10-13T12:00:00+03:00","stateVersion":"7"},"result":{"evaluatedAt":"2026-10-13T12:00:00+03:00","stateVersion":"7","dishes":[]}}`)
	}))
	defer peer.Close()
	registry, err := bootstrap.NewRegistry(bootstrap.RegistryOptions{Epoch: epoch, Now: func() time.Time { return now }, NewID: func() string { return "proposal" }, WarehouseFactory: func(ctx context.Context, selected string) (bootstrap.WarehousePorts, error) {
		return bootstrap.NewScopedWarehouseHTTPPorts(bootstrap.ScopedWarehouseHTTPOptions{WarehouseInternalURL: peer.URL, WarehouseTimeout: time.Second, HTTPClient: peer.Client(), TenantID: selected, Epoch: epoch, ServiceKey: "serving-key", Verifier: verifier})
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(context.Background(), contract.RestaurantRegistration{OrgID: org, TenantID: tenant, OperationID: epoch, ExpectedEpoch: epoch, LifecycleVersion: "1"}); err != nil {
		t.Fatal(err)
	}
	handler, err := bootstrap.NewScopedHTTP(bootstrap.ScopedHTTPOptions{Registry: registry, Verifier: verifier, GatewayKey: "gateway-key", RequestTimeout: time.Second, MaxRequestBytes: 1024, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	request := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/internal/serving/v2/r/"+tenant+"/recommendations", nil)
		r.Header.Set("Authorization", "Bearer "+token())
		r.Header.Set("X-Umir-Service-Key", "gateway-key")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	w := request()
	if w.Code != 200 || w.Header().Get("X-Umir-Committed-Version") != "7" || w.Header().Get("X-Umir-Runtime-Epoch") != epoch {
		t.Fatalf("successful preparation lost: %d %v %s", w.Code, w.Header(), w.Body.String())
	}
	fail = true
	w = request()
	if w.Code != 503 || w.Header().Get("X-Umir-Committed-Version") != "7" || w.Header().Get("X-Umir-Invalidation-Required") != "true" || !strings.Contains(w.Body.String(), `"retryable":true`) {
		t.Fatalf("failed calculation lost committed preparation: %d %v %s", w.Code, w.Header(), w.Body.String())
	}
	claims["aud"] = []string{"umir-serving"}
	w = request()
	if w.Code != 401 || calls != 2 {
		t.Fatalf("missing Warehouse audience dispatched: status=%d calls=%d", w.Code, calls)
	}
}
