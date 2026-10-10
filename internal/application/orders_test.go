package application_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/JustIkra/umir-serving/contract"
	"github.com/JustIkra/umir-serving/internal/adapters/memory"
	"github.com/JustIkra/umir-serving/internal/application"
	"github.com/JustIkra/umir-serving/internal/domain"
	"github.com/JustIkra/umir-serving/tests/testsupport"
)

type warehouseFake struct {
	mu                sync.Mutex
	evaluation        contract.OrderEvaluation
	commitResult      contract.CommitResult
	commitErr         error
	recorded          map[string]contract.CommandRecord
	ordersByProposal  map[string]contract.Order
	findCommandErr    error
	findProposalErr   error
	evaluateErr       error
	evaluateCalls     int
	evaluateRequests  []contract.EvaluateOrderRequest
	commitCalls       int
	commitRequests    []contract.CommitOrderRequest
	findCommandCalls  int
	findProposalCalls int
	commitHook        func(contract.CommitOrderRequest) (contract.CommitResult, error)
}

func newWarehouseFake() *warehouseFake {
	return &warehouseFake{
		recorded:         make(map[string]contract.CommandRecord),
		ordersByProposal: make(map[string]contract.Order),
	}
}

func (w *warehouseFake) EvaluateOrder(_ context.Context, request contract.EvaluateOrderRequest) (contract.OrderEvaluation, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.evaluateCalls++
	request.Positions = append([]contract.Position(nil), request.Positions...)
	w.evaluateRequests = append(w.evaluateRequests, request)
	return cloneEvaluation(w.evaluation), w.evaluateErr
}

func (w *warehouseFake) CommitAcceptedOrder(_ context.Context, request contract.CommitOrderRequest) (contract.CommitResult, error) {
	w.mu.Lock()
	w.commitCalls++
	w.commitRequests = append(w.commitRequests, cloneCommitRequest(request))
	hook := w.commitHook
	result, err := w.commitResult, w.commitErr
	w.mu.Unlock()
	if hook != nil {
		return hook(request)
	}
	return cloneCommitResult(result), err
}

func (w *warehouseFake) FindRecordedCommand(_ context.Context, commandID string) (contract.CommandRecord, bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.findCommandCalls++
	if w.findCommandErr != nil {
		return contract.CommandRecord{}, false, w.findCommandErr
	}
	record, ok := w.recorded[commandID]
	return cloneCommandRecord(record), ok, nil
}

func (w *warehouseFake) FindOrderByProposal(_ context.Context, proposalID string) (contract.Order, bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.findProposalCalls++
	if w.findProposalErr != nil {
		return contract.Order{}, false, w.findProposalErr
	}
	order, ok := w.ordersByProposal[proposalID]
	return cloneOrder(order), ok, nil
}

func (w *warehouseFake) recordedCommandCalls() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.findCommandCalls
}

func (w *warehouseFake) proposalLookupCalls() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.findProposalCalls
}

func (w *warehouseFake) evaluationInputs() []contract.EvaluateOrderRequest {
	w.mu.Lock()
	defer w.mu.Unlock()
	requests := make([]contract.EvaluateOrderRequest, len(w.evaluateRequests))
	for i, request := range w.evaluateRequests {
		requests[i] = request
		requests[i].Positions = append([]contract.Position(nil), request.Positions...)
	}
	return requests
}

func (w *warehouseFake) commits() ([]contract.CommitOrderRequest, int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	requests := make([]contract.CommitOrderRequest, len(w.commitRequests))
	for i := range w.commitRequests {
		requests[i] = cloneCommitRequest(w.commitRequests[i])
	}
	return requests, w.commitCalls
}

