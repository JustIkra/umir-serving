package bootstrap_test

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JustIkra/umir-serving/bootstrap"
	"github.com/JustIkra/umir-serving/contract"
)

func TestFINRecommendationsThroughSignedScopedComposition(t *testing.T) {
	const tenant = "33333333-3333-4333-8333-333333333333"
	const org = "22222222-2222-4222-8222-222222222222"
	const epoch = "44444444-4444-4444-8444-444444444444"
	at := time.Date(2026, 10, 15, 12, 0, 0, 0, time.FixedZone("MSK", 10800))
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := bootstrap.NewJWTVerifier(bootstrap.JWTOptions{Now: func() time.Time { return at }, Keys: func(context.Context, string) (ed25519.PublicKey, error) { return public, nil }})
	if err != nil {
		t.Fatal(err)
	}
	encode := func(value any) string {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(data)
	}
	header := encode(map[string]string{"alg": "EdDSA", "typ": "JWT", "kid": "test-key"})
	claims := map[string]any{"iss": "urn:umir:auth", "aud": []string{"umir-serving", "umir-warehouse"}, "iat": at.Unix(), "nbf": at.Unix(), "exp": at.Unix() + 60, "jti": epoch, "operation": "recommendations", "permissions": []string{"recommendations.read", "consumer.recommendations.read"}, "kind": "user", "sub": "66666666-6666-4666-8666-666666666666", "sid": "77777777-7777-4777-8777-777777777777", "session_version": "1", "credential_version": "1", "access_version": "1", "org_id": org, "tenant_id": tenant, "time_zone": "Europe/Moscow", "settings_version": "1", "lifecycle_version": "1", "azp": "umir-gateway"}
	unsigned := header + "." + encode(claims)
	token := unsigned + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, []byte(unsigned)))
	body, err := os.ReadFile("../internal/adapters/warehousehttp/testdata/fin1-recommendation-inputs.json")
	if err != nil {
		t.Fatal(err)
	}
	var fail atomic.Bool
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/warehouse/v2/r/"+tenant+"/recommendation-inputs" || r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("X-Umir-Service-Key") != "serving-key" {
			t.Error("incorrect consumer operation/trust")
		}
		w.Header().Set("Content-Type", "application/json")
		if fail.Load() {
			w.WriteHeader(503)
			writeTestBody(t, w, `{"code":"LimitExceeded","message":"Capacity exceeded.","outcome":"unavailable","retryable":false}`)
			return
		}
		if _, err := w.Write(body); err != nil {
			t.Error(err)
		}
	}))
	defer peer.Close()
	registry, err := bootstrap.NewRegistry(bootstrap.RegistryOptions{Epoch: epoch, Now: func() time.Time { return at }, NewID: func() string { return epoch }, WarehouseFactory: func(ctx context.Context, selected string) (bootstrap.WarehousePorts, error) {
		return bootstrap.NewScopedWarehouseHTTPPorts(bootstrap.ScopedWarehouseHTTPOptions{WarehouseInternalURL: peer.URL, WarehouseTimeout: time.Second, HTTPClient: peer.Client(), TenantID: selected, Epoch: epoch, ServiceKey: "serving-key", Verifier: verifier})
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(context.Background(), contract.RestaurantRegistration{OrgID: org, TenantID: tenant, OperationID: epoch, ExpectedEpoch: epoch, LifecycleVersion: "1"}); err != nil {
		t.Fatal(err)
	}
	handler, err := bootstrap.NewScopedHTTP(bootstrap.ScopedHTTPOptions{Registry: registry, Verifier: verifier, GatewayKey: "gateway-key", RequestTimeout: time.Second, MaxRequestBytes: 1024, Now: func() time.Time { return at }})
	if err != nil {
		t.Fatal(err)
	}
	request := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/internal/serving/v2/r/"+tenant+"/recommendations", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("X-Umir-Service-Key", "gateway-key")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	w := request()
	var result struct {
		Context struct{ TenantID, RuntimeEpoch, WarehouseEpoch, StateVersion string }
		Result  struct {
			PolicyVersion, StateVersion string
			Items                       []struct {
				DishID, Score, DemandBasis string
				Reasons                    []struct{ QuantityPerPortion, RiskQuantity, TargetPortions string }
			}
		}
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Context.TenantID != tenant || result.Context.RuntimeEpoch != epoch || result.Context.WarehouseEpoch != epoch || result.Context.StateVersion != "42" || result.Result.StateVersion != "42" || result.Result.PolicyVersion != "FIN1" || len(result.Result.Items) != 2 || result.Result.Items[0].Score != "0.666667" || result.Result.Items[1].Score != "0.133333" || result.Result.Items[1].Reasons[0].QuantityPerPortion != "5000" || w.Header().Get("X-Umir-Invalidation-Required") != "" {
		t.Fatalf("FIN1 composed response %d %s", w.Code, w.Body)
	}
	fail.Store(true)
	w = request()
	var failure struct{ Code, Outcome string }
	if w.Code != 503 || json.Unmarshal(w.Body.Bytes(), &failure) != nil || failure.Code != "CalculationUnavailable" || failure.Outcome != "unavailable" {
		t.Fatalf("capacity became empty success or wrong refusal: %d %s", w.Code, w.Body)
	}
}
