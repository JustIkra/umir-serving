package application_test

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/JustIkra/umir-serving/contract"
	"github.com/JustIkra/umir-serving/internal/adapters/memory"
	"github.com/JustIkra/umir-serving/internal/application"
)

func TestWaitingForProposalLockHonorsContext(t *testing.T) {
	waiters := map[string]func(context.Context, *application.OrdersService) error{
		"confirm": func(ctx context.Context, service *application.OrdersService) error {
			_, err := service.ConfirmOrder(ctx, contract.ConfirmOrderRequest{CommandID: "command-2", ProposalID: "proposal-1"})
			return err
		},
		"resolve": func(ctx context.Context, service *application.OrdersService) error {
			_, err := service.ResolveConfirmation(ctx, contract.ConfirmOrderRequest{CommandID: "command-2", ProposalID: "proposal-1"})
			return err
		},
	}
	for name, wait := range waiters {
		t.Run(name, func(t *testing.T) {
			store := memory.NewProposalStore()
			warehouse := newWarehouseFake()
			reader := &attemptReaderFake{result: contract.OrderAttempt{State: contract.OrderAttemptUnseen}}
			service := newOrdersServiceWithReader(t, store, warehouse, reader)
			createProposal(t, store, proposalFixture("proposal-1", 2))
			entered := make(chan struct{})
			release := make(chan struct{})
			warehouse.commitHook = func(contract.CommitOrderRequest) (contract.CommitResult, error) {
				close(entered)
				<-release
				return contract.CommitResult{}, errors.New("response lost")
			}
			baseline := runtime.NumGoroutine()

			firstDone := make(chan error, 1)
			go func() {
				_, err := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "proposal-1"})
				firstDone <- err
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("first ConfirmOrder() never reached the warehouse")
			}

			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			started := time.Now()
			waitDone := make(chan error, 1)
			go func() { waitDone <- wait(ctx, service) }()
			var err error
			select {
			case err = <-waitDone:
			case <-time.After(2 * time.Second):
				close(release)
				t.Fatal("waiting call did not honor its 100ms deadline")
			}
			elapsed := time.Since(started)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("waiting call error = %v, want context deadline exceeded", err)
			}
			var operationError *contract.OperationError
			if errors.As(err, &operationError) {
				t.Fatalf("waiting call error = %v, want the bare context error, not an operation outcome", err)
			}
			if elapsed > 150*time.Millisecond {
				t.Fatalf("waiting call returned after %v, want at most 150ms", elapsed)
			}

			close(release)
			select {
			case err := <-firstDone:
				if err == nil {
					t.Fatal("first ConfirmOrder() unexpectedly succeeded")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("first ConfirmOrder() did not finish after release")
			}
			if _, calls := warehouse.commits(); calls != 1 {
				t.Fatalf("commit calls = %d, want only the first command to reach the warehouse", calls)
			}
			if reader.calls > 2 {
				t.Fatalf("attempt lookups = %d, the cancelled waiter must not reach the warehouse", reader.calls)
			}
			deadline := time.Now().Add(5 * time.Second)
			for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if got := runtime.NumGoroutine(); got > baseline {
				t.Fatalf("goroutines = %d, want at most the initial %d", got, baseline)
			}
		})
	}
}

func TestProposalLockIsReusableAfterCancelledWaiter(t *testing.T) {
	store := memory.NewProposalStore()
	warehouse := newWarehouseFake()
	service := newOrdersService(t, store, warehouse)
	createProposal(t, store, proposalFixture("proposal-1", 2))
	entered := make(chan struct{})
	release := make(chan struct{})
	warehouse.commitHook = func(contract.CommitOrderRequest) (contract.CommitResult, error) {
		close(entered)
		<-release
		return contract.CommitResult{}, errors.New("response lost")
	}
	firstDone := make(chan error, 1)
	go func() {
		_, err := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "proposal-1"})
		firstDone <- err
	}()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	waiterDone := make(chan error, 1)
	go func() {
		_, err := service.ConfirmOrder(ctx, contract.ConfirmOrderRequest{CommandID: "command-2", ProposalID: "proposal-1"})
		waiterDone <- err
	}()
	select {
	case err := <-waiterDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled waiter error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("cancelled waiter did not return")
	}
	close(release)
	<-firstDone
	// The owner released the lock and the cancelled waiter left nothing
	// behind: the same command is served again without blocking.
	warehouse.commitHook = nil
	retryDone := make(chan error, 1)
	go func() {
		_, err := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "proposal-1"})
		retryDone <- err
	}()
	select {
	case <-retryDone:
	case <-time.After(5 * time.Second):
		t.Fatal("ConfirmOrder() blocked after the cancelled waiter left")
	}
}