func TestPrepareDoesNotReserve(t *testing.T) {
	store := memory.NewProposalStore()
	warehouse := newWarehouseFake()
	warehouse.evaluation = contract.OrderEvaluation{
		EvaluatedAt:        time.Date(2026, time.October, 10, 12, 0, 0, 0, testsupport.Moscow),
		StateVersion:       9,
		RequestedPositions: []contract.Position{{LineNo: 1, DishID: "dish-1", Portions: 2}},
		AcceptedPositions:  []contract.AcceptedPosition{{LineNo: 1, DishID: "dish-1", Portions: 2}},
	}
	service := newOrdersService(t, store, warehouse)

	proposal, err := service.PrepareOrder(context.Background(), contract.PrepareOrderRequest{
		Positions: []contract.Position{{DishID: "dish-1", Portions: 2}},
	})
	if err != nil {
		t.Fatalf("PrepareOrder() error = %v", err)
	}
	if proposal.ProposalID != "generated-proposal-1" || proposal.StateVersion != 9 || len(proposal.AcceptedPositions) != 1 {
		t.Fatalf("PrepareOrder() proposal = %+v", proposal)
	}
	record, ok, err := store.Get(context.Background(), proposal.ProposalID)
	if err != nil || !ok {
		t.Fatalf("stored proposal lookup = (%v, %v), want present without error", ok, err)
	}
	if record.PendingCommandID != "" || record.Proposal.ProposalID != proposal.ProposalID {
		t.Fatalf("prepared proposal record = %+v", record)
	}
	if _, calls := warehouse.commits(); calls != 0 {
		t.Fatalf("PrepareOrder() called CommitAcceptedOrder %d times", calls)
	}
}

func TestPrepareAssignsLineNumbersAndReturnsIndependentCopy(t *testing.T) {
	store := memory.NewProposalStore()
	warehouse := newWarehouseFake()
	warehouse.evaluation = contract.OrderEvaluation{
		EvaluatedAt:       time.Date(2026, time.October, 10, 12, 0, 0, 0, testsupport.Moscow),
		StateVersion:      9,
		AcceptedPositions: []contract.AcceptedPosition{{LineNo: 1, DishID: "dish-1", Portions: 2}, {LineNo: 2, DishID: "dish-2", Portions: 1}},
		Changes:           []contract.Change{{LineNo: 2, DishID: "dish-2", RequestedPortions: 3, AcceptedPortions: 1, Deficits: []contract.Deficit{{ProductID: "product-1", RequiredQuantity: 3000, AvailableQuantity: 1000}}}},
	}
	service := newOrdersService(t, store, warehouse)
	proposal, err := service.PrepareOrder(context.Background(), contract.PrepareOrderRequest{Positions: []contract.Position{
		{LineNo: 90, DishID: "dish-1", Portions: 2},
		{LineNo: 91, DishID: "dish-2", Portions: 3},
	}})
	if err != nil {
		t.Fatalf("PrepareOrder() error = %v", err)
	}
	inputs := warehouse.evaluationInputs()
	if len(inputs) != 1 || len(inputs[0].Positions) != 2 || inputs[0].Positions[0].LineNo != 1 || inputs[0].Positions[1].LineNo != 2 {
		t.Fatalf("EvaluateOrder() inputs = %+v, want sequential line numbers", inputs)
	}
	proposal.RequestedPositions[0].DishID = "changed"
	proposal.Changes[0].Deficits[0].AvailableQuantity = 0
	record, ok, err := store.Get(context.Background(), "generated-proposal-1")
	if err != nil || !ok || record.Proposal.RequestedPositions[0].DishID != "dish-1" || record.Proposal.Changes[0].Deficits[0].AvailableQuantity != 1000 {
		t.Fatalf("stored proposal changed through returned value: (%+v, %v, %v)", record, ok, err)
	}
}

func TestPrepareWarehouseErrorDoesNotSaveProposal(t *testing.T) {
	store := memory.NewProposalStore()
	warehouse := newWarehouseFake()
	warehouse.evaluateErr = errors.New("evaluation unavailable")
	service := newOrdersService(t, store, warehouse)
	_, err := service.PrepareOrder(context.Background(), contract.PrepareOrderRequest{Positions: []contract.Position{{DishID: "dish-1", Portions: 1}}})
	if !errors.Is(err, warehouse.evaluateErr) {
		t.Fatalf("PrepareOrder() error = %v, want evaluation error", err)
	}
	if _, ok, getErr := store.Get(context.Background(), "generated-proposal-1"); getErr != nil || ok {
		t.Fatalf("failed evaluation left proposal: present=%v err=%v", ok, getErr)
	}
}

