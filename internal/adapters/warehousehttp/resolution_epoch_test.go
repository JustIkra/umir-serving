package warehousehttp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/JustIkra/umir-serving/contract"
)

func TestS3PassiveAttemptUsesFrozenQueryAndSavedEpoch(t *testing.T) {
	scope := s2Scope("resolveOrder", "consumer.order.resolve")
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "GET" || r.URL.Path != "/internal/warehouse/v2/r/"+s2Tenant+"/order-attempts" || r.URL.Query().Get("commandID") != "unknown-command" || len(r.URL.Query()) != 1 || r.Header.Get("X-Umir-Runtime-Epoch") != s2Epoch {
			t.Errorf("wrong passive request: %s %s headers=%v", r.Method, r.URL, r.Header)
		}
		s2Envelope(t, w, map[string]any{"state": "Unseen"}, nil)
	}))
	defer server.Close()
	c := s2Client(t, server, scope)
	result, err := c.FindOrderAttempt(contract.WithRequestScope(context.Background(), scope), "unknown-command", "proposal")
	if err != nil || result.State != contract.OrderAttemptUnseen || calls != 1 {
		t.Fatalf("lookup=%+v/%v calls=%d", result, err, calls)
	}
}

func TestS3RetainedLookupPreservesOriginalEpochAndSettings(t *testing.T) {
	scope := s2Scope("resolveOrder", "consumer.order.resolve")
	scope.TimeZone = "Europe/Moscow"
	scope.SettingsVersion = "2"
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		wantEpoch := s2Epoch
		if calls == 2 {
			wantEpoch = s2Other
		}
		if r.Header.Get("X-Umir-Runtime-Epoch") != wantEpoch {
			t.Errorf("replaced saved epoch: %s", r.Header.Get("X-Umir-Runtime-Epoch"))
		}
		s2Envelope(t, w, map[string]any{"state": "Unseen"}, nil)
	}))
	defer server.Close()
	c := s2Client(t, server, scope)
	c.scoped.Epoch = s2Other
	saved := contract.WarehouseContext{OrgID: s2Org, TenantID: s2Tenant, Epoch: s2Epoch, TimeZone: "Europe/Berlin", SettingsVersion: "1"}
	ctx := contract.WithWarehouseBinding(contract.WithRequestScope(context.Background(), scope), saved, true)
	result, err := c.FindOrderAttempt(ctx, "command", "proposal")
	if err != nil || result.State != contract.OrderAttemptUnseen {
		t.Fatalf("saved lookup=%+v/%v", result, err)
	}
	if _, err = c.FindOrderAttempt(contract.WithRequestScope(context.Background(), scope), "command", "proposal"); err == nil {
		t.Fatal("fresh lookup accepted foreign epoch/settings")
	}
}

func TestS3ConsumerOrderUsesOrderIDAndNeverProposalQuery(t *testing.T) {
	scope := s2Scope("resolveOrder", "consumer.order.resolve")
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Get("orderID") != "order" || len(r.URL.Query()) != 1 || r.Header.Get("X-Umir-Runtime-Epoch") != s2Epoch {
			t.Errorf("wrong order lookup: %s", r.URL)
		}
		s2Envelope(t, w, map[string]any{"found": false}, nil)
	}))
	defer server.Close()
	c := s2Client(t, server, scope)
	ctx := contract.WithRequestScope(context.Background(), scope)
	_, found, err := c.FindOrderByID(ctx, "order")
	if err != nil || found || calls != 1 {
		t.Fatalf("order lookup found=%v err=%v calls=%d", found, err, calls)
	}
	if _, _, err = c.FindOrderByProposal(ctx, "proposal"); err == nil || calls != 1 {
		t.Fatalf("legacy proposal query escaped: err=%v calls=%d", err, calls)
	}
}

func TestS3PassiveRefusalRetainsOwnerSettingsChangedOutcome(t *testing.T) {
	scope := s2Scope("resolveOrder", "consumer.order.resolve")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		s2Envelope(t, w, map[string]any{"state": "Rejected", "error": map[string]any{"code": "SettingsChanged", "message": "fresh settings were stale", "outcome": "not_committed"}}, nil)
	}))
	defer server.Close()
	c := s2Client(t, server, scope)
	got, err := c.FindOrderAttempt(contract.WithRequestScope(context.Background(), scope), "command", "proposal")
	if err != nil || got.State != contract.OrderAttemptRejected || got.Error == nil || got.Error.Code != "SettingsChanged" || got.Error.Outcome != "not_committed" {
		t.Fatalf("owner refusal=%+v/%v", got, err)
	}
}
