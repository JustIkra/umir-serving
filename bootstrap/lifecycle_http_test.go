package bootstrap

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/JustIkra/umir-serving/contract"
)

type s3LifecycleVerifier struct{}

func (s3LifecycleVerifier) Verify(_ context.Context, token string) (contract.RequestScope, error) {
	kind, subject, op := "service", "service:auth", "runtime.register"
	version := "1"
	if token == "close" {
		op = "runtime.close"
		version = "2"
	}
	if token == "user" {
		kind = "user"
		subject = tenantB
	}
	return contract.RequestScope{Kind: kind, Subject: subject, OrgID: orgA, TenantID: tenantA, Operation: op, Permissions: []string{op}, LifecycleVersion: version, RuntimeEpoch: epochA}, nil
}
func TestS3LifecycleHTTPRegistrationCloseAndReconciliation(t *testing.T) {
	r := s3Registry(t, &warehouseStub{})
	h, err := NewScopedHTTP(ScopedHTTPOptions{Registry: r, Verifier: s3LifecycleVerifier{}, GatewayKey: "gateway", AuthKey: "auth", TZDataVersion: "test-iana-2026", Now: time.Now, RequestTimeout: time.Second, MaxRequestBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path, key, token, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("X-Umir-Service-Key", key)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	status := request("GET", "/internal/serving/v2/runtime", "auth", "", "")
	if status.Code != 200 || !strings.Contains(status.Body.String(), `"ready":false`) {
		t.Fatalf("status=%d %s", status.Code, status.Body)
	}
	body := `{"operationID":"` + operationA + `","expectedEpoch":"` + epochA + `","lifecycleVersion":"1","settings":{"timeZone":"Europe/Moscow","settingsVersion":"1"}}`
	path := "/internal/serving/v2/r/" + tenantA + "/registry"
	for _, denied := range [][2]string{{"gateway", "register"}, {"auth", "user"}} {
		w := request("PUT", path, denied[0], denied[1], body)
		if w.Code != 403 && w.Code != 401 {
			t.Fatalf("unauthorized lifecycle=%d %s", w.Code, w.Body)
		}
	}
	w := request("PUT", path, "auth", "register", body)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"ready"`) {
		t.Fatalf("register=%d %s", w.Code, w.Body)
	}
	var readiness map[string]any
	if err = json.Unmarshal(w.Body.Bytes(), &readiness); err != nil {
		t.Fatal(err)
	}
	if readiness["restaurantID"] != tenantA || readiness["epoch"] != epochA || readiness["lifecycleVersion"] != "1" {
		t.Fatalf("readiness=%v", readiness)
	}
	snapshot := contract.RegistrySnapshot{Revision: "1", Complete: true, Entries: []contract.RegistryRestaurant{{OrgID: orgA, TenantID: tenantA, LifecycleVersion: "1", Lifecycle: "ready"}}}
	if err = r.Reconcile(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	status = request("GET", "/internal/serving/v2/runtime", "auth", "", "")
	if !strings.Contains(status.Body.String(), `"ready":true`) {
		t.Fatalf("reconciled=%s", status.Body)
	}
	snapshot.Complete = false
	if err = r.Reconcile(context.Background(), snapshot); err == nil {
		t.Fatal("partial registry accepted")
	}
	if _, _, err = r.Resolve(context.Background(), s3Scope(tenantA)); err != nil {
		t.Fatalf("partial registry removed neighbor: %v", err)
	}
	closeBody := `{"operationID":"` + operationA + `","expectedEpoch":"` + epochA + `","lifecycleVersion":"2"}`
	for range 2 {
		w = request("DELETE", path, "auth", "close", closeBody)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"closed"`) {
			t.Fatalf("close=%d %s", w.Code, w.Body)
		}
	}
	deleted := contract.RegistryRestaurant{OrgID: orgA, TenantID: tenantA, LifecycleVersion: "2", Lifecycle: "deleting"}
	snapshot = contract.RegistrySnapshot{Revision: "2", Complete: true, Entries: []contract.RegistryRestaurant{deleted}, Tombstones: []contract.RegistryRestaurant{deleted}}
	if err = r.Reconcile(context.Background(), snapshot); err != nil {
		t.Fatalf("deleting entry/tombstone reconciliation=%v", err)
	}
	snapshot.Entries[0].Lifecycle = "invalid"
	if err = r.Reconcile(context.Background(), snapshot); err == nil {
		t.Fatal("unknown lifecycle reconciled")
	}
	w = request("PUT", path, "auth", "register", body)
	if w.Code != 410 {
		t.Fatalf("recreated=%d %s", w.Code, w.Body)
	}
}