func TestConfirmReducesAgain(t *testing.T) {
	store := memory.NewProposalStore()
	warehouse := newWarehouseFake()
	warehouse.commitResult = contract.CommitResult{
		Kind: contract.RecheckRequired,
		OrderEvaluation: &contract.OrderEvaluation{
			EvaluatedAt:        time.Date(2026, time.October, 10, 12, 5, 0, 0, testsupport.Moscow),
			StateVersion:       12,
			RequestedPositions: []contract.Position{{LineNo: 3, DishID: "dish-1", Portions: 2}},
			AcceptedPositions:  []contract.AcceptedPosition{{LineNo: 3, DishID: "dish-1", Portions: 1}},
			Changes:            []contract.Change{{LineNo: 3, DishID: "dish-1", RequestedPortions: 2, AcceptedPortions: 1, Deficits: []contract.Deficit{{ProductID: "product-1", RequiredQuantity: 2000, AvailableQuantity: 1000}}}},
		},
	}
	service := newOrdersService(t, store, warehouse)
	createProposal(t, store, proposalFixture("proposal-1", 2))

	result, err := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "proposal-1"})
	if err != nil {
		t.Fatalf("ConfirmOrder() error = %v", err)
	}
	if result.Kind != contract.RequiresConfirmation || result.Proposal == nil || result.Proposal.ProposalID != "generated-proposal-1" {
		t.Fatalf("ConfirmOrder() result = %+v", result)
	}
	if result.Proposal.RequestedPositions[0].LineNo != 3 || result.Proposal.RequestedPositions[0].Portions != 2 {
		t.Fatalf("replacement request did not preserve accepted line: %+v", result.Proposal.RequestedPositions)
	}
	if result.Proposal.AcceptedPositions[0].Portions != 1 || result.Proposal.Changes[0].Deficits[0].AvailableQuantity != 1000 {
		t.Fatalf("replacement did not preserve warehouse reduction and explanation: %+v", result.Proposal)
	}
	old, ok, err := store.Get(context.Background(), "proposal-1")
	if err != nil || !ok || old.SupersededBy != "generated-proposal-1" || old.PendingCommandID != "" {
		t.Fatalf("old proposal record = (%+v, %v, %v)", old, ok, err)
	}
	requests, calls := warehouse.commits()
	if calls != 1 || requests[0].AcceptedPositions[0].Portions != 2 || requests[0].AcceptedPositions[0].LineNo != 3 {
		t.Fatalf("warehouse commit request = %+v, calls %d", requests, calls)
	}
}

func TestConfirmDoesNotGrow(t *testing.T) {
	store := memory.NewProposalStore()
	warehouse := newWarehouseFake()
	warehouse.commitResult = contract.CommitResult{
		Kind:  contract.OrderCommitted,
		Order: &contract.Order{OrderID: "order-1", ProposalID: "proposal-1", Positions: []contract.AcceptedPosition{{LineNo: 3, DishID: "dish-1", Portions: 1}}},
	}
	service := newOrdersService(t, store, warehouse)
	createProposal(t, store, proposalFixture("proposal-1", 1))

	result, err := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "proposal-1"})
	if err != nil {
		t.Fatalf("ConfirmOrder() error = %v", err)
	}
	if result.Kind != contract.OrderConfirmed || result.Order == nil || result.Order.Positions[0].Portions != 1 {
		t.Fatalf("ConfirmOrder() result = %+v", result)
	}
	requests, calls := warehouse.commits()
	if calls != 1 || requests[0].AcceptedPositions[0].Portions != 1 {
		t.Fatalf("ConfirmOrder() grew accepted positions: requests=%+v calls=%d", requests, calls)
	}
}

