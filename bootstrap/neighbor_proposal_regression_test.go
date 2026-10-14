package bootstrap

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/JustIkra/umir-serving/contract"
)

func TestS3RepairPartialCleanupPreservesNeighborProposal(t *testing.T) {
	at := time.Date(2026, 10, 13, 12, 0, 0, 0, time.FixedZone("MSK", 10800))
	accepted := []contract.AcceptedPosition{{LineNo: 1, DishID: "soup", Portions: 9007199254740993}}
	neighbor := &warehouseStub{evaluation: contract.OrderEvaluation{EvaluatedAt: at, StateVersion: 7, AcceptedPositions: accepted}, commit: contract.CommitResult{Kind: contract.OrderCommitted, Order: &contract.Order{OrderID: "order", ProposalID: "p", Positions: accepted, PlacedAt: at, Allocations: []contract.Allocation{{ProductID: "p", BatchID: "b", Quantity: 9007199254740993}}}}}
	blocked := &s3BlockedWarehouse{warehouseStub: &warehouseStub{}, entered: make(chan struct{}), release: make(chan struct{})}
	registry, err := NewRegistry(RegistryOptions{Epoch: epochA, Now: func() time.Time { return at }, NewID: func() string { return "p" }, WarehouseFactory: func(_ context.Context, tenant string) (WarehousePorts, error) {
		if tenant == tenantA {
			return WarehousePorts{Orders: blocked, Catalog: blocked, Menu: blocked, Epoch: peerEpoch}, nil
		}
		return WarehousePorts{Orders: neighbor, Catalog: neighbor, Menu: neighbor, Epoch: peerEpoch}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tenant := range []string{tenantA, tenantB} {
		if err = registry.Register(context.Background(), s3Registration(tenant)); err != nil {
			t.Fatal(err)
		}
	}
	a, _, err := registry.Resolve(context.Background(), s3Scope(tenantA))
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := registry.Resolve(context.Background(), s3Scope(tenantB))
	if err != nil {
		t.Fatal(err)
	}
	bctx := contract.WithRequestScope(context.Background(), s3Scope(tenantB))
	proposal, err := b.PrepareOrder(bctx, contract.PrepareOrderRequest{Positions: []contract.Position{{DishID: "soup", Portions: 9007199254740993}}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, e := a.PrepareOrder(contract.WithRequestScope(context.Background(), s3Scope(tenantA)), contract.PrepareOrderRequest{Positions: []contract.Position{{DishID: "soup", Portions: 1}}})
		done <- e
	}()
	<-blocked.entered
	closed := false
	defer func() {
		if !closed {
			close(blocked.release)
			<-done
		}
	}()
	req := s3Registration(tenantA)
	req.LifecycleVersion = "2"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err = registry.Close(ctx, req); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("partial close=%v", err)
	}
	confirmation := contract.ConfirmOrderRequest{CommandID: "neighbor-command", ProposalID: proposal.ProposalID}
	result, err := b.ConfirmOrder(bctx, confirmation)
	if err != nil || result.Kind != contract.OrderConfirmed || result.Order.Allocations[0].Quantity != 9007199254740993 || result.Order.Positions[0].Portions != 9007199254740993 {
		t.Fatalf("neighbor during partial close=%+v/%v", result, err)
	}
	close(blocked.release)
	<-done
	closed = true
	if err = registry.Close(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	repeated, err := b.ConfirmOrder(bctx, confirmation)
	if err != nil || repeated.Kind != result.Kind || repeated.Order.OrderID != result.Order.OrderID || !repeated.Order.PlacedAt.Equal(at) || repeated.Order.Allocations[0].Quantity != 9007199254740993 {
		t.Fatalf("neighbor after cleanup retry=%+v/%v", repeated, err)
	}
}
