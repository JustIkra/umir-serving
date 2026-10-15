package bootstrap_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/JustIkra/umir-serving/bootstrap"
	"github.com/JustIkra/umir-serving/contract"
)

type s2RefusalVerifier struct{ scope contract.RequestScope }

func (v s2RefusalVerifier) Verify(context.Context, string) (contract.RequestScope, error) {
	return v.scope, nil
}

func TestS2RefusalAcrossScopedComposition(t *testing.T) {
	const tenant = "33333333-3333-4333-8333-333333333333"
	const org = "22222222-2222-4222-8222-222222222222"
	const epoch = "44444444-4444-4444-8444-444444444444"
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.FixedZone("MSK", 10800))
	scope := contract.RequestScope{OrgID: org, TenantID: tenant, UserID: "66666666-6666-4666-8666-666666666666", Operation: "menu", Permissions: []string{"menu.read", "consumer.menu.evaluate"}, TimeZone: "Europe/Moscow", SettingsVersion: "1", LifecycleVersion: "1"}.WithToken("review-token")
	verifier := s2RefusalVerifier{scope}
	for code, want := range map[string]int{"InvalidRequest": 400, "ServiceUnavailable": 503, "LimitExceeded": 429, "ProductConflict": 409, "DishConflict": 409} {
		t.Run(code, func(t *testing.T) {
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/internal/warehouse/v2/r/"+tenant+"/menu-evaluation" {
					t.Errorf("unexpected path %s", r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(want)
				if _, err := fmt.Fprintf(w, `{"code":%q,"message":"Warehouse refusal","outcome":"unavailable","retryable":false}`, code); err != nil {
					t.Error(err)
				}
			}))
			defer peer.Close()
			registry, err := bootstrap.NewRegistry(bootstrap.RegistryOptions{Epoch: epoch, Now: func() time.Time { return now }, NewID: func() string { return "proposal" }, WarehouseFactory: func(ctx context.Context, selected string) (bootstrap.WarehousePorts, error) {
				return bootstrap.NewScopedWarehouseHTTPPorts(bootstrap.ScopedWarehouseHTTPOptions{WarehouseInternalURL: peer.URL, WarehouseTimeout: time.Second, HTTPClient: peer.Client(), TenantID: selected, Epoch: epoch, ServiceKey: "review-serving-key", Verifier: verifier})
			}})
			if err != nil {
				t.Fatal(err)
			}
			if err := registry.Register(context.Background(), contract.RestaurantRegistration{OrgID: org, TenantID: tenant, OperationID: epoch, ExpectedEpoch: epoch, LifecycleVersion: "1"}); err != nil {
				t.Fatal(err)
			}
			handler, err := bootstrap.NewScopedHTTP(bootstrap.ScopedHTTPOptions{Registry: registry, Verifier: verifier, GatewayKey: "review-gateway-key", RequestTimeout: time.Second, MaxRequestBytes: 1024, Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest("GET", "/internal/serving/v2/r/"+tenant+"/menu", nil)
			r.Header.Set("Authorization", "Bearer review-token")
			r.Header.Set("X-Umir-Service-Key", "review-gateway-key")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != want {
				t.Fatalf("real scoped composition received Warehouse %d/%s, returned %d: %s", want, code, w.Code, w.Body.String())
			}
		})
	}
}
