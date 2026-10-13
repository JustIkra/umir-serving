package authhttp

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JustIkra/umir-serving/contract"
)

const testTenant = "33333333-3333-4333-8333-333333333333"
const testOrg = "22222222-2222-4222-8222-222222222222"
const testEpoch = "44444444-4444-4444-8444-444444444444"

func testRegistry() map[string]any {
	return map[string]any{"registryRevision": "1", "complete": true, "entries": []any{map[string]any{"restaurant": map[string]any{"restaurantID": testTenant, "orgID": testOrg, "name": "Fixture", "lifecycle": "ready", "lifecycleVersion": "1"}, "settings": map[string]any{"timeZone": "Europe/Moscow", "settingsVersion": "1"}, "services": []any{map[string]any{"restaurantID": testTenant, "service": "warehouse", "epoch": testEpoch, "state": "ready", "lifecycleVersion": "1"}, map[string]any{"restaurantID": testTenant, "service": "serving", "epoch": testTenant, "state": "ready", "lifecycleVersion": "1"}}}}, "tombstones": []any{}}
}
func testClient(t *testing.T, s *httptest.Server) *Client {
	t.Helper()
	c, e := New(s.URL, "synthetic-auth-client", s.Client(), time.Second, time.Now)
	if e != nil {
		t.Fatal(e)
	}
	return c
}
func testWrite(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if e := json.NewEncoder(w).Encode(v); e != nil {
		t.Error(e)
	}
}

