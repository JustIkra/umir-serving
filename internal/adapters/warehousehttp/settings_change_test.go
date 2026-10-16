package warehousehttp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/JustIkra/umir-serving/contract"
)

// currentSettingsEnvelope answers like the real scoped writer: the settings in
// the envelope are those of the presented token, not of any saved attempt.
func currentSettingsEnvelope(t *testing.T, w http.ResponseWriter, scope contract.RequestScope, result any, mutate func(map[string]any)) {
	t.Helper()
	s2Envelope(t, w, result, func(ctx map[string]any) {
		ctx["settings"] = map[string]any{"timeZone": scope.TimeZone, "settingsVersion": scope.SettingsVersion}
		if mutate != nil {
			mutate(ctx)
		}
	})
}

func TestRetainedPassiveReadsIgnoreCurrentSettings(t *testing.T) {
	scope := s2Scope("resolveOrder", "consumer.order.resolve")
	scope.TimeZone, scope.SettingsVersion = "Europe/Moscow", "2"
	saved := contract.WarehouseContext{OrgID: s2Org, TenantID: s2Tenant, Epoch: s2Epoch, TimeZone: "Europe/Berlin", SettingsVersion: "1"}
	lookups := map[string]func(*Client, context.Context) error{
		"/order-attempts": func(c *Client, ctx context.Context) error {
			_, err := c.FindOrderAttempt(ctx, "command", "proposal")
			return err
		},
		"/commands": func(c *Client, ctx context.Context) error {
			_, _, err := c.FindRecordedCommand(ctx, "command")
			return err
		},
		"/orders": func(c *Client, ctx context.Context) error {
			_, _, err := c.FindOrderByID(ctx, "order")
			return err
		},
	}
	for path, lookup := range lookups {
		t.Run(path, func(t *testing.T) {
			for _, mismatch := range []struct {
				name   string
				mutate func(map[string]any)
				ok     bool
			}{
				{"other settings", nil, true},
				{"other epoch", func(ctx map[string]any) { ctx["runtimeEpoch"] = s2Other }, false},
				{"other org", func(ctx map[string]any) { ctx["orgID"] = s2Other }, false},
				{"other tenant", func(ctx map[string]any) { ctx["tenantID"] = s2Other }, false},
			} {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					result := map[string]any{"state": "Unseen"}
					if r.URL.Path != "/internal/warehouse/v2/r/"+s2Tenant+"/order-attempts" {
						result = map[string]any{"found": false}
					}
					currentSettingsEnvelope(t, w, scope, result, mismatch.mutate)
				}))
				observation := &s2Observation{}
				ctx := contract.WithWarehouseObserver(contract.WithWarehouseBinding(contract.WithRequestScope(context.Background(), scope), saved, true), observation)
				err := lookup(s2Client(t, server, scope), ctx)
				server.Close()
				if (err == nil) != mismatch.ok {
					t.Fatalf("%s: err=%v, want ok=%v", mismatch.name, err, mismatch.ok)
				}
				if mismatch.ok && (len(observation.values) != 1 || observation.values[0].TimeZone != saved.TimeZone || observation.values[0].SettingsVersion != saved.SettingsVersion) {
					t.Fatalf("observation does not keep the saved settings: %+v", observation.values)
				}
			}
		})
	}
}

func TestRetainedCommitStaysStrictAboutSettings(t *testing.T) {
	scope := s2Scope("confirmOrder", "consumer.order.commit")
	scope.TimeZone, scope.SettingsVersion = "Europe/Moscow", "2"
	saved := contract.WarehouseContext{OrgID: s2Org, TenantID: s2Tenant, Epoch: s2Epoch, TimeZone: "Europe/Berlin", SettingsVersion: "1"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		currentSettingsEnvelope(t, w, scope, map[string]any{"kind": "OrderCommitted"}, nil)
	}))
	defer server.Close()
	ctx := contract.WithWarehouseBinding(contract.WithRequestScope(context.Background(), scope), saved, true)
	_, err := s2Client(t, server, scope).CommitAcceptedOrder(ctx, structCommitRequest())
	var operation *contract.OperationError
	if !errors.As(err, &operation) || operation.Outcome != "unknown" {
		t.Fatalf("retained commit with other settings: %v", err)
	}
}

// CR 01: these codes are not in the public Error.code enum. Only the legacy
// parser can produce them; the scoped client must refuse them with the generic WarehouseUnavailable error, which the HTTP
// handler reports as UpstreamUnavailable, so that none of them reaches Gateway as an unknown code.
func TestScopedClientRefusesCodesOutsideTheEnum(t *testing.T) {
	scope := s2Scope("servingDish", "catalog.read")
	for _, code := range []string{"ProposalSuperseded", "ProductNotFound", "DishNotFound"} {
		t.Run(code, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				writeResponse(t, w, `{"code":"`+code+`","message":"absent","outcome":"unavailable","retryable":false}`)
			}))
			defer server.Close()
			_, err := s2Client(t, server, scope).GetDish(contract.WithRequestScope(context.Background(), scope), "absent")
			var operation *contract.OperationError
			if !errors.As(err, &operation) || operation.Code != "WarehouseUnavailable" || operation.Outcome != "unavailable" {
				t.Fatalf("%s: got %v, want the generic WarehouseUnavailable refusal", code, err)
			}
		})
	}
}
