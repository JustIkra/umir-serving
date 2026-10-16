package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JustIkra/umir-serving/contract"
	"github.com/JustIkra/umir-serving/internal/adapters/warehousehttp"
)

// memoryLog is a thread-safe slog.Handler that keeps every record as flat
// string fields, so tests can look for events and scan every written value.
type memoryLog struct {
	shared *memoryLogShared
	attrs  []slog.Attr
	groups []string
}

type memoryLogShared struct {
	mu      sync.Mutex
	records []map[string]string
}

func newMemoryLog() *memoryLog { return &memoryLog{shared: &memoryLogShared{}} }

func (m *memoryLog) Enabled(context.Context, slog.Level) bool { return true }

func (m *memoryLog) Handle(_ context.Context, record slog.Record) error {
	fields := map[string]string{"level": record.Level.String(), "msg": record.Message}
	prefix := strings.Join(m.groups, ".")
	add := func(attr slog.Attr) {
		key := attr.Key
		if prefix != "" {
			key = prefix + "." + key
		}
		fields[key] = attr.Value.Resolve().String()
	}
	for _, attr := range m.attrs {
		add(attr)
	}
	record.Attrs(func(attr slog.Attr) bool {
		add(attr)
		return true
	})
	m.shared.mu.Lock()
	m.shared.records = append(m.shared.records, fields)
	m.shared.mu.Unlock()
	return nil
}

func (m *memoryLog) WithAttrs(attrs []slog.Attr) slog.Handler {
	copy := *m
	copy.attrs = append(append([]slog.Attr(nil), m.attrs...), attrs...)
	return &copy
}

func (m *memoryLog) WithGroup(name string) slog.Handler {
	copy := *m
	copy.groups = append(append([]string(nil), m.groups...), name)
	return &copy
}

func (m *memoryLog) all() []map[string]string {
	m.shared.mu.Lock()
	defer m.shared.mu.Unlock()
	return append([]map[string]string(nil), m.shared.records...)
}

func (m *memoryLog) events(event string) []map[string]string {
	var found []map[string]string
	for _, record := range m.all() {
		if record["event"] == event {
			found = append(found, record)
		}
	}
	return found
}

func (m *memoryLog) text() string {
	var lines []string
	for _, record := range m.all() {
		for key, value := range record {
			lines = append(lines, key+"="+value)
		}
	}
	return strings.Join(lines, "\n")
}

func (m *memoryLog) one(t *testing.T, event string) map[string]string {
	t.Helper()
	found := m.events(event)
	if len(found) != 1 {
		t.Fatalf("%s: %d records, want 1; all=%v", event, len(found), m.all())
	}
	return found[0]
}

func wantFields(t *testing.T, record map[string]string, want map[string]string) {
	t.Helper()
	for key, value := range want {
		if record[key] != value {
			t.Fatalf("%s: %s=%q, want %q; record=%v", record["event"], key, record[key], value, record)
		}
	}
}

const (
	canaryTokenSuffix  = "-CANARY-TOKEN-7c1e"
	canaryBodyDish     = "dish-CANARY-BODY-93ab"
	canaryGatewayKey   = "CANARY-GATEWAY-KEY-55d0"
	canaryWarehouseKey = "CANARY-WAREHOUSE-KEY-0b6f"
)

// canaryVerifier accepts the synthetic tokens of s3HTTPVerifier with a canary
// suffix, so every request carries a value that must never reach a log.
type canaryVerifier struct{}

func (canaryVerifier) Verify(ctx context.Context, token string) (contract.RequestScope, error) {
	base, ok := strings.CutSuffix(token, canaryTokenSuffix)
	if !ok {
		return contract.RequestScope{}, fmt.Errorf("invalid token")
	}
	scope, err := s3HTTPVerifier{}.Verify(ctx, base)
	return scope.WithToken(token), err
}

// logPeer is a Warehouse that echoes the settings of the presented token, like
// the real scoped writer.
type logPeer struct {
	t             *testing.T
	mu            sync.Mutex
	attemptState  string
	commands      string // "none", "found", "error"
	badEvaluation bool
	commits       int
	gate          chan struct{} // when set, /order-commits waits for it
	arrived       chan struct{} // receives one value when a commit starts waiting
}

func (p *logPeer) set(attemptState, commands string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.attemptState, p.commands = attemptState, commands
}

