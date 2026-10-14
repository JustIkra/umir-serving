package warehousehttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/JustIkra/umir-serving/contract"
)

const s2Tenant = "33333333-3333-4333-8333-333333333333"
const s2Other = "66666666-6666-4666-8666-666666666666"
const s2Org = "22222222-2222-4222-8222-222222222222"
const s2Epoch = "44444444-4444-4444-8444-444444444444"

var s2At = time.Date(2026, 10, 13, 9, 0, 0, 0, time.FixedZone("local", 2*3600))

type s2Verifier func(context.Context, string) (contract.RequestScope, error)

func (v s2Verifier) Verify(ctx context.Context, token string) (contract.RequestScope, error) {
	return v(ctx, token)
}

type s2Observation struct{ values []contract.WarehouseContext }

func (o *s2Observation) Record(v contract.WarehouseContext) { o.values = append(o.values, v) }

func s2Scope(operation, permission string) contract.RequestScope {
	return contract.RequestScope{OrgID: s2Org, TenantID: s2Tenant, UserID: s2Other, Operation: operation, Permissions: []string{permission}, TimeZone: "Europe/Berlin", SettingsVersion: "1", LifecycleVersion: "1"}.WithToken("synthetic-a")
}
func s2Client(t *testing.T, server *httptest.Server, scope contract.RequestScope) *Client {
	t.Helper()
	c, err := NewScopedClient(server.URL, server.Client(), time.Second, ScopedOptions{TenantID: s2Tenant, Epoch: s2Epoch, ServiceKey: "synthetic-serving-key", Verifier: s2Verifier(func(_ context.Context, token string) (contract.RequestScope, error) {
		if token != "synthetic-a" {
			return contract.RequestScope{}, errors.New("foreign credential")
		}
		return scope, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func s2Envelope(t *testing.T, w http.ResponseWriter, result any, mutate func(map[string]any)) {
	t.Helper()
	ctx := map[string]any{"orgID": s2Org, "tenantID": s2Tenant, "runtimeEpoch": s2Epoch, "settings": map[string]any{"timeZone": "Europe/Berlin", "settingsVersion": "1"}, "evaluatedAt": s2At.Format(time.RFC3339Nano), "stateVersion": "9007199254740993"}
	if mutate != nil {
		mutate(ctx)
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"context": ctx, "result": result}); err != nil {
		t.Error(err)
	}
}
func s2Menu() map[string]any {
	return map[string]any{"evaluatedAt": s2At.Format(time.RFC3339Nano), "stateVersion": "9007199254740993", "dishes": []any{map[string]any{"dishID": "dish", "dishName": "Dish", "isAvailable": true, "deficits": []any{}, "allocations": []any{map[string]any{"productID": "p", "productName": "P", "batchID": "b", "quantity": "9007199254740993", "baseUnit": "ml", "expiresAt": s2At.Add(time.Hour).Format(time.RFC3339Nano)}}}}}
}
func TestS2ScopedMenuForwardingAndObservation(t *testing.T) {
	scope := s2Scope("recommendations", "consumer.menu.evaluate")
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "GET" || r.URL.Path != "/internal/warehouse/v2/r/"+s2Tenant+"/menu-evaluation" || r.Header.Get("Authorization") != "Bearer synthetic-a" || r.Header.Get("X-Umir-Service-Key") != "synthetic-serving-key" {
			t.Errorf("wrong scope/credential forwarding: %s %s", r.Method, r.URL.Path)
		}
		s2Envelope(t, w, s2Menu(), func(ctx map[string]any) { ctx["warehouseEpoch"] = s2Epoch })
	}))
	defer server.Close()
	c := s2Client(t, server, scope)
	o := &s2Observation{}
	ctx := contract.WithWarehouseObserver(contract.WithRequestScope(context.Background(), scope), o)
	for range 2 {
		got, err := c.EvaluateMenu(ctx)
		if err != nil {
			t.Fatal(err)
		}
		a := got.Dishes[0].Allocations[0]
		if a.Quantity != 9007199254740993 || a.BaseUnit != "ml" || !a.ExpiresAt.Equal(s2At.Add(time.Hour)) || got.StateVersion != 9007199254740993 {
			t.Fatalf("lossy result: %+v", got)
		}
	}
	if calls != 2 || len(o.values) != 2 || o.values[0].Epoch != s2Epoch || !o.values[0].EvaluatedAt.Equal(s2At) {
		t.Fatalf("observation/calls: %+v/%d", o, calls)
	}
}
func TestS2ScopeRefusalBeforeDispatch(t *testing.T) {
	scope := s2Scope("recommendations", "consumer.menu.evaluate")
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls++; s2Envelope(t, w, s2Menu(), nil) }))
	defer server.Close()
	c := s2Client(t, server, scope)
	for _, kind := range []string{"missing", "tenant", "token", "actor", "permission", "operation", "settings", "cancelled", "catalog-write"} {
		t.Run(kind, func(t *testing.T) {
			s := scope
			ctx := context.Background()
			switch kind {
			case "tenant":
				s.TenantID = s2Other
			case "token":
				s = s.WithToken("synthetic-b")
			case "actor":
				s.UserID = s2Tenant
			case "permission":
				s.Permissions = []string{"stock.read"}
			case "operation":
				s.Operation = "prepareOrder"
			case "settings":
				s.SettingsVersion = "2"
			}
			if kind != "missing" {
				ctx = contract.WithRequestScope(ctx, s)
			}
			if kind == "cancelled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			var err error
			if kind == "catalog-write" {
				_, err = c.RegisterProduct(ctx, contract.RegisterProductRequest{ProductID: "p"})
			} else {
				_, err = c.EvaluateMenu(ctx)
			}
			if err == nil {
				t.Fatal("unsafe context accepted")
			}
		})
	}
	if calls != 0 {
		t.Fatalf("refusals dispatched %d requests", calls)
	}
}
func TestS2RejectInconsistentEnvelope(t *testing.T) {
	scope := s2Scope("recommendations", "consumer.menu.evaluate")
	for _, kind := range []string{"tenant", "org", "epoch", "zone", "settings", "time", "version", "expired", "empty-available", "invalid-unit", "unavailable-allocation"} {
		t.Run(kind, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				result := s2Menu()
				dishes, ok := result["dishes"].([]any)
				if !ok {
					t.Fatal("fixture dishes shape")
				}
				dish, ok := dishes[0].(map[string]any)
				if !ok {
					t.Fatal("fixture dish shape")
				}
				allocations, ok := dish["allocations"].([]any)
				if !ok {
					t.Fatal("fixture allocations shape")
				}
				a, ok := allocations[0].(map[string]any)
				if !ok {
					t.Fatal("fixture allocation shape")
				}
				switch kind {
				case "expired":
					a["expiresAt"] = s2At.Format(time.RFC3339Nano)
				case "empty-available":
					dish["allocations"] = []any{}
				case "invalid-unit":
					a["baseUnit"] = "kg"
				case "unavailable-allocation":
					dish["isAvailable"] = false
				}
				s2Envelope(t, w, result, func(ctx map[string]any) {
					switch kind {
					case "tenant":
						ctx["tenantID"] = s2Other
					case "org":
						ctx["orgID"] = s2Other
					case "epoch":
						ctx["runtimeEpoch"] = s2Other
					case "zone":
						settings, ok := ctx["settings"].(map[string]any)
						if !ok {
							t.Fatal("fixture settings shape")
						}
						settings["timeZone"] = "Europe/Moscow"
					case "settings":
						settings, ok := ctx["settings"].(map[string]any)
						if !ok {
							t.Fatal("fixture settings shape")
						}
						settings["settingsVersion"] = "2"
					case "time":
						ctx["evaluatedAt"] = s2At.Add(time.Second).Format(time.RFC3339Nano)
					case "version":
						ctx["stateVersion"] = "1"
					}
				})
			}))
			defer server.Close()
			c := s2Client(t, server, scope)
			o := &s2Observation{}
			_, err := c.EvaluateMenu(contract.WithWarehouseObserver(contract.WithRequestScope(context.Background(), scope), o))
			if err == nil || len(o.values) != 0 {
				t.Fatalf("invalid response accepted/observed: %v %+v", err, o)
			}
		})
	}
}
func TestS2CommitUnknownAndDeadline(t *testing.T) {
	scope := s2Scope("confirmOrder", "consumer.order.commit")
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("X-Umir-Runtime-Epoch") != s2Epoch {
			t.Error("missing epoch")
		}
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"malformed":true}`))
		if err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	c := s2Client(t, server, scope)
	_, err := c.CommitAcceptedOrder(contract.WithRequestScope(context.Background(), scope), structCommitRequest())
	var op *contract.OperationError
	if !errors.As(err, &op) || op.Outcome != "unknown" || calls != 1 {
		t.Fatalf("commit observation: %v calls=%d", err, calls)
	}
	captured := time.Time{}
	c.http.Transport = s2Transport(func(r *http.Request) (*http.Response, error) {
		captured, _ = r.Context().Deadline()
		return nil, context.DeadlineExceeded
	})
	deadline := time.Now().Add(100 * time.Millisecond)
	ctx, cancel := context.WithDeadline(contract.WithRequestScope(context.Background(), scope), deadline)
	defer cancel()
	_, err = c.CommitAcceptedOrder(ctx, structCommitRequest())
	if err == nil || captured.After(deadline) {
		t.Fatalf("deadline widened: %v %v", captured, err)
	}
}

type s2Transport func(*http.Request) (*http.Response, error)

func (t s2Transport) RoundTrip(r *http.Request) (*http.Response, error) { return t(r) }
func TestS2BoundedStrictDecode(t *testing.T) {
	scope := s2Scope("recommendations", "consumer.menu.evaluate")
	for _, body := range []string{`{"context":null,"result":{}}`, `{"context":{},"context":{},"result":{}}`, strings.Repeat(" ", maxResponseBytes+1)} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if _, err := w.Write([]byte(body)); err != nil {
				t.Error(err)
			}
		}))
		c := s2Client(t, server, scope)
		_, err := c.EvaluateMenu(contract.WithRequestScope(context.Background(), scope))
		server.Close()
		if err == nil {
			t.Fatal("malformed/oversize response accepted")
		}
	}
}

func TestS2CommitRejectsPendingSuccess(t *testing.T) {
	scope := s2Scope("confirmOrder", "consumer.order.commit")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { s2Envelope(t, w, map[string]any{"kind": "Pending"}, nil) }))
	defer server.Close()
	c := s2Client(t, server, scope)
	o := &s2Observation{}
	_, err := c.CommitAcceptedOrder(contract.WithWarehouseObserver(contract.WithRequestScope(context.Background(), scope), o), structCommitRequest())
	if err == nil || len(o.values) != 0 {
		t.Fatal("frozen v2 accepted legacy Pending success")
	}
}
func TestS2RejectsNonRFC3339Offset(t *testing.T) {
	scope := s2Scope("recommendations", "consumer.menu.evaluate")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		menu := s2Menu()
		menu["evaluatedAt"] = "2026-10-13T09:00:00+24:00"
		s2Envelope(t, w, menu, func(ctx map[string]any) { ctx["evaluatedAt"] = menu["evaluatedAt"] })
	}))
	defer server.Close()
	c := s2Client(t, server, scope)
	if _, err := c.EvaluateMenu(contract.WithRequestScope(context.Background(), scope)); err == nil {
		t.Fatal("invalid RFC3339 offset accepted")
	}
}

func TestS2CatalogAndOrderEvaluation(t *testing.T) {
	for _, kind := range []string{"products", "catalog", "dish", "evaluate", "commit"} {
		t.Run(kind, func(t *testing.T) {
			operation, permission := map[string][2]string{"products": {"servingProducts", "catalog.read"}, "catalog": {"servingDishes", "catalog.read"}, "dish": {"servingDish", "catalog.read"}, "evaluate": {"prepareOrder", "consumer.order.evaluate"}, "commit": {"confirmOrder", "consumer.order.commit"}}[kind][0], map[string][2]string{"products": {"servingProducts", "catalog.read"}, "catalog": {"servingDishes", "catalog.read"}, "dish": {"servingDish", "catalog.read"}, "evaluate": {"prepareOrder", "consumer.order.evaluate"}, "commit": {"confirmOrder", "consumer.order.commit"}}[kind][1]
			scope := s2Scope(operation, permission)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var result any
				switch kind {
				case "products":
					result = map[string]any{"evaluatedAt": s2At.Format(time.RFC3339Nano), "stateVersion": "9007199254740993", "items": []wireProduct{{ProductID: "p", Name: "P", BaseUnit: "ml"}}}
				case "catalog", "dish":
					d := wireDish{DishID: "Борщ /+&?", Name: "Dish", Recipe: []wireIngredient{{ProductID: "p", Quantity: "9007199254740993", BaseUnit: "ml"}}}
					if kind == "catalog" {
						result = map[string]any{"evaluatedAt": s2At.Format(time.RFC3339Nano), "stateVersion": "9007199254740993", "items": []wireDish{d}}
					} else {
						if r.URL.Query().Get("dishID") != d.DishID {
							t.Error("opaque query changed")
						}
						result = map[string]any{"evaluatedAt": s2At.Format(time.RFC3339Nano), "stateVersion": "9007199254740993", "dish": d}
					}
				case "evaluate":
					if r.Header.Get("X-Umir-Runtime-Epoch") != s2Epoch {
						t.Error("missing evaluation epoch")
					}
					p := []wirePosition{{LineNo: "1", DishID: "dish", Portions: "9007199254740993"}}
					result = wireOrderEvaluation{EvaluatedAt: s2At.Format(time.RFC3339Nano), StateVersion: "9007199254740993", RequestedPositions: p, AcceptedPositions: p, Changes: []wireChange{}}
				case "commit":
					result = wireCommitResult{Kind: contract.OrderCommitted, Order: &wireOrder{OrderID: "order", ProposalID: "proposal", PlacedAt: s2At.Format(time.RFC3339Nano), AcceptedPositions: []wirePosition{{LineNo: "1", DishID: "dish", Portions: "9007199254740993"}}, Allocations: []wireAllocation{{ProductID: "p", BatchID: "b", Quantity: "9007199254740993"}}}}
				}
				s2Envelope(t, w, result, nil)
			}))
			defer server.Close()
			c := s2Client(t, server, scope)
			ctx := contract.WithRequestScope(context.Background(), scope)
			var err error
			switch kind {
			case "products":
				var value contract.Products
				value, err = c.GetProducts(ctx)
				if err == nil && value.Items[0].BaseUnit != "ml" {
					t.Error("unit changed")
				}
			case "catalog":
				_, err = c.GetDishCatalog(ctx)
			case "dish":
				_, err = c.GetDish(ctx, "Борщ /+&?")
			case "evaluate":
				_, err = c.EvaluateOrder(ctx, contract.EvaluateOrderRequest{Positions: []contract.Position{{LineNo: 1, DishID: "dish", Portions: 9007199254740993}}})
			case "commit":
				_, err = c.CommitAcceptedOrder(ctx, contract.CommitOrderRequest{CommandID: "command", ProposalID: "proposal", AcceptedPositions: []contract.AcceptedPosition{{LineNo: 1, DishID: "dish", Portions: 9007199254740993}}})
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestS2EvaluationMustMatchRequest(t *testing.T) {
	scope := s2Scope("prepareOrder", "consumer.order.evaluate")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		p := []wirePosition{{LineNo: "1", DishID: "foreign", Portions: "1"}}
		s2Envelope(t, w, wireOrderEvaluation{EvaluatedAt: s2At.Format(time.RFC3339Nano), StateVersion: "9007199254740993", RequestedPositions: p, AcceptedPositions: p, Changes: []wireChange{}}, nil)
	}))
	defer server.Close()
	c := s2Client(t, server, scope)
	o := &s2Observation{}
	_, err := c.EvaluateOrder(contract.WithWarehouseObserver(contract.WithRequestScope(context.Background(), scope), o), contract.EvaluateOrderRequest{Positions: []contract.Position{{LineNo: 1, DishID: "requested", Portions: 1}}})
	if err == nil || len(o.values) != 0 {
		t.Fatal("evaluation for another input accepted/observed")
	}
}
func TestS2OversizePostNeverDispatches(t *testing.T) {
	scope := s2Scope("prepareOrder", "consumer.order.evaluate")
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls++; w.WriteHeader(503) }))
	defer server.Close()
	c := s2Client(t, server, scope)
	_, err := c.EvaluateOrder(contract.WithRequestScope(context.Background(), scope), contract.EvaluateOrderRequest{Positions: []contract.Position{{LineNo: 1, DishID: strings.Repeat("x", 1<<20), Portions: 1}}})
	if err == nil || calls != 0 {
		t.Fatalf("oversize request dispatched: err=%v calls=%d", err, calls)
	}
}

func TestS2ReadProtocolFailureIsUnavailable(t *testing.T) {
	scope := s2Scope("recommendations", "consumer.menu.evaluate")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		s2Envelope(t, w, s2Menu(), func(ctx map[string]any) { ctx["tenantID"] = s2Other })
	}))
	defer server.Close()
	c := s2Client(t, server, scope)
	_, err := c.EvaluateMenu(contract.WithRequestScope(context.Background(), scope))
	var operation *contract.OperationError
	if !errors.As(err, &operation) || operation.Outcome != "unavailable" {
		t.Fatalf("GET transport observation = %v", err)
	}
}
func TestS2RecheckCannotWidenAcceptedOrder(t *testing.T) {
	scope := s2Scope("confirmOrder", "consumer.order.commit")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		p := []wirePosition{{LineNo: "1", DishID: "dish", Portions: "2"}}
		s2Envelope(t, w, wireCommitResult{Kind: contract.RecheckRequired, Evaluation: &wireOrderEvaluation{EvaluatedAt: s2At.Format(time.RFC3339Nano), StateVersion: "9007199254740993", RequestedPositions: p, AcceptedPositions: p, Changes: []wireChange{}}}, nil)
	}))
	defer server.Close()
	c := s2Client(t, server, scope)
	_, err := c.CommitAcceptedOrder(contract.WithRequestScope(context.Background(), scope), contract.CommitOrderRequest{CommandID: "command", ProposalID: "proposal", AcceptedPositions: []contract.AcceptedPosition{{LineNo: 1, DishID: "dish", Portions: 1}}})
	if err == nil {
		t.Fatal("recheck widened immutable accepted positions")
	}
}

func TestS2CatalogNotFoundRemainsTyped(t *testing.T) {
	scope := s2Scope("servingDish", "catalog.read")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		writeResponse(t, w, `{"code":"NotFound","message":"dish is absent","outcome":"unavailable","retryable":false}`)
	}))
	defer server.Close()
	c := s2Client(t, server, scope)
	_, err := c.GetDish(contract.WithRequestScope(context.Background(), scope), "absent")
	var operation *contract.OperationError
	if !errors.As(err, &operation) || operation.Code != "NotFound" || operation.Outcome != "unavailable" {
		t.Fatalf("catalog refusal lost its type: %v", err)
	}
}