func TestConfirmLostResponse(t *testing.T) {
	store := memory.NewProposalStore()
	warehouse := newWarehouseFake()
	warehouse.commitErr = errors.New("response lost")
	service := newOrdersService(t, store, warehouse)
	createProposal(t, store, proposalFixture("proposal-1", 2))

	first, err := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "proposal-1"})
	if err == nil || first.Kind != "" {
		t.Fatalf("first ConfirmOrder() = (%+v, %v), want technical error", first, err)
	}
	record, ok, getErr := store.Get(context.Background(), "proposal-1")
	if getErr != nil || !ok || record.PendingCommandID != "command-1" {
		t.Fatalf("pending record after lost response = (%+v, %v, %v)", record, ok, getErr)
	}

	order := contract.Order{OrderID: "order-1", ProposalID: "proposal-1", Positions: []contract.AcceptedPosition{{LineNo: 3, DishID: "dish-1", Portions: 2}}}
	warehouse.mu.Lock()
	warehouse.recorded["command-1"] = commandRecord("command-1", "proposal-1", order)
	warehouse.ordersByProposal["proposal-1"] = order
	warehouse.commitErr = nil
	warehouse.mu.Unlock()
	second, err := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "proposal-1"})
	if err != nil || second.Kind != contract.OrderConfirmed || second.Order == nil || second.Order.OrderID != "order-1" {
		t.Fatalf("retry ConfirmOrder() = (%+v, %v)", second, err)
	}
	_, calls := warehouse.commits()
	if calls != 1 {
		t.Fatalf("retry called warehouse commit again: %d calls", calls)
	}
}

func TestDifferentCommandWhilePending(t *testing.T) {
	store := memory.NewProposalStore()
	warehouse := newWarehouseFake()
	warehouse.commitErr = errors.New("outcome unknown")
	service := newOrdersService(t, store, warehouse)
	createProposal(t, store, proposalFixture("proposal-1", 2))

	_, err := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "proposal-1"})
	if err == nil {
		t.Fatal("first ConfirmOrder() unexpectedly succeeded")
	}
	result, err := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-2", ProposalID: "proposal-1"})
	if err != nil || result.Kind != contract.ConfirmationPending {
		t.Fatalf("second ConfirmOrder() = (%+v, %v), want ConfirmationPending", result, err)
	}
	_, calls := warehouse.commits()
	if calls != 1 {
		t.Fatalf("different command reached warehouse: %d commit calls", calls)
	}
}

func TestRecordedOrderBeforeProposalLookup(t *testing.T) {
	store := memory.NewProposalStore()
	warehouse := newWarehouseFake()
	order := contract.Order{OrderID: "order-1", ProposalID: "proposal-1", Positions: []contract.AcceptedPosition{{LineNo: 3, DishID: "dish-1", Portions: 2}}}
	warehouse.recorded["command-1"] = commandRecord("command-1", "proposal-1", order)
	warehouse.findProposalErr = errors.New("proposal lookup should not run")
	service := newOrdersService(t, store, warehouse)

	result, err := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "proposal-1"})
	if err != nil || result.Kind != contract.OrderConfirmed || result.Order == nil || result.Order.OrderID != "order-1" {
		t.Fatalf("ConfirmOrder() = (%+v, %v)", result, err)
	}
	if warehouse.proposalLookupCalls() != 0 {
		t.Fatalf("recorded order performed proposal lookup %d times", warehouse.proposalLookupCalls())
	}
}

