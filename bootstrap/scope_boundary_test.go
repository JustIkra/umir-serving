package bootstrap_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/JustIkra/umir-serving/bootstrap"
	"github.com/JustIkra/umir-serving/contract"
)

type browserVerifierProbe struct{ calls int }

func (v *browserVerifierProbe) Verify(context.Context, string) (contract.RequestScope, error) {
	v.calls++
	return contract.RequestScope{}, contract.ErrInvalidArgument
}

func TestS1BrowserIdentityHeadersCannotReachBusiness(t *testing.T) {
	calls := 0
	registry, err := bootstrap.NewRegistry(bootstrap.RegistryOptions{Epoch: "44444444-4444-4444-8444-444444444444", Now: time.Now, NewID: func() string { return "p" }, WarehouseFactory: func(context.Context, string) (bootstrap.WarehousePorts, error) {
		calls++
		return bootstrap.WarehousePorts{}, contract.ErrInvalidArgument
	}})
	if err != nil {
		t.Fatal(err)
	}
	verifier := &browserVerifierProbe{}
	handler, err := bootstrap.NewScopedHTTP(bootstrap.ScopedHTTPOptions{Registry: registry, Verifier: verifier, GatewayKey: "test-gateway", RequestTimeout: time.Second, MaxRequestBytes: 1024, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/internal/serving/v2/r/33333333-3333-4333-8333-333333333333/menu", nil)
	request.Header.Set("X-Umir-Tenant-ID", "33333333-3333-4333-8333-333333333333")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || calls != 0 || verifier.calls != 0 {
		t.Fatalf("browser identity dispatched: status=%d factory=%d verifier=%d %s", response.Code, calls, verifier.calls, response.Body.String())
	}
}

func TestS1LegacyCompositionPreservesSelectedHeaderBehavior(t *testing.T) {
	calls := 0
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		writeTestBody(t, w, `{"evaluatedAt":"2026-10-13T12:00:00+03:00","stateVersion":"1","items":[]}`)
	}))
	defer peer.Close()
	_, handler, err := bootstrap.NewHTTP(bootstrap.HTTPOptions{WarehouseInternalURL: peer.URL, WarehouseTimeout: time.Second, RequestTimeout: time.Second, MaxRequestBytes: 1024, Now: time.Now, NewID: func() string { return "p1" }})
	if err != nil {
		t.Fatal(err)
	}
	defer handler.CloseIdleConnections()
	request := httptest.NewRequest(http.MethodGet, "/products", nil)
	request.Header.Set("Cookie", "legacy-client=test")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || calls != 1 {
		t.Fatalf("selected legacy behavior changed: %d calls=%d %s", response.Code, calls, response.Body.String())
	}
}
