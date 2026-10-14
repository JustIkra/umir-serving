package bootstrap

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/JustIkra/umir-serving/contract"
)

func s3Registry(t *testing.T, w *warehouseStub) *Registry {
	t.Helper()
	r, err := NewRegistry(RegistryOptions{Epoch: epochA, Now: time.Now, NewID: func() string { return "proposal" }, WarehouseFactory: func(context.Context, string) (WarehousePorts, error) {
		return WarehousePorts{Orders: w, Catalog: w, Menu: w, Epoch: peerEpoch}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func s3Registration(tenant string) contract.RestaurantRegistration {
	return contract.RestaurantRegistration{OrgID: orgA, TenantID: tenant, OperationID: operationA, ExpectedEpoch: epochA, LifecycleVersion: "1"}
}
func s3Scope(tenant string) contract.RequestScope {
	return contract.RequestScope{OrgID: orgA, TenantID: tenant, LifecycleVersion: "1", Permissions: []string{"menu.read", "order.prepare", "order.confirm", "order.read"}, TimeZone: "Europe/Moscow", SettingsVersion: "1"}
}
func s3Code(t *testing.T, err error, code, outcome string) {
	t.Helper()
	var e *contract.OperationError
	if !errors.As(err, &e) || e.Code != code || e.Outcome != outcome {
		t.Fatalf("error=%v, want %s/%s", err, code, outcome)
	}
}

func TestS3CloseFencesRetainedHandleAndKeepsNeighbor(t *testing.T) {
	r := s3Registry(t, &warehouseStub{})
	for _, tenant := range []string{tenantA, tenantB} {
		if err := r.Register(context.Background(), s3Registration(tenant)); err != nil {
			t.Fatal(err)
		}
	}
	old, _, err := r.Resolve(context.Background(), s3Scope(tenantA))
	if err != nil {
		t.Fatal(err)
	}
	closeRequest := s3Registration(tenantA)
	closeRequest.LifecycleVersion = "2"
	if err = r.Close(context.Background(), closeRequest); err != nil {
		t.Fatal(err)
	}
	if err = r.Close(context.Background(), closeRequest); err != nil {
		t.Fatal(err)
	}
	_, err = old.GetMenu(contract.WithRequestScope(context.Background(), s3Scope(tenantA)))
	s3Code(t, err, "ScopeDeleted", "unavailable")
	_, _, err = r.Resolve(context.Background(), s3Scope(tenantA))
	s3Code(t, err, "ScopeDeleted", "unavailable")
	s3Code(t, r.Register(context.Background(), s3Registration(tenantA)), "ScopeDeleted", "not_committed")
	neighbor, _, err := r.Resolve(context.Background(), s3Scope(tenantB))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = neighbor.GetMenu(contract.WithRequestScope(context.Background(), s3Scope(tenantB))); err != nil {
		t.Fatal(err)
	}
	changed := closeRequest
	changed.OperationID = tenantB
	s3Code(t, r.Close(context.Background(), changed), "CommandConflict", "not_committed")
}

func TestS3CloseBeforeRegistrationAndCanceledCleanupRetry(t *testing.T) {
	r := s3Registry(t, &warehouseStub{})
	req := s3Registration(tenantA)
	req.LifecycleVersion = "2"
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.Close(canceled, req); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled=%v", err)
	}
	if err := r.Close(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	s3Code(t, r.Register(context.Background(), s3Registration(tenantA)), "ScopeDeleted", "not_committed")
	stale := s3Registration(tenantB)
	stale.ExpectedEpoch = peerEpoch
	s3Code(t, r.Register(context.Background(), stale), "RuntimeReset", "indeterminate")
	if err := r.Register(context.Background(), s3Registration(tenantB)); err != nil {
		t.Fatal(err)
	}
}

type s3BlockedWarehouse struct {
	*warehouseStub
	entered chan struct{}
	release chan struct{}
}

func (w *s3BlockedWarehouse) EvaluateOrder(context.Context, contract.EvaluateOrderRequest) (contract.OrderEvaluation, error) {
	close(w.entered)
	<-w.release
	return w.evaluation, nil
}
func TestS3PartialCloseFencesAdmissionUntilInflightDrains(t *testing.T) {
	w := &s3BlockedWarehouse{warehouseStub: &warehouseStub{}, entered: make(chan struct{}), release: make(chan struct{})}
	r, err := NewRegistry(RegistryOptions{Epoch: epochA, Now: time.Now, NewID: func() string { return "p" }, WarehouseFactory: func(context.Context, string) (WarehousePorts, error) {
		return WarehousePorts{Orders: w, Catalog: w, Menu: w, Epoch: peerEpoch}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	reg := s3Registration(tenantA)
	if err = r.Register(context.Background(), reg); err != nil {
		t.Fatal(err)
	}
	rt, _, err := r.Resolve(context.Background(), s3Scope(tenantA))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, e := rt.PrepareOrder(contract.WithRequestScope(context.Background(), s3Scope(tenantA)), contract.PrepareOrderRequest{Positions: []contract.Position{{DishID: "dish", Portions: 1}}})
		done <- e
	}()
	<-w.entered
	req := reg
	req.LifecycleVersion = "2"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err = r.Close(ctx, req); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("partial cleanup=%v", err)
	}
	_, err = rt.GetProducts(contract.WithRequestScope(context.Background(), s3Scope(tenantA)))
	s3Code(t, err, "ScopeDeleted", "unavailable")
	s3Code(t, r.Register(context.Background(), reg), "ScopeDeleted", "not_committed")
	close(w.release)
	if err = <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("inflight result=%v", err)
	}
	if err = r.Close(context.Background(), req); err != nil {
		t.Fatal(err)
	}
}

func TestS3CanceledRegistrationNeverAdmitsRuntime(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &warehouseStub{}
	r, err := NewRegistry(RegistryOptions{Epoch: epochA, Now: time.Now, NewID: func() string { return "p" }, WarehouseFactory: func(context.Context, string) (WarehousePorts, error) {
		cancel()
		return WarehousePorts{Orders: w, Catalog: w, Menu: w, Epoch: peerEpoch}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Register(ctx, s3Registration(tenantA)); !errors.Is(err, context.Canceled) {
		t.Fatalf("late registration=%v", err)
	}
	_, _, err = r.Resolve(context.Background(), s3Scope(tenantA))
	s3Code(t, err, "RuntimeNotReady", "unavailable")
}

func TestS3RestartRegistersEmptyStateAtCurrentEpoch(t *testing.T) {
	w := &warehouseStub{}
	r, err := NewRegistry(RegistryOptions{Epoch: tenantB, Now: time.Now, NewID: func() string { return "p" }, WarehouseFactory: func(context.Context, string) (WarehousePorts, error) {
		return WarehousePorts{Orders: w, Attempts: &s3UnseenReader{}, Catalog: w, Menu: w, Epoch: peerEpoch}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	reg := s3Registration(tenantA)
	s3Code(t, r.Register(context.Background(), reg), "RuntimeReset", "indeterminate")
	reg.ExpectedEpoch = tenantB
	if err = r.Register(context.Background(), reg); err != nil {
		t.Fatal(err)
	}
	rt, info, err := r.Resolve(context.Background(), s3Scope(tenantA))
	if err != nil || info.Epoch != tenantB {
		t.Fatalf("fresh epoch=%+v/%v", info, err)
	}
	_, err = rt.ResolveConfirmation(contract.WithRequestScope(context.Background(), s3Scope(tenantA)), contract.ConfirmOrderRequest{CommandID: "old-command", ProposalID: "old-proposal"})
	s3Code(t, err, "ProposalNotFound", "unknown")
	if err = r.Reconcile(context.Background(), contract.RegistrySnapshot{Revision: "1", Complete: true, Entries: []contract.RegistryRestaurant{{OrgID: orgA, TenantID: tenantA, LifecycleVersion: "1", Lifecycle: "ready"}}}); err != nil {
		t.Fatal(err)
	}
	if !r.Status().Ready {
		t.Fatal("current epoch did not become ready")
	}
}

type s3UnseenReader struct{}

func (*s3UnseenReader) FindOrderAttempt(context.Context, string, string) (contract.OrderAttempt, error) {
	return contract.OrderAttempt{State: contract.OrderAttemptUnseen}, nil
}