func TestCancelledAfterWarehouseCommit(t *testing.T) {
	store := memory.NewProposalStore()
	warehouse := newWarehouseFake()
	ctx, cancel := context.WithCancel(context.Background())
	order := contract.Order{OrderID: "order-1", ProposalID: "proposal-1", Positions: []contract.AcceptedPosition{{LineNo: 3, DishID: "dish-1", Portions: 2}}}
	warehouse.commitHook = func(request contract.CommitOrderRequest) (contract.CommitResult, error) {
		warehouse.mu.Lock()
		warehouse.recorded[request.CommandID] = commandRecord(request.CommandID, request.ProposalID, order)
		warehouse.ordersByProposal[request.ProposalID] = order
		warehouse.mu.Unlock()
		cancel()
		return contract.CommitResult{}, context.Canceled
	}
	service := newOrdersService(t, store, warehouse)
	createProposal(t, store, proposalFixture("proposal-1", 2))

	_, err := service.ConfirmOrder(ctx, contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "proposal-1"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ConfirmOrder() error = %v, want context.Canceled", err)
	}
	record, ok, getErr := store.Get(context.Background(), "proposal-1")
	if getErr != nil || !ok || record.PendingCommandID != "command-1" {
		t.Fatalf("pending record after cancellation = (%+v, %v, %v)", record, ok, getErr)
	}
	retry, retryErr := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "proposal-1"})
	if retryErr != nil || retry.Kind != contract.OrderConfirmed || retry.Order == nil || retry.Order.OrderID != "order-1" {
		t.Fatalf("retry after cancellation = (%+v, %v), want committed order", retry, retryErr)
	}
	_, calls := warehouse.commits()
	if calls != 1 {
		t.Fatalf("retry after cancellation called CommitAcceptedOrder %d times", calls)
	}
}

func TestConfirmStableRepeatReturnsSameOrderWithoutCommit(t *testing.T) {
	store := memory.NewProposalStore()
	warehouse := newWarehouseFake()
	order := contract.Order{OrderID: "order-1", ProposalID: "proposal-1", Positions: []contract.AcceptedPosition{{LineNo: 3, DishID: "dish-1", Portions: 2}}}
	warehouse.commitHook = func(request contract.CommitOrderRequest) (contract.CommitResult, error) {
		warehouse.mu.Lock()
		warehouse.recorded[request.CommandID] = commandRecord(request.CommandID, request.ProposalID, order)
		warehouse.ordersByProposal[request.ProposalID] = order
		warehouse.mu.Unlock()
		return contract.CommitResult{Kind: contract.OrderCommitted, Order: &order}, nil
	}
	service := newOrdersService(t, store, warehouse)
	createProposal(t, store, proposalFixture("proposal-1", 2))
	request := contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "proposal-1"}

	first, err := service.ConfirmOrder(context.Background(), request)
	if err != nil || first.Kind != contract.OrderConfirmed || first.Order == nil {
		t.Fatalf("first ConfirmOrder() = (%+v, %v)", first, err)
	}
	first.Order.Positions[0].Portions = 99
	second, err := service.ConfirmOrder(context.Background(), request)
	if err != nil || second.Kind != contract.OrderConfirmed || second.Order == nil || second.Order.OrderID != "order-1" || second.Order.Positions[0].Portions != 2 {
		t.Fatalf("repeated ConfirmOrder() = (%+v, %v), want independent stored order", second, err)
	}
	_, calls := warehouse.commits()
	if calls != 1 {
		t.Fatalf("stable repeat called CommitAcceptedOrder %d times", calls)
	}
}

func TestConfirmLookupErrorsAreNotAbsence(t *testing.T) {
	store := memory.NewProposalStore()
	warehouse := newWarehouseFake()
	warehouse.findCommandErr = errors.New("command read failed")
	service := newOrdersService(t, store, warehouse)
	createProposal(t, store, proposalFixture("proposal-1", 2))

	_, err := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "proposal-1"})
	if !errors.Is(err, warehouse.findCommandErr) {
		t.Fatalf("ConfirmOrder() error = %v, want command lookup failure", err)
	}
	_, calls := warehouse.commits()
	if calls != 0 {
		t.Fatalf("lookup failure called warehouse commit %d times", calls)
	}
}

func TestConfirmProposalLookupErrorsAreNotAbsence(t *testing.T) {
	store := memory.NewProposalStore()
	warehouse := newWarehouseFake()
	warehouse.findProposalErr = errors.New("proposal order lookup failed")
	service := newOrdersService(t, store, warehouse)
	createProposal(t, store, proposalFixture("proposal-1", 1))

	_, err := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "proposal-1"})
	if !errors.Is(err, warehouse.findProposalErr) {
		t.Fatalf("ConfirmOrder() error = %v, want proposal lookup failure", err)
	}
	_, calls := warehouse.commits()
	if calls != 0 {
		t.Fatalf("proposal lookup failure called CommitAcceptedOrder %d times", calls)
	}
}

