package bootstrap

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/JustIkra/umir-serving/contract"
)

const tenantA = "33333333-3333-4333-8333-333333333333"
const tenantB = "55555555-5555-4555-8555-555555555555"
const orgA = "22222222-2222-4222-8222-222222222222"
const epochA = "44444444-4444-4444-8444-444444444444"
const peerEpoch = "66666666-6666-4666-8666-666666666666"
const operationA = "77777777-7777-4777-8777-777777777777"

func TestS1RegistryIsolatesIdenticalProposalAndCommandIDs(t *testing.T) {
	at := time.Date(2026, 10, 13, 12, 0, 0, 0, time.FixedZone("MSK", 10800))
	calls := 0
	registry, err := NewRegistry(RegistryOptions{Epoch: epochA, Now: func() time.Time { return at }, NewID: func() string { return "proposal-same" }, WarehouseFactory: func(_ context.Context, tenant string) (WarehousePorts, error) {
		calls++
		portions := int64(1)
		if tenant == tenantB {
			portions = 2
		}
		w := &warehouseStub{evaluation: contract.OrderEvaluation{EvaluatedAt: at, StateVersion: 7, RequestedPositions: []contract.Position{{LineNo: 1, DishID: "soup", Portions: portions}}, AcceptedPositions: []contract.AcceptedPosition{{LineNo: 1, DishID: "soup", Portions: portions}}}, commit: contract.CommitResult{Kind: contract.OrderCommitted, Order: &contract.Order{OrderID: tenant, ProposalID: "proposal-same", PlacedAt: at, Positions: []contract.AcceptedPosition{{LineNo: 1, DishID: "soup", Portions: portions}}}}}
		return WarehousePorts{Orders: w, Catalog: w, Menu: w, Epoch: peerEpoch}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	scope := contract.RequestScope{OrgID: orgA, TenantID: tenantA, LifecycleVersion: "1", Permissions: []string{"order.prepare", "order.confirm", "order.read"}}
	if _, _, err = registry.Resolve(context.Background(), scope); err == nil || calls != 0 {
		t.Fatalf("unregistered tenant resolved: %v calls=%d", err, calls)
	}
	for _, tenant := range []string{tenantA, tenantB} {
		if err = registry.Register(context.Background(), contract.RestaurantRegistration{OrgID: orgA, TenantID: tenant, OperationID: operationA, ExpectedEpoch: epochA, LifecycleVersion: "1"}); err != nil {
			t.Fatal(err)
		}
		scope.TenantID = tenant
		runtime, _, resolveErr := registry.Resolve(context.Background(), scope)
		if resolveErr != nil {
			t.Fatal(resolveErr)
		}
		ctx := contract.WithRequestScope(context.Background(), scope)
		if tenant == tenantB {
			if _, err = runtime.ConfirmOrder(ctx, contract.ConfirmOrderRequest{CommandID: "command-foreign", ProposalID: "proposal-same"}); !errors.Is(err, contract.ErrProposalNotFound) {
				t.Fatalf("foreign proposal discovered: %v", err)
			}
		}
		portions := int64(1)
		if tenant == tenantB {
			portions = 2
		}
		proposal, prepareErr := runtime.PrepareOrder(ctx, contract.PrepareOrderRequest{Positions: []contract.Position{{DishID: "soup", Portions: portions}}})
		if prepareErr != nil {
			t.Fatal(prepareErr)
		}
		if proposal.ProposalID != "proposal-same" || proposal.AcceptedPositions[0].Portions != portions {
			t.Fatalf("proposal=%+v", proposal)
		}
		result, confirmErr := runtime.ConfirmOrder(ctx, contract.ConfirmOrderRequest{CommandID: "command-same", ProposalID: "proposal-same"})
		if confirmErr != nil || result.Order == nil || result.Order.OrderID != tenant {
			t.Fatalf("result=%+v err=%v", result, confirmErr)
		}
		scope.TenantID = tenantA
		if tenant == tenantA {
			scope.TenantID = tenantB
		}
		if _, err = runtime.GetMenu(contract.WithRequestScope(context.Background(), scope)); err == nil {
			t.Fatal("retained runtime accepted foreign context")
		}
	}
	if calls != 2 {
		t.Fatalf("factory calls=%d", calls)
	}
}

func TestS1RegistryConcurrentRegistrationAndScopeCopies(t *testing.T) {
	var calls int
	registry, err := NewRegistry(RegistryOptions{Epoch: epochA, Now: time.Now, NewID: func() string { return "p" }, WarehouseFactory: func(context.Context, string) (WarehousePorts, error) {
		calls++
		w := &warehouseStub{}
		return WarehousePorts{Orders: w, Catalog: w, Menu: w, Epoch: peerEpoch}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	reg := contract.RestaurantRegistration{OrgID: orgA, TenantID: tenantA, OperationID: operationA, ExpectedEpoch: epochA, LifecycleVersion: "1"}
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			if registerErr := registry.Register(context.Background(), reg); registerErr != nil {
				t.Error(registerErr)
			}
		})
	}
	wg.Wait()
	if calls != 1 {
		t.Fatalf("factory calls=%d", calls)
	}
	for _, edit := range []func(*contract.RestaurantRegistration){func(r *contract.RestaurantRegistration) { r.OrgID = tenantB }, func(r *contract.RestaurantRegistration) { r.ExpectedEpoch = peerEpoch }, func(r *contract.RestaurantRegistration) { r.LifecycleVersion = "2" }, func(r *contract.RestaurantRegistration) { r.OperationID = tenantB }} {
		changed := reg
		edit(&changed)
		if registry.Register(context.Background(), changed) == nil {
			t.Fatal("changed registration accepted")
		}
	}
	scope := contract.RequestScope{OrgID: orgA, TenantID: tenantA, LifecycleVersion: "1", Permissions: []string{"menu.read"}}
	ctx := contract.WithRequestScope(context.Background(), scope)
	scope.Permissions[0] = "order.confirm"
	copied, ok := contract.RequestScopeFromContext(ctx)
	if !ok || copied.Permissions[0] != "menu.read" {
		t.Fatal("scope aliased caller permissions")
	}
	copied.Permissions[0] = "catalog.write"
	copied, _ = contract.RequestScopeFromContext(ctx)
	if copied.Permissions[0] != "menu.read" {
		t.Fatal("scope aliased returned permissions")
	}
}

func TestS1EveryRuntimeMethodRejectsMissingForeignAndInsufficientScope(t *testing.T) {
	w := &warehouseStub{}
	registry, err := NewRegistry(RegistryOptions{Epoch: epochA, Now: time.Now, NewID: func() string { return "p" }, WarehouseFactory: func(context.Context, string) (WarehousePorts, error) {
		return WarehousePorts{Orders: w, Catalog: w, Menu: w, Epoch: peerEpoch}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err = registry.Register(context.Background(), contract.RestaurantRegistration{OrgID: orgA, TenantID: tenantA, OperationID: operationA, ExpectedEpoch: epochA, LifecycleVersion: "1"}); err != nil {
		t.Fatal(err)
	}
	scope := contract.RequestScope{OrgID: orgA, TenantID: tenantA, LifecycleVersion: "1", Permissions: []string{"menu.read", "order.prepare"}}
	runtime, _, err := registry.Resolve(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	methods := []func(context.Context) error{
		func(ctx context.Context) error { _, err := runtime.GetProducts(ctx); return err },
		func(ctx context.Context) error { _, err := runtime.GetDishCatalog(ctx); return err },
		func(ctx context.Context) error { _, err := runtime.GetDish(ctx, "soup"); return err },
		func(ctx context.Context) error {
			_, err := runtime.RegisterProduct(ctx, contract.RegisterProductRequest{})
			return err
		},
		func(ctx context.Context) error {
			_, err := runtime.DefineDish(ctx, contract.DefineDishRequest{})
			return err
		},
		func(ctx context.Context) error { _, err := runtime.GetMenu(ctx); return err },
		func(ctx context.Context) error { _, err := runtime.GetStopList(ctx); return err },
		func(ctx context.Context) error { _, err := runtime.GetRecommendations(ctx); return err },
		func(ctx context.Context) error {
			_, err := runtime.PrepareOrder(ctx, contract.PrepareOrderRequest{})
			return err
		},
		func(ctx context.Context) error {
			_, err := runtime.ConfirmOrder(ctx, contract.ConfirmOrderRequest{})
			return err
		},
		func(ctx context.Context) error {
			_, err := runtime.ResolveConfirmation(ctx, contract.ConfirmOrderRequest{})
			return err
		},
	}
	foreign := scope
	foreign.TenantID = tenantB
	denied := scope
	denied.Permissions = nil
	for _, ctx := range []context.Context{context.Background(), contract.WithRequestScope(context.Background(), foreign), contract.WithRequestScope(context.Background(), denied)} {
		for index, method := range methods {
			var refusal *contract.OperationError
			if err = method(ctx); !errors.As(err, &refusal) || refusal.Code != "Forbidden" {
				t.Fatalf("method %d bypassed scope: %v", index, err)
			}
		}
	}
	if _, err = runtime.GetMenu(contract.WithRequestScope(context.Background(), scope)); err != nil {
		t.Fatal(err)
	}
	scope.Permissions = []string{"menu.read"}
	if _, err = runtime.PrepareOrder(contract.WithRequestScope(context.Background(), scope), contract.PrepareOrderRequest{}); err == nil {
		t.Fatal("menu grant escalated into order preparation")
	}
}