func TestS4AuthRegistryRejectsIncompleteOrMisboundPeers(t *testing.T) {
	for _, kind := range []string{"valid", "partial", "missing-entries", "missing-services", "duplicate-tenant", "duplicate-service", "wrong-restaurant", "wrong-generation", "invalid-epoch", "unknown-zone", "active-tombstone", "unknown-field", "redirect", "media", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			value := testRegistry()
			entries, ok := value["entries"].([]any)
			if !ok {
				t.Fatal("fixture entries")
			}
			entry, ok := entries[0].(map[string]any)
			if !ok {
				t.Fatal("fixture entry")
			}
			services, ok := entry["services"].([]any)
			if !ok {
				t.Fatal("fixture services")
			}
			peer, ok := services[0].(map[string]any)
			if !ok {
				t.Fatal("fixture peer")
			}
			switch kind {
			case "partial":
				value["complete"] = false
			case "missing-entries":
				delete(value, "entries")
			case "missing-services":
				delete(entry, "services")
			case "duplicate-tenant":
				value["entries"] = append(entries, entry)
			case "duplicate-service":
				entry["services"] = append(services, peer)
			case "wrong-restaurant":
				peer["restaurantID"] = testOrg
			case "wrong-generation":
				peer["lifecycleVersion"] = "2"
			case "invalid-epoch":
				peer["epoch"] = "old"
			case "unknown-zone":
				entry["settings"] = map[string]any{"timeZone": "Mars/Olympus", "settingsVersion": "1"}
			case "active-tombstone":
				value["tombstones"] = []any{map[string]any{"restaurantID": testTenant, "orgID": testOrg, "lifecycleVersion": "1"}}
			case "unknown-field":
				value["secret"] = "not-allowed"
			}
			requests := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Header.Get("X-Umir-Service-Key") != "synthetic-auth-client" || r.URL.Path != "/internal/auth/v1/registry" {
					t.Error("wrong private request")
				}
				if kind == "redirect" {
					w.Header().Set("Location", "/elsewhere")
					w.WriteHeader(302)
					return
				}
				if kind == "media" {
					w.Header().Set("Content-Type", "text/plain")
					if _, e := w.Write([]byte(`{}`)); e != nil {
						t.Error(e)
					}
					return
				}
				testWrite(t, w, value)
			}))
			defer s.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if kind == "cancelled" {
				cancel()
			}
			got, err := testClient(t, s).Registry(ctx)
			if kind == "valid" {
				if err != nil {
					t.Fatal(err)
				}
				if p := got.Projection(); !p.Complete || p.Entries[0].TenantID != testTenant {
					t.Fatal(p)
				}
				return
			}
			var op *contract.OperationError
			if !errors.As(err, &op) || op.Code != "AuthUnavailable" || op.Outcome != "unavailable" {
				t.Fatalf("%s accepted or mistyped: %v", kind, err)
			}
			if requests > 1 {
				t.Fatal("automatic redirect or retry")
			}
		})
	}
}
func TestS4AuthJWKSRotationIsBoundedAndCopied(t *testing.T) {
	first, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	next, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.FixedZone("MSK", 10800))
	calls := 0
	nextPublished := false
	invalid := false
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("X-Umir-Service-Key") != "synthetic-auth-client" || r.URL.Path != "/internal/auth/v1/jwks" {
			t.Error("wrong discovery credentials")
		}
		key := func(id string, p ed25519.PublicKey) any {
			return map[string]any{"kty": "OKP", "crv": "Ed25519", "alg": "EdDSA", "use": "sig", "kid": id, "x": base64.RawURLEncoding.EncodeToString(p)}
		}
		keys := []any{key("first", first)}
		if nextPublished {
			keys = append(keys, key("next", next))
		}
		if invalid {
			keys = append(keys, key("first", first))
		}
		testWrite(t, w, map[string]any{"keys": keys})
	}))
	defer s.Close()
	c, e := NewWithRetryClock(s.URL, "synthetic-auth-client", s.Client(), time.Second, func() time.Time { return now }, func() time.Time { return now })
	if e != nil {
		t.Fatal(e)
	}
	p, e := c.SigningKey(context.Background(), "first")
	if e != nil {
		t.Fatal(e)
	}
	p[0] ^= 255
	p, e = c.SigningKey(context.Background(), "first")
	if e != nil || p[0] != first[0] || calls != 1 {
		t.Fatalf("cache alias or repeated refresh %d %v", calls, e)
	}
	nextPublished = true
	if _, e = c.SigningKey(context.Background(), "next"); e != nil || calls != 2 {
		t.Fatalf("unknown-kid rotation not refreshed: %d %v", calls, e)
	}
	for range 3 {
		if _, e = c.SigningKey(context.Background(), "missing"); e == nil {
			t.Fatal("unknown key accepted")
		}
	}
	if calls != 3 {
		t.Fatalf("unknown kid refresh was not bounded: %d", calls)
	}
	now = now.Add(30 * time.Second)
	invalid = true
	if _, e = c.SigningKey(context.Background(), "first"); e == nil {
		t.Fatal("duplicate Auth kid accepted after refresh")
	}
}
func TestS4AuthPeerEpochRequiresMatchingCurrentReadiness(t *testing.T) {
	for _, kind := range []string{"valid", "serving-reset", "warehouse-pending", "wrong-org", "wrong-generation", "deleted", "registration"} {
		t.Run(kind, func(t *testing.T) {
			v := testRegistry()
			entries, ok := v["entries"].([]any)
			if !ok {
				t.Fatal("entries")
			}
			entry, ok := entries[0].(map[string]any)
			if !ok {
				t.Fatal("entry")
			}
			r, ok := entry["restaurant"].(map[string]any)
			if !ok {
				t.Fatal("restaurant")
			}
			peers, ok := entry["services"].([]any)
			if !ok {
				t.Fatal("peers")
			}
			if kind == "deleted" {
				r["lifecycle"] = "deleting"
			}
			if kind == "warehouse-pending" {
				p, ok := peers[0].(map[string]any)
				if !ok {
					t.Fatal("peer")
				}
				p["state"] = "pending"
			}
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { testWrite(t, w, v) }))
			defer s.Close()
			scope := contract.RequestScope{OrgID: testOrg, TenantID: testTenant, LifecycleVersion: "1"}
			if kind == "wrong-org" {
				scope.OrgID = testTenant
			}
			if kind == "wrong-generation" {
				scope.LifecycleVersion = "2"
			}
			ctx := contract.WithRequestScope(context.Background(), scope)
			epoch := testTenant
			if kind == "serving-reset" {
				epoch = testOrg
			}
			if kind == "registration" {
				ctx = context.Background()
				epoch = testOrg
			}
			got, e := testClient(t, s).WarehouseEpoch(ctx, testTenant, epoch)
			if kind == "valid" || kind == "registration" {
				if e != nil || got != testEpoch {
					t.Fatalf("valid peer rejected: %s %v", got, e)
				}
			} else if e == nil {
				t.Fatal("unacknowledged scope accepted")
			}
		})
	}
}
func TestS4AuthTransportUsesCallerCancellation(t *testing.T) {
	entered := make(chan struct{})
	var once sync.Once
	s := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { once.Do(func() { close(entered) }); <-r.Context().Done() }))
	defer s.Close()
	c := testClient(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := c.Registry(ctx); done <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("request not started")
	}
	cancel()
	select {
	case e := <-done:
		if e == nil || !strings.Contains(e.Error(), "Auth discovery") {
			t.Fatal(e)
		}
	case <-time.After(time.Second):
		t.Fatal("caller cancellation ignored")
	}
}

func TestS4SigningDiscoveryWaitPreservesCallerDeadline(t *testing.T) {
	entered := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done() }))
	defer s.Close()
	c := testClient(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := make(chan error, 1)
	go func() { _, err := c.SigningKey(ctx, "first"); first <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("discovery not started")
	}
	waiting, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer stop()
	second := make(chan error, 1)
	go func() { _, err := c.SigningKey(waiting, "second"); second <- err }()
	select {
	case err := <-second:
		if err == nil {
			t.Fatal("cancelled key wait succeeded")
		}
	case <-time.After(200 * time.Millisecond):
		cancel()
		<-first
		t.Fatal("serialized key discovery ignored waiting caller deadline")
	}
	cancel()
	select {
	case <-first:
	case <-time.After(time.Second):
		t.Fatal("first discovery did not stop")
	}
}