func TestConfirmRecordedIdentityConflictStopsBeforeProposalLookup(t *testing.T) {
	store := memory.NewProposalStore()
	warehouse := newWarehouseFake()
	warehouse.recorded["command-1"] = contract.CommandRecord{
		CommandID:          "command-1",
		CommandType:        "ConfirmOrder",
		CommandFingerprint: "different-fingerprint",
	}
	warehouse.findProposalErr = errors.New("proposal lookup must not run")
	service := newOrdersService(t, store, warehouse)

	_, err := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "proposal-1"})
	if !errors.Is(err, contract.ErrCommandIdentityConflict) {
		t.Fatalf("ConfirmOrder() error = %v, want ErrCommandIdentityConflict", err)
	}
	if warehouse.proposalLookupCalls() != 0 {
		t.Fatalf("identity conflict performed proposal lookup %d times", warehouse.proposalLookupCalls())
	}
}

func TestAlreadyOrderedProposalReturnsOrderForDifferentCommand(t *testing.T) {
	warehouse := newWarehouseFake()
	order := contract.Order{OrderID: "order-1", ProposalID: "proposal-1", Positions: []contract.AcceptedPosition{{LineNo: 3, DishID: "dish-1", Portions: 2}}}
	warehouse.ordersByProposal["proposal-1"] = order
	service := newOrdersService(t, memory.NewProposalStore(), warehouse)

	result, err := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-2", ProposalID: "proposal-1"})
	if err != nil || result.Kind != contract.OrderConfirmed || result.Order == nil || result.Order.OrderID != "order-1" {
		t.Fatalf("ConfirmOrder() = (%+v, %v), want existing order", result, err)
	}
	_, calls := warehouse.commits()
	if calls != 0 {
		t.Fatalf("existing proposal order caused %d new commits", calls)
	}
}

func TestKnownInventoryUnavailableClearsPending(t *testing.T) {
	store := memory.NewProposalStore()
	warehouse := newWarehouseFake()
	warehouse.commitErr = contract.ErrInventoryUnavailable
	service := newOrdersService(t, store, warehouse)
	createProposal(t, store, proposalFixture("proposal-1", 1))

	_, err := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "proposal-1"})
	if !errors.Is(err, contract.ErrInventoryUnavailable) {
		t.Fatalf("ConfirmOrder() error = %v, want ErrInventoryUnavailable", err)
	}
	record, ok, getErr := store.Get(context.Background(), "proposal-1")
	if getErr != nil || !ok || record.PendingCommandID != "" {
		t.Fatalf("known unavailable left pending state: (%+v, %v, %v)", record, ok, getErr)
	}
}

func TestReplacementSaveFailureLeavesOldProposalActive(t *testing.T) {
	store := memory.NewProposalStore()
	warehouse := newWarehouseFake()
	warehouse.commitResult = contract.CommitResult{Kind: contract.RecheckRequired, OrderEvaluation: &contract.OrderEvaluation{
		EvaluatedAt:       time.Date(2026, time.October, 10, 12, 5, 0, 0, testsupport.Moscow),
		StateVersion:      12,
		AcceptedPositions: []contract.AcceptedPosition{{LineNo: 3, DishID: "dish-1", Portions: 1}},
		Changes:           []contract.Change{{LineNo: 3, DishID: "dish-1", RequestedPortions: 2, AcceptedPortions: 1, Deficits: []contract.Deficit{{ProductID: "product-1", RequiredQuantity: 2000, AvailableQuantity: 1000}}}},
	}}
	createProposal(t, store, proposalFixture("proposal-1", 2))
	createProposal(t, store, proposalFixture("generated-proposal-1", 1))
	service := newOrdersService(t, store, warehouse)

	result, err := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "proposal-1"})
	if err == nil || result.Kind != "" {
		t.Fatalf("ConfirmOrder() = (%+v, %v), want replacement save error", result, err)
	}
	old, ok, getErr := store.Get(context.Background(), "proposal-1")
	if getErr != nil || !ok || old.SupersededBy != "" || old.PendingCommandID != "" {
		t.Fatalf("old proposal after failed replacement = (%+v, %v, %v)", old, ok, getErr)
	}
}

