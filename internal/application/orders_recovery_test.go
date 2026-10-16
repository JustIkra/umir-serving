package application_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/JustIkra/umir-serving/contract"
	"github.com/JustIkra/umir-serving/internal/adapters/memory"
	"github.com/JustIkra/umir-serving/internal/application"
	"github.com/JustIkra/umir-serving/tests/testsupport"
)

type recoveryWarehouse struct {
	*s3ObservedWarehouse
	attempt contract.OrderAttempt
	observe bool
}

func (w *recoveryWarehouse) FindOrderAttempt(ctx context.Context, _, _ string) (contract.OrderAttempt, error) {
	w.mu.Lock()
	result, observation, observe := w.attempt, w.context, w.observe
	if result.Result != nil {
		copy := cloneCommitResult(*result.Result)
		result.Result = &copy
	}
	w.mu.Unlock()
	if observe {
		contract.RecordWarehouseContext(ctx, observation)
	}
	return result, nil
}

func retainedConfirmation(t *testing.T) (context.Context, *application.OrdersService, *recoveryWarehouse, contract.ConfirmOrderRequest) {
	t.Helper()
	store := memory.NewProposalStore()
	fake := newWarehouseFake()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, testsupport.Moscow)
	fake.evaluation = contract.OrderEvaluation{EvaluatedAt: now, StateVersion: 1, RequestedPositions: []contract.Position{{LineNo: 1, DishID: "dish-1", Portions: 2}}, AcceptedPositions: []contract.AcceptedPosition{{LineNo: 1, DishID: "dish-1", Portions: 2}}}
	observation := contract.WarehouseContext{OrgID: "22222222-2222-4222-8222-222222222222", TenantID: "33333333-3333-4333-8333-333333333333", Epoch: "44444444-4444-4444-8444-444444444444", TimeZone: "Europe/Moscow", SettingsVersion: "1", EvaluatedAt: now, StateVersion: 1}
	warehouse := &recoveryWarehouse{s3ObservedWarehouse: &s3ObservedWarehouse{warehouseFake: fake, context: observation}, attempt: contract.OrderAttempt{State: contract.OrderAttemptUnseen}, observe: true}
	service, err := application.NewOrdersServiceWithOptions(store, warehouse, func() time.Time { return now }, func() string { return "proposal-1" }, application.OrdersServiceOptions{LeaseClock: time.Now, Wait: testWait}, warehouse)
	if err != nil {
		t.Fatal(err)
	}
	scope := contract.RequestScope{OrgID: observation.OrgID, TenantID: observation.TenantID, TimeZone: observation.TimeZone, SettingsVersion: observation.SettingsVersion}
	ctx := contract.WithWarehouseBinding(contract.WithRequestScope(context.Background(), scope), observation, false)
	proposal, err := service.PrepareOrder(ctx, contract.PrepareOrderRequest{Positions: []contract.Position{{DishID: "dish-1", Portions: 2}}})
	if err != nil {
		t.Fatal(err)
	}
	request := contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: proposal.ProposalID}
	fake.commitErr = errors.New("synthetic request delivery interrupted")
	if _, err := service.ConfirmOrder(ctx, request); err == nil {
		t.Fatal("interrupted initial commit reported success")
	}
	fake.commitErr = nil
	fake.commitResult = contract.CommitResult{Kind: contract.OrderCommitted, Order: &contract.Order{OrderID: "order-1", ProposalID: proposal.ProposalID, Positions: append([]contract.AcceptedPosition(nil), fake.evaluation.AcceptedPositions...)}}
	return ctx, service, warehouse, request
}

func TestRetainedConfirmRedeliversOnlyTheOriginalUnseenCommit(t *testing.T) {
	ctx, service, warehouse, request := retainedConfirmation(t)
	results := make(chan struct {
		result contract.ConfirmResult
		err    error
	}, 16)
	for range 16 {
		go func() {
			result, err := service.ConfirmOrder(ctx, request)
			results <- struct {
				result contract.ConfirmResult
				err    error
			}{result, err}
		}()
	}
	for range 16 {
		got := <-results
		if got.err != nil || got.result.Kind != contract.OrderConfirmed || got.result.Order == nil || got.result.Order.OrderID != "order-1" {
			t.Fatal("same-identity retry did not converge to the original order", got.err, got.result.Kind)
		}
	}
	requests, calls := warehouse.commits()
	if calls != 2 || !reflect.DeepEqual(requests[0], requests[1]) {
		t.Fatal("recovery changed the saved payload or duplicated concurrent commits", calls)
	}
}

func TestRetainedConfirmKeepsResolvePassiveAndPendingNonMutating(t *testing.T) {
	ctx, service, warehouse, request := retainedConfirmation(t)
	if result, err := service.ResolveConfirmation(ctx, request); err != nil || result.Kind != contract.ConfirmationPending {
		t.Fatal("passive resolution did not retain uncertainty", err)
	}
	warehouse.attempt.State = contract.OrderAttemptPending
	if result, err := service.ConfirmOrder(ctx, request); err != nil || result.Kind != contract.ConfirmationPending {
		t.Fatal("a real pending owner attempt was not retained", err)
	}
	if _, calls := warehouse.commits(); calls != 1 {
		t.Fatal("passive resolution or a pending attempt dispatched another commit", calls)
	}
}

func TestRetainedConfirmRequiresAnObservedEpochForRedelivery(t *testing.T) {
	ctx, service, warehouse, request := retainedConfirmation(t)
	warehouse.observe = false
	if result, err := service.ConfirmOrder(ctx, request); err != nil || result.Kind != contract.ConfirmationPending {
		t.Fatal("missing owner provenance was treated as a known outcome", err)
	}
	if _, calls := warehouse.commits(); calls != 1 {
		t.Fatal("unproven unseen state triggered a commit", calls)
	}
}

func TestRetainedConfirmDoesNotRecommitARecordedOrder(t *testing.T) {
	ctx, service, warehouse, request := retainedConfirmation(t)
	result := cloneCommitResult(warehouse.commitResult)
	warehouse.attempt = contract.OrderAttempt{State: contract.OrderAttemptCompleted, Result: &result}
	confirmed, err := service.ConfirmOrder(ctx, request)
	if err != nil || confirmed.Kind != contract.OrderConfirmed || confirmed.Order == nil || confirmed.Order.OrderID != "order-1" {
		t.Fatal("recorded outcome did not complete the local attempt", err)
	}
	if _, calls := warehouse.commits(); calls != 1 {
		t.Fatal("a recorded order was committed again", calls)
	}
}

func TestRetainedConfirmReportsAnObservedRuntimeReset(t *testing.T) {
	ctx, service, warehouse, request := retainedConfirmation(t)
	warehouse.context.Epoch = "55555555-5555-4555-8555-555555555555"
	_, err := service.ConfirmOrder(ctx, request)
	var reset *contract.OperationError
	if !errors.As(err, &reset) || reset.Code != "RuntimeReset" || reset.Outcome != "indeterminate" {
		t.Fatal("a new owner epoch was treated as recoverable unseen state", err)
	}
	if _, calls := warehouse.commits(); calls != 1 {
		t.Fatal("old confirmation was executed in a new owner epoch", calls)
	}
}
