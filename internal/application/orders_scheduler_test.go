package application_test

import (
	"context"
	"testing"
	"time"

	"github.com/JustIkra/umir-serving/contract"
	"github.com/JustIkra/umir-serving/internal/adapters/memory"
	"github.com/JustIkra/umir-serving/internal/application"
)

func TestProposalLeaseWaitUsesInjectedMechanismClock(t *testing.T) {
	store := memory.NewProposalStore()
	createProposal(t, store, proposalFixture("proposal-1", 1))
	if _, acquired, err := store.AcquireLease(context.Background(), "proposal-1", "other-holder", time.Second); err != nil || !acquired {
		t.Fatalf("AcquireLease() = (%v, %v), want acquired", acquired, err)
	}
	fixedBusinessTime := time.Date(2026, time.October, 10, 12, 0, 0, 0, time.UTC)
	service, err := application.NewOrdersServiceWithOptions(
		store,
		newWarehouseFake(),
		func() time.Time { return fixedBusinessTime },
		func() string { return "replacement" },
		application.OrdersServiceOptions{
			LeaseTTL:   time.Second,
			LeaseWait:  40 * time.Millisecond,
			LeaseClock: time.Now,
			Wait:       testWait,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	result, err := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "proposal-1"})
	elapsed := time.Since(started)
	if err != nil || result.Kind != contract.ConfirmationPending {
		t.Fatalf("ConfirmOrder() = (%+v, %v), want ConfirmationPending", result, err)
	}
	if elapsed < 35*time.Millisecond || elapsed > 500*time.Millisecond {
		t.Fatalf("lease wait elapsed %v, want about 40ms", elapsed)
	}
}