func TestConfirmUnknownProposal(t *testing.T) {
	service := newOrdersService(t, memory.NewProposalStore(), newWarehouseFake())
	_, err := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "missing"})
	if !errors.Is(err, contract.ErrProposalNotFound) {
		t.Fatalf("ConfirmOrder() error = %v, want ErrProposalNotFound", err)
	}
}

func TestConfirmSupersededProposal(t *testing.T) {
	store := memory.NewProposalStore()
	service := newOrdersService(t, store, newWarehouseFake())
	createProposal(t, store, proposalFixture("proposal-1", 2))
	if err := store.MarkPending(context.Background(), "proposal-1", "command-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Replace(context.Background(), "proposal-1", "command-1", proposalFixture("proposal-2", 1)); err != nil {
		t.Fatal(err)
	}

	result, err := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-2", ProposalID: "proposal-1"})
	if err != nil || result.Kind != contract.ProposalSuperseded || result.SupersededBy != "proposal-2" {
		t.Fatalf("ConfirmOrder() = (%+v, %v), want superseded proposal-2", result, err)
	}
}

func TestConfirmConcurrentAttemptsSerializeWholeWarehouseCall(t *testing.T) {
	store := memory.NewProposalStore()
	warehouse := newWarehouseFake()
	entered := make(chan struct{})
	release := make(chan struct{})
	order := contract.Order{OrderID: "order-1", ProposalID: "proposal-1", Positions: []contract.AcceptedPosition{{LineNo: 3, DishID: "dish-1", Portions: 2}}}
	warehouse.commitHook = func(request contract.CommitOrderRequest) (contract.CommitResult, error) {
		close(entered)
		<-release
		warehouse.mu.Lock()
		warehouse.recorded[request.CommandID] = commandRecord(request.CommandID, request.ProposalID, order)
		warehouse.ordersByProposal[request.ProposalID] = order
		warehouse.mu.Unlock()
		return contract.CommitResult{}, errors.New("response lost")
	}
	service := newOrdersService(t, store, warehouse)
	createProposal(t, store, proposalFixture("proposal-1", 2))

	firstDone := make(chan error, 1)
	go func() {
		_, err := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "proposal-1"})
		firstDone <- err
	}()
	<-entered
	secondDone := make(chan contract.ConfirmResult, 1)
	go func() {
		result, _ := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-2", ProposalID: "proposal-1"})
		secondDone <- result
	}()
	close(release)
	if err := <-firstDone; err == nil {
		t.Fatal("first ConfirmOrder() unexpectedly succeeded")
	}
	if result := <-secondDone; result.Kind != contract.OrderConfirmed {
		t.Fatalf("concurrent second result = %+v, want recorded order", result)
	}
	_, calls := warehouse.commits()
	if calls != 1 {
		t.Fatalf("serialized attempts made %d commit calls", calls)
	}
}

func TestConfirmEmptyOrderDoesNotCommit(t *testing.T) {
	store := memory.NewProposalStore()
	warehouse := newWarehouseFake()
	proposal := proposalFixture("proposal-1", 0)
	proposal.AcceptedPositions = nil
	createProposal(t, store, proposal)
	service := newOrdersService(t, store, warehouse)

	result, err := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "proposal-1"})
	if err != nil || result.Kind != contract.EmptyOrder {
		t.Fatalf("ConfirmOrder() = (%+v, %v), want EmptyOrder", result, err)
	}
	_, calls := warehouse.commits()
	if calls != 0 {
		t.Fatalf("empty order called warehouse commit %d times", calls)
	}
}

func TestConfirmRejectsUnknownRecordedResultKind(t *testing.T) {
	store := memory.NewProposalStore()
	warehouse := newWarehouseFake()
	warehouse.recorded["command-1"] = contract.CommandRecord{
		CommandID:          "command-1",
		CommandType:        "ConfirmOrder",
		CommandFingerprint: confirmFingerprint("proposal-1"),
		Result:             contract.CommandResult{Kind: "FutureKind"},
	}
	service := newOrdersService(t, store, warehouse)

	_, err := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "proposal-1"})
	if err == nil || err.Error() != `unsupported recorded command result kind "FutureKind"` {
		t.Fatalf("ConfirmOrder() error = %v, want unknown result kind schema error", err)
	}
}