func (p *logPeer) encode(w http.ResponseWriter, value any) {
	if err := json.NewEncoder(w).Encode(value); err != nil {
		p.t.Error(err)
	}
}

func (p *logPeer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	const at = "2026-10-13T12:00:00+03:00"
	if p.gate != nil && strings.HasSuffix(r.URL.Path, "/order-commits") {
		p.arrived <- struct{}{}
		select {
		case <-p.gate:
		case <-r.Context().Done():
			return
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	scope, err := canaryVerifier{}.Verify(r.Context(), token)
	if err != nil {
		w.WriteHeader(401)
		return
	}
	tenant := tenantA
	if p.badEvaluation {
		tenant = tenantB
	}
	contextWire := map[string]any{"orgID": orgA, "tenantID": tenant, "runtimeEpoch": peerEpoch, "settings": map[string]any{"timeZone": scope.TimeZone, "settingsVersion": scope.SettingsVersion}, "evaluatedAt": at, "stateVersion": "9007199254740993"}
	orderWire := map[string]any{"orderID": "order", "proposalID": "proposal", "placedAt": at, "acceptedPositions": []any{map[string]any{"lineNo": "1", "dishID": canaryBodyDish, "portions": "2"}}, "allocations": []any{map[string]any{"productID": "p", "batchID": "b", "quantity": "2"}}}
	var result any
	switch strings.TrimPrefix(r.URL.Path, "/internal/warehouse/v2/r/"+tenantA) {
	case "/order-evaluations":
		result = map[string]any{"evaluatedAt": at, "stateVersion": "9007199254740993", "requestedPositions": []any{map[string]any{"lineNo": "1", "dishID": canaryBodyDish, "portions": "2"}}, "acceptedPositions": []any{map[string]any{"lineNo": "1", "dishID": canaryBodyDish, "portions": "2"}}, "changes": []any{}}
	case "/commands":
		switch p.commands {
		case "error":
			w.WriteHeader(502)
			p.encode(w, map[string]any{"code": "UpstreamUnavailable", "message": "lookup lost", "outcome": "unknown", "retryable": true})
			return
		case "found":
			result = map[string]any{"found": true, "record": map[string]any{"commandID": "command", "commandType": "ConfirmOrder", "commandFingerprint": confirmFingerprintForTest("proposal"), "result": map[string]any{"kind": "OrderPlaced", "order": orderWire}}}
		default:
			result = map[string]any{"found": false}
		}
	case "/order-attempts":
		result = map[string]any{"state": p.attemptState}
	case "/order-commits":
		p.commits++
		w.WriteHeader(502)
		p.encode(w, map[string]any{"code": "UpstreamUnavailable", "message": "lost reply", "outcome": "unknown", "retryable": true})
		return
	default:
		w.WriteHeader(500)
		return
	}
	p.encode(w, map[string]any{"context": contextWire, "result": result})
}

type logHarness struct {
	t        *testing.T
	peer     *logPeer
	peerURL  string
	logs     *memoryLog
	registry *Registry
	handler  http.Handler
}

func newLogHarness(t *testing.T) *logHarness {
	t.Helper()
	peer := &logPeer{t: t, attemptState: "Unseen", commands: "none"}
	server := httptest.NewServer(peer)
	t.Cleanup(server.Close)
	logs := newMemoryLog()
	logger := slog.New(logs)
	registry, err := NewRegistry(RegistryOptions{Epoch: epochA, Now: time.Now, NewID: func() string { return "proposal" }, Logger: logger, WarehouseFactory: func(context.Context, string) (WarehousePorts, error) {
		c, e := warehousehttp.NewScopedClient(server.URL, server.Client(), time.Second, warehousehttp.ScopedOptions{TenantID: tenantA, Epoch: peerEpoch, ServiceKey: canaryWarehouseKey, Verifier: canaryVerifier{}, Logger: logger})
		return WarehousePorts{Orders: c, Attempts: c, Catalog: c, Menu: c, Epoch: peerEpoch}, e
	}})
	if err != nil {
		t.Fatal(err)
	}
	registration := s3Registration(tenantA)
	registration.TimeZone, registration.SettingsVersion = "Europe/Moscow", "1"
	if err = registry.Register(context.Background(), registration); err != nil {
		t.Fatal(err)
	}
	handler, err := NewScopedHTTP(ScopedHTTPOptions{Registry: registry, Verifier: canaryVerifier{}, GatewayKey: canaryGatewayKey, Now: time.Now, RequestTimeout: time.Second, MaxRequestBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	return &logHarness{t: t, peer: peer, peerURL: server.URL, logs: logs, registry: registry, handler: handler}
}

func (h *logHarness) build(method, path, token, body string) *http.Request {
	req := httptest.NewRequest(method, "/internal/serving/v2/r/"+tenantA+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token+canaryTokenSuffix)
	req.Header.Set("X-Umir-Service-Key", canaryGatewayKey)
	req.Header.Set("X-Umir-Runtime-Epoch", epochA)
	req.Header.Set("Content-Type", "application/json")
	return req
}

func (h *logHarness) request(method, path, token, body string) *httptest.ResponseRecorder {
	h.t.Helper()
	out := httptest.NewRecorder()
	h.handler.ServeHTTP(out, h.build(method, path, token, body))
	return out
}

func (h *logHarness) prepare() {
	h.t.Helper()
	if p := h.request("POST", "/order-proposals", "prepare", `{"positions":[{"dishID":"`+canaryBodyDish+`","portions":"2"}]}`); p.Code != 200 {
		h.t.Fatalf("prepare=%d %s", p.Code, p.Body)
	}
}

const confirmBody = `{"commandID":"command","proposalID":"proposal"}`
const resolvePath = "/order-confirmations/resolve?commandID=command&proposalID=proposal&runtimeEpoch=" + epochA

func TestLogsUnknownOutcomeAndPendingLifecycle(t *testing.T) {
	h := newLogHarness(t)
	h.prepare()

	// The commit reply is lost: the attempt is pending and the outcome unknown.
	if first := h.request("POST", "/order-confirmations", "confirm", confirmBody); first.Code != 502 {
		t.Fatalf("first=%d %s", first.Code, first.Body)
	}
	started := h.logs.one(t, "confirmation.pending.started")
	wantFields(t, started, map[string]string{"level": "INFO", "operation": "confirm", "command_id": "command", "proposal_id": "proposal", "tenant_id": tenantA, "warehouse_epoch": peerEpoch, "runtime_epoch": epochA})
	unknown := h.logs.one(t, "confirmation.unknown")
	wantFields(t, unknown, map[string]string{"level": "ERROR", "operation": "confirm", "command_id": "command", "proposal_id": "proposal", "tenant_id": tenantA, "outcome": "unknown", "warehouse_epoch": peerEpoch})
	if unknown["error_code"] == "" {
		t.Fatalf("unknown outcome has no error code: %v", unknown)
	}

	// The owner has no finished attempt and /commands fails: still unknown, still pending.
	h.peer.set("Pending", "error")
	if got := h.request("GET", resolvePath, "resolve", ""); got.Code != 502 {
		t.Fatalf("resolve=%d %s", got.Code, got.Body)
	}
	if n := len(h.logs.events("confirmation.unknown")); n != 2 {
		t.Fatalf("unknown records=%d, want 2", n)
	}
	second := h.logs.events("confirmation.unknown")[1]
	wantFields(t, second, map[string]string{"operation": "resolve", "command_id": "command", "proposal_id": "proposal"})

	// Nothing recorded yet: a pending answer is neither started nor resolved again.
	h.peer.set("Pending", "none")
	if got := h.request("GET", resolvePath, "resolve", ""); got.Code != 200 || !strings.Contains(got.Body.String(), `"kind":"ConfirmationPending"`) {
		t.Fatalf("pending resolve=%d %s", got.Code, got.Body)
	}
	if len(h.logs.events("confirmation.pending.started")) != 1 || len(h.logs.events("confirmation.pending.resolved")) != 0 {
		t.Fatalf("pending answer logged lifecycle events: %v", h.logs.all())
	}

	// The owner recorded the order: the retained pending attempt resolves.
	h.peer.set("Pending", "found")
	if got := h.request("GET", resolvePath, "resolve", ""); got.Code != 200 || !strings.Contains(got.Body.String(), `"kind":"OrderConfirmed"`) {
		t.Fatalf("found resolve=%d %s", got.Code, got.Body)
	}
	resolved := h.logs.one(t, "confirmation.pending.resolved")
	wantFields(t, resolved, map[string]string{"level": "INFO", "operation": "resolve", "command_id": "command", "proposal_id": "proposal", "tenant_id": tenantA, "result": "OrderConfirmed", "warehouse_epoch": peerEpoch})
	if h.peer.commits != 1 {
		t.Fatalf("commits=%d, want 1", h.peer.commits)
	}
	if n := len(h.logs.events("warehouse.protocol_error")); n != 0 {
		t.Fatalf("unexpected protocol errors: %v", h.logs.all())
	}
}

func TestLogsResolvedAfterSettingsChange(t *testing.T) {
	h := newLogHarness(t)
	h.prepare()
	if first := h.request("POST", "/order-confirmations", "confirm", confirmBody); first.Code != 502 {
		t.Fatalf("first=%d %s", first.Code, first.Body)
	}
	changed := s3Registration(tenantA)
	changed.TimeZone, changed.SettingsVersion = "Europe/Berlin", "2"
	if err := h.registry.Register(context.Background(), changed); err != nil {
		t.Fatal(err)
	}
	h.peer.set("Unseen", "found")
	if got := h.request("POST", "/order-confirmations", "retry", confirmBody); got.Code != 200 || !strings.Contains(got.Body.String(), `"kind":"OrderConfirmed"`) {
		t.Fatalf("retry=%d %s", got.Code, got.Body)
	}
	resolved := h.logs.one(t, "confirmation.pending.resolved")
	wantFields(t, resolved, map[string]string{"operation": "confirm", "command_id": "command", "result": "OrderConfirmed", "settings_version": "1"})
	if n := len(h.logs.events("warehouse.protocol_error")); n != 0 {
		t.Fatalf("settings change was reported as a protocol error: %v", h.logs.all())
	}
}

func TestLogsWarehouseProtocolError(t *testing.T) {
	h := newLogHarness(t)
	h.peer.badEvaluation = true
	got := h.request("POST", "/order-proposals", "prepare", `{"positions":[{"dishID":"`+canaryBodyDish+`","portions":"2"}]}`)
	if got.Code == 200 {
		t.Fatalf("prepare accepted a response for another restaurant: %s", got.Body)
	}
	record := h.logs.one(t, "warehouse.protocol_error")
	wantFields(t, record, map[string]string{"level": "ERROR", "tenant_id": tenantA, "operation": "prepareOrder", "warehouse_epoch": peerEpoch})
}

func TestLogsReconcileReasons(t *testing.T) {
	logs := newMemoryLog()
	registry, err := NewRegistry(RegistryOptions{Epoch: epochA, Now: time.Now, NewID: func() string { return "p" }, Logger: slog.New(logs), WarehouseFactory: func(context.Context, string) (WarehousePorts, error) {
		w := &warehouseStub{}
		return WarehousePorts{Orders: w, Catalog: w, Menu: w, Epoch: peerEpoch}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err = registry.Register(t.Context(), contract.RestaurantRegistration{OrgID: orgA, TenantID: tenantA, OperationID: operationA, ExpectedEpoch: epochA, LifecycleVersion: "1", TimeZone: "Europe/Moscow", SettingsVersion: "1"}); err != nil {
		t.Fatal(err)
	}
	entry := func(tenant, lifecycle string) contract.RegistryRestaurant {
		return contract.RegistryRestaurant{OrgID: orgA, TenantID: tenant, LifecycleVersion: "1", Lifecycle: lifecycle}
	}

	// A registering restaurant that is not installed yet is skipped, with a reason, once.
	snapshot := contract.RegistrySnapshot{Revision: "1", Complete: true, Entries: []contract.RegistryRestaurant{entry(tenantA, "ready"), entry(tenantB, "registering")}}
	for revision := 1; revision <= 3; revision++ {
		snapshot.Revision = fmt.Sprint(revision)
		if err = registry.Reconcile(t.Context(), snapshot); err != nil {
			t.Fatal(err)
		}
	}
	skipped := logs.one(t, "reconcile.failed")
	wantFields(t, skipped, map[string]string{"level": "WARN", "reason": "registering_not_installed", "tenant_id": tenantB, "org_id": orgA, "lifecycle_version": "1", "revision": "1"})

	// Failures carry a reason and the error code.
	cases := []struct {
		name     string
		snapshot contract.RegistrySnapshot
		reason   string
	}{
		{"incomplete", contract.RegistrySnapshot{Revision: "9", Complete: false}, "snapshot_invalid"},
		{"rollback", contract.RegistrySnapshot{Revision: "2", Complete: true, Entries: []contract.RegistryRestaurant{entry(tenantA, "ready")}}, "revision_rollback"},
		{"ready not installed", contract.RegistrySnapshot{Revision: "9", Complete: true, Entries: []contract.RegistryRestaurant{entry(tenantA, "ready"), entry(tenantB, "ready")}}, "ready_not_installed"},
		{"local restaurant missing", contract.RegistrySnapshot{Revision: "9", Complete: true}, "local_missing_from_snapshot"},
		{"invalid entry", contract.RegistrySnapshot{Revision: "9", Complete: true, Entries: []contract.RegistryRestaurant{entry(tenantA, "ready"), {OrgID: "bad", TenantID: tenantB, LifecycleVersion: "1", Lifecycle: "ready"}}}, "invalid_entry"},
	}
	for _, tc := range cases {
		before := len(logs.events("reconcile.failed"))
		if err := registry.Reconcile(t.Context(), tc.snapshot); err == nil {
			t.Fatalf("%s: reconciliation succeeded", tc.name)
		}
		got := logs.events("reconcile.failed")
		if len(got) != before+1 {
			t.Fatalf("%s: reconcile.failed records=%d, want %d", tc.name, len(got), before+1)
		}
		wantFields(t, got[len(got)-1], map[string]string{"level": "ERROR", "reason": tc.reason})
	}

	// The same restaurant under another organization is a tenant mismatch.
	mismatch := contract.RegistrySnapshot{Revision: "9", Complete: true, Entries: []contract.RegistryRestaurant{{OrgID: tenantB, TenantID: tenantA, LifecycleVersion: "1", Lifecycle: "ready"}}}
	if err = registry.Reconcile(t.Context(), mismatch); err == nil {
		t.Fatal("reconciliation accepted another organization")
	}
	record := logs.one(t, "registry.tenant_mismatch")
	wantFields(t, record, map[string]string{"level": "WARN", "where": "reconcile", "reason": "org_mismatch", "tenant_id": tenantA, "registered_org_id": orgA, "presented_org_id": tenantB})
	last := logs.events("reconcile.failed")
	wantFields(t, last[len(last)-1], map[string]string{"reason": "tenant_mismatch"})

	// A scope with another organization or lifecycle version is refused and recorded.
	_, _, err = registry.Resolve(t.Context(), contract.RequestScope{OrgID: tenantB, TenantID: tenantA, LifecycleVersion: "1"})
	s3Code(t, err, "Forbidden", "unavailable")
	resolve := logs.events("registry.tenant_mismatch")
	if len(resolve) != 2 {
		t.Fatalf("tenant_mismatch records=%d, want 2", len(resolve))
	}
	wantFields(t, resolve[1], map[string]string{"where": "resolve", "reason": "org_mismatch", "tenant_id": tenantA, "registered_org_id": orgA, "presented_org_id": tenantB})
	_, _, err = registry.Resolve(t.Context(), contract.RequestScope{OrgID: orgA, TenantID: tenantA, LifecycleVersion: "7"})
	s3Code(t, err, "Forbidden", "unavailable")
	versions := logs.events("registry.tenant_mismatch")
	wantFields(t, versions[2], map[string]string{"where": "resolve", "reason": "lifecycle_mismatch", "registered_lifecycle_version": "1", "presented_lifecycle_version": "7"})
}

func TestLogsProcessReconcileFailureWhenAuthIsUnavailable(t *testing.T) {
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer auth.Close()
	logs := newMemoryLog()
	handler, err := NewProcessHTTP(ProcessHTTPOptions{
		HTTPOptions:          HTTPOptions{WarehouseInternalURL: "http://127.0.0.1:1", WarehouseTimeout: time.Second, RequestTimeout: time.Second, MaxRequestBytes: 1 << 20, Now: time.Now, NewID: func() string { return "id" }, Logger: slog.New(logs)},
		AuthInternalURL:      auth.URL,
		AuthTimeout:          time.Second,
		ServingAuthKey:       "canary-serving-auth-key",
		AuthRuntimeKey:       "canary-auth-runtime-key",
		GatewayServingKey:    "canary-gateway-serving-key",
		ServingWarehouseKey:  "canary-serving-warehouse-key",
		TZDataVersion:        "2026a",
		Epoch:                epochA,
		RegistryPollInterval: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer handler.CloseIdleConnections()
	deadline := time.Now().Add(5 * time.Second)
	for len(logs.events("reconcile.failed")) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("no reconcile.failed record: %v", logs.all())
		}
		time.Sleep(10 * time.Millisecond)
	}
	record := logs.events("reconcile.failed")[0]
	wantFields(t, record, map[string]string{"level": "WARN", "reason": "auth_registry_unavailable"})
	for _, secret := range []string{"canary-serving-auth-key", "canary-auth-runtime-key", "canary-gateway-serving-key", "canary-serving-warehouse-key", auth.URL} {
		if strings.Contains(logs.text(), secret) {
			t.Fatalf("log contains %q: %v", secret, logs.all())
		}
	}
}

type failingResponseWriter struct {
	header http.Header
	status int
}

func (w *failingResponseWriter) Header() http.Header    { return w.header }
func (w *failingResponseWriter) WriteHeader(status int) { w.status = status }
func (w *failingResponseWriter) Write([]byte) (int, error) {
	return 0, errors.New("write tcp 203.0.113.9:4444->198.51.100.7:5555: broken pipe CANARY-ADDRESS")
}

func TestLogsResponseWriteFailureWithoutAddress(t *testing.T) {
	h := newLogHarness(t)
	writer := &failingResponseWriter{header: make(http.Header)}
	h.handler.ServeHTTP(writer, h.build("POST", "/order-proposals", "prepare", `{"positions":[{"dishID":"`+canaryBodyDish+`","portions":"2"}]}`))
	record := h.logs.one(t, "http.response_failed")
	wantFields(t, record, map[string]string{"level": "WARN", "stage": "write_response"})
	if record["error_kind"] == "" {
		t.Fatalf("no error kind: %v", record)
	}
}

// TestLogsCarryNoTokensBodiesOrAddresses sends canary tokens, keys, bodies and
// addresses through every logged scenario and scans every attribute written.
func TestLogsCarryNoTokensBodiesOrAddresses(t *testing.T) {
	h := newLogHarness(t)
	h.prepare()
	h.request("POST", "/order-confirmations", "confirm", confirmBody)
	h.peer.set("Pending", "error")
	h.request("GET", resolvePath, "resolve", "")
	h.peer.set("Pending", "found")
	h.request("GET", resolvePath, "resolve", "")
	h.request("POST", "/order-proposals", "prepare", `{"positions":[{"dishID":"`+canaryBodyDish+`","portions":"2"}]`)
	h.peer.badEvaluation = true
	h.request("POST", "/order-proposals", "prepare", `{"positions":[{"dishID":"`+canaryBodyDish+`","portions":"2"}]}`)
	h.peer.badEvaluation = false
	h.handler.ServeHTTP(&failingResponseWriter{header: make(http.Header)}, h.build("POST", "/order-proposals", "prepare", `{"positions":[{"dishID":"`+canaryBodyDish+`","portions":"2"}]}`))

	if len(h.logs.all()) < 5 {
		t.Fatalf("scenario wrote too few records to prove anything: %v", h.logs.all())
	}
	text := h.logs.text()
	for _, canary := range []string{canaryTokenSuffix, "CANARY-TOKEN", canaryBodyDish, "CANARY-BODY", canaryGatewayKey, canaryWarehouseKey, "CANARY-ADDRESS", "203.0.113.9", "198.51.100.7", "Authorization", "Bearer", "X-Umir-Service-Key", h.peerURL, strings.TrimPrefix(h.peerURL, "http://"), "192.0.2.1"} {
		if strings.Contains(text, canary) {
			t.Fatalf("log contains %q:\n%s", canary, text)
		}
	}
}

func TestNewLoggerWritesCommonFields(t *testing.T) {
	var out strings.Builder
	logger := NewLogger(&out, "instance-1", "abc123", slog.LevelInfo)
	logger.Info("confirmation.unknown", slog.String("event", "confirmation.unknown"), slog.String("command_id", "command"))
	var record map[string]any
	if err := json.Unmarshal([]byte(out.String()), &record); err != nil {
		t.Fatalf("not JSON: %v: %q", err, out.String())
	}
	want := map[string]any{"level": "INFO", "msg": "confirmation.unknown", "event": "confirmation.unknown", "log_schema_version": float64(1), "service": "umir-serving", "version": "abc123", "instance_id": "instance-1", "command_id": "command"}
	for key, value := range want {
		if record[key] != value {
			t.Fatalf("%s=%v, want %v; record=%v", key, record[key], value, record)
		}
	}
	if timestamp, ok := record["time"].(string); !ok || !strings.HasSuffix(timestamp, "+03:00") {
		t.Fatalf("time=%v, want +03:00 offset", record["time"])
	}
}

func TestLogsNoResolvedForIdentityConflict(t *testing.T) {
	h := newLogHarness(t)
	h.prepare()
	if first := h.request("POST", "/order-confirmations", "confirm", confirmBody); first.Code != 502 {
		t.Fatalf("first=%d %s", first.Code, first.Body)
	}
	h.peer.set("Pending", "found")
	other := "/order-confirmations/resolve?commandID=command&proposalID=another&runtimeEpoch=" + epochA
	if got := h.request("GET", other, "resolve", ""); got.Code == 200 {
		t.Fatalf("identity conflict answered %d %s", got.Code, got.Body)
	}
	if resolved := h.logs.events("confirmation.pending.resolved"); len(resolved) != 0 {
		t.Fatalf("identity conflict logged resolved: %v", resolved)
	}
	conflict := h.request("POST", "/order-confirmations", "retry", `{"commandID":"command","proposalID":"another"}`)
	if conflict.Code == 200 || len(h.logs.events("confirmation.pending.resolved")) != 0 {
		t.Fatalf("confirm conflict=%d logged=%v", conflict.Code, h.logs.events("confirmation.pending.resolved"))
	}
}

func TestLogsContextEndBeforeReservationIsNotAnErrorRecord(t *testing.T) {
	h := newLogHarness(t)
	h.prepare()
	h.peer.gate, h.peer.arrived = make(chan struct{}), make(chan struct{}, 1)
	var release sync.Once
	releaseGate := func() { release.Do(func() { close(h.peer.gate) }) }
	t.Cleanup(releaseGate) // runs before the peer server closes, so a failed assertion cannot hang it
	runtime, _, err := h.registry.Resolve(context.Background(), s3Scope(tenantA))
	if err != nil {
		t.Fatal(err)
	}
	scope, err := canaryVerifier{}.Verify(context.Background(), "confirm"+canaryTokenSuffix)
	if err != nil {
		t.Fatal(err)
	}
	request := contract.ConfirmOrderRequest{CommandID: "command", ProposalID: "proposal"}
	stuck := make(chan error, 1)
	go func() {
		_, confirmErr := runtime.ConfirmOrder(contract.WithRequestScope(context.Background(), scope), request)
		stuck <- confirmErr
	}()
	select {
	case <-h.peer.arrived:
	case <-time.After(2 * time.Second):
		t.Fatal("the first commit never reached the Warehouse")
	}
	waiting, cancel := context.WithTimeout(contract.WithRequestScope(context.Background(), scope), 50*time.Millisecond)
	defer cancel()
	if _, err = runtime.ConfirmOrder(waiting, request); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued confirm error=%v", err)
	}
	unknown := h.logs.events("confirmation.unknown")
	if len(unknown) != 1 {
		t.Fatalf("unknown records=%v", unknown)
	}
	wantFields(t, unknown[0], map[string]string{"level": "WARN", "error_code": "ContextDone", "cause": "deadline", "command_id": "command"})
	releaseGate()
	if confirmErr := <-stuck; confirmErr == nil {
		t.Fatal("stuck confirm succeeded")
	}
	var errorRecords int
	for _, record := range h.logs.events("confirmation.unknown") {
		if record["level"] == "ERROR" {
			errorRecords++
		}
	}
	if errorRecords != 1 {
		t.Fatalf("ERROR unknown records=%d, want the one lost commit: %v", errorRecords, h.logs.all())
	}
}

// blockingLog calls back into the registry while it writes a record, which
// deadlocks if the registry still holds its mutex.
type blockingLog struct {
	memoryLog
	registry func() *Registry
}

func (b *blockingLog) Handle(ctx context.Context, record slog.Record) error {
	b.registry().Status()
	return b.memoryLog.Handle(ctx, record)
}

func TestLogsAreWrittenOutsideRegistryLocks(t *testing.T) {
	var registry *Registry
	logs := &blockingLog{memoryLog: *newMemoryLog(), registry: func() *Registry { return registry }}
	var err error
	registry, err = NewRegistry(RegistryOptions{Epoch: epochA, Now: time.Now, NewID: func() string { return "p" }, Logger: slog.New(logs), WarehouseFactory: func(context.Context, string) (WarehousePorts, error) {
		w := &warehouseStub{}
		return WarehousePorts{Orders: w, Catalog: w, Menu: w, Epoch: peerEpoch}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan [2]error, 1)
	go func() {
		first := registry.Reconcile(context.Background(), contract.RegistrySnapshot{Revision: "1", Complete: true, Entries: []contract.RegistryRestaurant{{OrgID: orgA, TenantID: tenantB, LifecycleVersion: "1", Lifecycle: "registering"}}})
		second := registry.Reconcile(context.Background(), contract.RegistrySnapshot{Revision: "2", Complete: false})
		done <- [2]error{first, second}
	}()
	select {
	case errs := <-done:
		if errs[0] != nil || errs[1] == nil {
			t.Fatalf("reconcile errors=%v", errs)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a record was written while the registry mutex was held")
	}
	if len(logs.events("reconcile.failed")) != 2 {
		t.Fatalf("records=%v", logs.all())
	}
}

func TestLogsTenantMismatchIsRateLimited(t *testing.T) {
	logs := newMemoryLog()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	registry, err := NewRegistry(RegistryOptions{Epoch: epochA, Now: func() time.Time { return now }, NewID: func() string { return "p" }, Logger: slog.New(logs), WarehouseFactory: func(context.Context, string) (WarehousePorts, error) {
		w := &warehouseStub{}
		return WarehousePorts{Orders: w, Catalog: w, Menu: w, Epoch: peerEpoch}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err = registry.Register(context.Background(), s3Registration(tenantA)); err != nil {
		t.Fatal(err)
	}
	scope := contract.RequestScope{OrgID: tenantB, TenantID: tenantA, LifecycleVersion: "1"}
	for range 50 {
		if _, _, err = registry.Resolve(context.Background(), scope); err == nil {
			t.Fatal("mismatch accepted")
		}
	}
	if n := len(logs.events("registry.tenant_mismatch")); n != 1 {
		t.Fatalf("records=%d, want 1 within the interval", n)
	}
	other := scope
	other.LifecycleVersion = "9"
	if _, _, err = registry.Resolve(context.Background(), other); err == nil {
		t.Fatal("mismatch accepted")
	}
	if n := len(logs.events("registry.tenant_mismatch")); n != 2 {
		t.Fatalf("a different presented value was suppressed: %d", n)
	}
	now = now.Add(2 * time.Minute)
	if _, _, err = registry.Resolve(context.Background(), scope); err == nil {
		t.Fatal("mismatch accepted")
	}
	if n := len(logs.events("registry.tenant_mismatch")); n != 3 {
		t.Fatalf("record after the interval: %d, want 3", n)
	}
	for i := range 3 * mismatchLogCapacity {
		spam := scope
		spam.OrgID = fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
		if _, _, err = registry.Resolve(context.Background(), spam); err == nil {
			t.Fatal("mismatch accepted")
		}
	}
	registry.mismatchMu.Lock()
	size := len(registry.mismatchSeen)
	registry.mismatchMu.Unlock()
	if size > mismatchLogCapacity {
		t.Fatalf("dedup map grew to %d", size)
	}
}

func TestLogsReconcileCancellationIsQuiet(t *testing.T) {
	logs := newMemoryLog()
	registry, err := NewRegistry(RegistryOptions{Epoch: epochA, Now: time.Now, NewID: func() string { return "p" }, Logger: slog.New(logs), WarehouseFactory: func(context.Context, string) (WarehousePorts, error) {
		w := &warehouseStub{}
		return WarehousePorts{Orders: w, Catalog: w, Menu: w, Epoch: peerEpoch}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err = registry.Reconcile(canceled, contract.RegistrySnapshot{Revision: "1", Complete: true}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	if len(logs.all()) != 0 {
		t.Fatalf("shutdown wrote records: %v", logs.all())
	}
	expired, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	if err = registry.Reconcile(expired, contract.RegistrySnapshot{Revision: "1", Complete: true}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
	wantFields(t, logs.one(t, "reconcile.failed"), map[string]string{"reason": "context_done"})
}

func TestErrorCodeIsBounded(t *testing.T) {
	long := strings.Repeat("X", 1000)
	if got := errorCode(&contract.OperationError{Code: long}); len(got) > 64 {
		t.Fatalf("code length %d", len(got))
	}
}