func TestConfirmCommandFingerprintMatchesWarehouseConvention(t *testing.T) {
	if got, want := confirmFingerprint("p1"), "67610c5d5f3bb1111e064b8ca1f6c8e020c17f3d3dd9651ebb3a3fc69d9ac3b5"; got != want {
		t.Fatalf("confirmFingerprint(p1) = %q, want %q", got, want)
	}
}

func newOrdersService(t *testing.T, store application.ProposalStore, warehouse *warehouseFake) *application.OrdersService {
	t.Helper()
	ids := testsupport.NewIDs("generated-proposal-")
	service, err := application.NewOrdersService(store, warehouse, func() time.Time {
		return time.Date(2026, time.October, 10, 12, 0, 0, 0, testsupport.Moscow)
	}, ids.Next)
	if err != nil {
		t.Fatalf("NewOrdersService() error = %v", err)
	}
	return service
}

func proposalFixture(proposalID string, portions int64) domain.OrderProposal {
	proposal := testsupport.Proposal(proposalID)
	if portions == 0 {
		proposal.AcceptedPositions = nil
		return proposal
	}
	proposal.AcceptedPositions[0].Portions = portions
	proposal.RequestedPositions[0].Portions = portions
	return proposal
}

func createProposal(t *testing.T, store application.ProposalStore, proposal domain.OrderProposal) {
	t.Helper()
	if err := store.Create(context.Background(), proposal); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
}

func commandRecord(commandID, proposalID string, order contract.Order) contract.CommandRecord {
	return contract.CommandRecord{
		CommandID:          commandID,
		CommandType:        "ConfirmOrder",
		CommandFingerprint: confirmFingerprint(proposalID),
		Result:             contract.CommandResult{Kind: contract.OrderPlaced, Order: &order},
	}
}

func confirmFingerprint(proposalID string) string {
	input, _ := json.Marshal(struct {
		ProposalID string `json:"proposalID"`
	}{ProposalID: proposalID})
	digest := sha256.Sum256(append([]byte("ConfirmOrder\x00"), input...))
	return hex.EncodeToString(digest[:])
}

func cloneEvaluation(evaluation contract.OrderEvaluation) contract.OrderEvaluation {
	evaluation.RequestedPositions = append([]contract.Position(nil), evaluation.RequestedPositions...)
	evaluation.AcceptedPositions = append([]contract.AcceptedPosition(nil), evaluation.AcceptedPositions...)
	evaluation.Changes = append([]contract.Change(nil), evaluation.Changes...)
	for i := range evaluation.Changes {
		evaluation.Changes[i].Deficits = append([]contract.Deficit(nil), evaluation.Changes[i].Deficits...)
	}
	return evaluation
}

func cloneCommitRequest(request contract.CommitOrderRequest) contract.CommitOrderRequest {
	request.AcceptedPositions = append([]contract.AcceptedPosition(nil), request.AcceptedPositions...)
	return request
}

func cloneCommitResult(result contract.CommitResult) contract.CommitResult {
	if result.Order != nil {
		order := cloneOrder(*result.Order)
		result.Order = &order
	}
	if result.OrderEvaluation != nil {
		evaluation := cloneEvaluation(*result.OrderEvaluation)
		result.OrderEvaluation = &evaluation
	}
	return result
}

func cloneOrder(order contract.Order) contract.Order {
	order.Positions = append([]contract.AcceptedPosition(nil), order.Positions...)
	order.Allocations = append([]contract.Allocation(nil), order.Allocations...)
	return order
}

func cloneCommandRecord(record contract.CommandRecord) contract.CommandRecord {
	if record.Result.Order != nil {
		order := cloneOrder(*record.Result.Order)
		record.Result.Order = &order
	}
	return record
}
