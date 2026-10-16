package application_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
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

type unavailableProposalStore struct {
	application.ProposalStore
	application.ConfirmationAttemptStore
	err error
}

type completeOnceFailureStore struct {
	application.ProposalStore
	application.ConfirmationAttemptStore
	err    error
	mu     sync.Mutex
	failed bool
}

func (s unavailableProposalStore) Get(context.Context, string) (domain.ProposalRecord, bool, error) {
	return domain.ProposalRecord{}, false, s.err
}

func (s *completeOnceFailureStore) CompleteAttempt(ctx context.Context, commandID string, result contract.ConfirmResult, rejection *contract.AttemptError, replacement *domain.OrderProposal) error {
	s.mu.Lock()
	if !s.failed {
		s.failed = true
		s.mu.Unlock()
		return s.err
	}
	s.mu.Unlock()
	return s.ConfirmationAttemptStore.CompleteAttempt(ctx, commandID, result, rejection, replacement)
}

type racingCommandWarehouse struct {
	mu                  sync.Mutex
	initialLookups      int
	initialLookupsReady chan struct{}
	recorded            map[string]contract.CommandRecord
	ordersByProposal    map[string]contract.Order
	commitCalls         int
}

func newRacingCommandWarehouse() *racingCommandWarehouse {
	return &racingCommandWarehouse{
		initialLookupsReady: make(chan struct{}),
		recorded:            make(map[string]contract.CommandRecord),
		ordersByProposal:    make(map[string]contract.Order),
	}
}

func (*racingCommandWarehouse) EvaluateOrder(context.Context, contract.EvaluateOrderRequest) (contract.OrderEvaluation, error) {
	return contract.OrderEvaluation{}, nil
}

func (w *racingCommandWarehouse) CommitAcceptedOrder(_ context.Context, request contract.CommitOrderRequest) (contract.CommitResult, error) {
	order := contract.Order{
		OrderID:    "order-for-" + request.ProposalID,
		ProposalID: request.ProposalID,
		Positions:  append([]contract.AcceptedPosition(nil), request.AcceptedPositions...),
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.commitCalls++
	if previous, exists := w.recorded[request.CommandID]; exists {
		if previous.CommandFingerprint != confirmFingerprint(request.ProposalID) {
			return contract.CommitResult{}, contract.ErrCommandIdentityConflict
		}
		return contract.CommitResult{Kind: contract.OrderCommitted, Order: previous.Result.Order}, nil
	}
	w.recorded[request.CommandID] = commandRecord(request.CommandID, request.ProposalID, order)
	w.ordersByProposal[request.ProposalID] = order
	return contract.CommitResult{Kind: contract.OrderCommitted, Order: &order}, nil
}

func (w *racingCommandWarehouse) FindRecordedCommand(_ context.Context, commandID string) (contract.CommandRecord, bool, error) {
	w.mu.Lock()
	record, found := w.recorded[commandID]
	if commandID == "command-1" && w.initialLookups < 2 {
		w.initialLookups++
		if w.initialLookups == 2 {
			close(w.initialLookupsReady)
		}
		ready := w.initialLookupsReady
		w.mu.Unlock()
		<-ready
		return cloneCommandRecord(record), found, nil
	}
	w.mu.Unlock()
	return cloneCommandRecord(record), found, nil
}

func (w *racingCommandWarehouse) FindOrderByProposal(_ context.Context, proposalID string) (contract.Order, bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	order, found := w.ordersByProposal[proposalID]
	return cloneOrder(order), found, nil
}

func newWarehouseFake() *warehouseFake {
	return &warehouseFake{
		recorded:         make(map[string]contract.CommandRecord),
		ordersByProposal: make(map[string]contract.Order),
	}
}

func TestPrepareOrderRejectsPositionLimitBeforeWarehouse(t *testing.T) {
	warehouse := newWarehouseFake()
	service := newOrdersService(t, memory.NewProposalStore(), warehouse)
	positions := make([]contract.Position, 201)
	for index := range positions {
		positions[index] = contract.Position{DishID: "dish-1", Portions: 1}
	}
	if _, err := service.PrepareOrder(context.Background(), contract.PrepareOrderRequest{Positions: positions}); !errors.Is(err, contract.ErrInvalidArgument) {
		t.Fatalf("PrepareOrder() error = %v, want invalid argument", err)
	}
	if warehouse.evaluateCalls != 0 {
		t.Fatalf("warehouse evaluations = %d, want 0", warehouse.evaluateCalls)
	}
}

func TestPrepareOrderRejectsGeneratedProposalIDOverLimitBeforeWarehouse(t *testing.T) {
	warehouse := newWarehouseFake()
	service, err := application.NewOrdersService(memory.NewProposalStore(), warehouse, time.Now, func() string { return strings.Repeat("p", contract.MaxClientIDLength+1) })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.PrepareOrder(context.Background(), contract.PrepareOrderRequest{Positions: []contract.Position{{DishID: "dish-1", Portions: 1}}}); !errors.Is(err, contract.ErrInvalidArgument) {
		t.Fatalf("PrepareOrder() error = %v, want invalid argument", err)
	}
	if warehouse.evaluateCalls != 0 {
		t.Fatalf("warehouse evaluations = %d, want 0", warehouse.evaluateCalls)
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
	ids := testsupport.NewIDs("generated-proposal-")
	service, err := application.NewOrdersService(store, warehouse, func() time.Time { return time.Date(2026, time.October, 10, 12, 0, 0, 0, testsupport.Moscow) }, ids.Next)
	if err != nil {
		t.Fatal(err)
	}

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

func TestMismatchedCommittedPayloadStaysUnknownAndPassiveLookupRecovers(t *testing.T) {
	store := memory.NewProposalStore()
	warehouse := newWarehouseFake()
	createProposal(t, store, proposalFixture("proposal-1", 2))
	order := contract.Order{OrderID: "order-1", ProposalID: "proposal-1", Positions: []contract.AcceptedPosition{{LineNo: 3, DishID: "dish-1", Portions: 2}}}
	warehouse.commitHook = func(request contract.CommitOrderRequest) (contract.CommitResult, error) {
		malformed := order
		malformed.Positions = []contract.AcceptedPosition{{LineNo: 3, DishID: "dish-1", Portions: 1}}
		warehouse.mu.Lock()
		warehouse.recorded[request.CommandID] = commandRecord(request.CommandID, request.ProposalID, order)
		warehouse.mu.Unlock()
		return contract.CommitResult{Kind: contract.OrderCommitted, Order: &malformed}, nil
	}
	service := newOrdersService(t, store, warehouse)
	first, firstErr := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "proposal-1"})
	var operationError *contract.OperationError
	if !errors.As(firstErr, &operationError) || operationError.Outcome != "unknown" || first.Kind != "" {
		t.Fatalf("first ConfirmOrder() = (%+v,%v), want unknown malformed result", first, firstErr)
	}
	attempt, found, attemptErr := store.GetAttempt(context.Background(), "command-1")
	if attemptErr != nil || !found || attempt.State != contract.OrderAttemptPending || attempt.WarehouseResult != nil {
		t.Fatalf("attempt after malformed result = (%+v,%v,%v)", attempt, found, attemptErr)
	}
	second, secondErr := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "proposal-1"})
	if secondErr != nil || second.Kind != contract.OrderConfirmed || second.Order == nil || second.Order.OrderID != "order-1" {
		t.Fatalf("passive recorded lookup = (%+v,%v)", second, secondErr)
	}
	_, calls := warehouse.commits()
	if calls != 1 {
		t.Fatalf("commit calls = %d, want one", calls)
	}
}

func TestEmptyOrderBindsCommandAcrossProposals(t *testing.T) {
	store := memory.NewProposalStore()
	warehouse := newWarehouseFake()
	createProposal(t, store, proposalFixture("proposal-1", 0))
	createProposal(t, store, proposalFixture("proposal-2", 2))
	service := newOrdersService(t, store, warehouse)

	first, err := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "proposal-1"})
	if err != nil || first.Kind != contract.EmptyOrder {
		t.Fatalf("ConfirmOrder(P1) = (%+v, %v), want EmptyOrder", first, err)
	}
	second, err := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "proposal-2"})
	if !errors.Is(err, contract.ErrCommandIdentityConflict) || second.Kind != "" {
		t.Fatalf("ConfirmOrder(P2) = (%+v, %v), want global command identity conflict", second, err)
	}
	_, calls := warehouse.commits()
	if calls != 0 {
		t.Fatalf("empty order/reused ID reached warehouse %d times", calls)
	}
	original, found, err := store.GetAttempt(context.Background(), "command-1")
	if err != nil || !found || original.ProposalID != "proposal-1" || original.State != contract.OrderAttemptCompleted {
		t.Fatalf("original attempt = (%+v, %v, %v), want completed P1 binding", original, found, err)
	}
}

func TestConcurrentCrossProposalCommandBindingHasOneWinner(t *testing.T) {
	store := memory.NewProposalStore()
	warehouse := newWarehouseFake()
	createProposal(t, store, proposalFixture("proposal-1", 0))
	createProposal(t, store, proposalFixture("proposal-2", 0))
	service := newOrdersService(t, store, warehouse)
	type response struct {
		proposalID string
		result     contract.ConfirmResult
		err        error
	}
	responses := make(chan response, 2)
	for _, proposalID := range []string{"proposal-1", "proposal-2"} {
		proposalID := proposalID
		go func() {
			result, err := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: proposalID})
			responses <- response{proposalID: proposalID, result: result, err: err}
		}()
	}
	first, second := <-responses, <-responses
	results := map[string]response{first.proposalID: first, second.proposalID: second}
	winners := 0
	for _, value := range results {
		if value.result.Kind == contract.EmptyOrder && value.err == nil {
			winners++
		} else if !errors.Is(value.err, contract.ErrCommandIdentityConflict) {
			t.Fatalf("cross-proposal result = (%+v,%v), want EmptyOrder or identity conflict", value.result, value.err)
		}
	}
	if winners != 1 {
		t.Fatalf("completed winners = %d, want exactly one", winners)
	}
	_, commits := warehouse.commits()
	if commits != 0 {
		t.Fatalf("empty cross-proposal command made %d commits", commits)
	}
}

func TestRecordedWarehouseIdentityCannotBePoisonedByFreshServingBinding(t *testing.T) {
	store := memory.NewProposalStore()
	warehouse := newWarehouseFake()
	order := contract.Order{OrderID: "order-1", ProposalID: "proposal-true", Positions: []contract.AcceptedPosition{{LineNo: 1, DishID: "dish-1", Portions: 1}}}
	warehouse.recorded["command-1"] = commandRecord("command-1", "proposal-true", order)
	service := newOrdersService(t, store, warehouse)
	wrong, wrongErr := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "proposal-fake"})
	if !errors.Is(wrongErr, contract.ErrCommandIdentityConflict) || wrong.Kind != "" {
		t.Fatalf("wrong proposal ConfirmOrder() = (%+v,%v), want identity conflict", wrong, wrongErr)
	}
	correct, correctErr := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "proposal-true"})
	if correctErr != nil || correct.Kind != contract.OrderConfirmed || correct.Order == nil || correct.Order.OrderID != "order-1" {
		t.Fatalf("correct replay = (%+v,%v), want original recorded order", correct, correctErr)
	}
	_, commitCalls := warehouse.commits()
	if commitCalls != 0 {
		t.Fatalf("recorded replay called commit %d times", commitCalls)
	}
}

func TestForeignWarehouseIdentityRejectsExistingPendingAttemptAndReleasesProposal(t *testing.T) {
	store := memory.NewProposalStore()
	warehouse := newWarehouseFake()
	createProposal(t, store, proposalFixture("proposal-1", 1))
	foreignOrder := contract.Order{OrderID: "order-foreign", ProposalID: "proposal-2", Positions: []contract.AcceptedPosition{{LineNo: 3, DishID: "dish-1", Portions: 1}}}
	warehouse.recorded["command-1"] = commandRecord("command-1", "proposal-2", foreignOrder)
	if _, _, err := store.BeginAttempt(context.Background(), contract.ConfirmationAttempt{CommandID: "command-1", ProposalID: "proposal-1", Fingerprint: confirmFingerprint("proposal-1"), AcceptedPositions: []contract.AcceptedPosition{{LineNo: 3, DishID: "dish-1", Portions: 1}}, Pending: true}); err != nil {
		t.Fatal(err)
	}
	service := newOrdersService(t, store, warehouse)
	_, firstErr := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "proposal-1"})
	if !errors.Is(firstErr, contract.ErrCommandIdentityConflict) {
		t.Fatalf("first ConfirmOrder() error = %v, want terminal command identity conflict", firstErr)
	}
	attempt, found, attemptErr := store.GetAttempt(context.Background(), "command-1")
	if attemptErr != nil || !found || attempt.State != contract.OrderAttemptRejected || attempt.Error == nil || attempt.Error.Code != "CommandConflict" || attempt.Error.Outcome != "not_committed" {
		t.Fatalf("attempt = (%+v,%v,%v), want cached rejection", attempt, found, attemptErr)
	}
	proposal, proposalFound, proposalErr := store.Get(context.Background(), "proposal-1")
	if proposalErr != nil || !proposalFound || proposal.PendingCommandID != "" || proposal.SupersededBy != "" {
		t.Fatalf("proposal after rejection = (%+v,%v,%v), want active without pending", proposal, proposalFound, proposalErr)
	}
	_, retryErr := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "proposal-1"})
	if !errors.Is(retryErr, contract.ErrCommandIdentityConflict) {
		t.Fatalf("exact retry error = %v", retryErr)
	}
	warehouse.mu.Lock()
	warehouse.recorded = make(map[string]contract.CommandRecord)
	warehouse.commitResult = contract.CommitResult{Kind: contract.OrderCommitted, Order: &contract.Order{OrderID: "order-1", ProposalID: "proposal-1", Positions: []contract.AcceptedPosition{{LineNo: 3, DishID: "dish-1", Portions: 1}}}}
	warehouse.mu.Unlock()
	second, secondErr := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-2", ProposalID: "proposal-1"})
	if secondErr != nil || second.Kind != contract.OrderConfirmed || second.Order == nil || second.Order.OrderID != "order-1" {
		t.Fatalf("new C2 on active proposal = (%+v,%v)", second, secondErr)
	}
	_, commits := warehouse.commits()
	if commits != 1 {
		t.Fatalf("warehouse commits = %d, want only C2", commits)
	}
}

// Attempts stored before CommandConflict was adopted carry the old code. It is
// only read: the caller gets the public code, and the cause stays the identity
// conflict.
func TestStoredAttemptWithLegacyCommandIdentityConflictCodeIsServedAsCommandConflict(t *testing.T) {
	store := memory.NewProposalStore()
	warehouse := newWarehouseFake()
	createProposal(t, store, proposalFixture("proposal-1", 1))
	if _, _, err := store.BeginAttempt(context.Background(), contract.ConfirmationAttempt{CommandID: "command-1", ProposalID: "proposal-1", Fingerprint: confirmFingerprint("proposal-1")}); err != nil {
		t.Fatal(err)
	}
	legacy := &contract.AttemptError{Code: "CommandIdentityConflict", Message: "command identity conflicts with an existing attempt", Outcome: "not_committed"}
	if err := store.CompleteAttempt(context.Background(), "command-1", contract.ConfirmResult{}, legacy, nil); err != nil {
		t.Fatal(err)
	}
	service := newOrdersService(t, store, warehouse)
	_, err := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "proposal-1"})
	var operation *contract.OperationError
	if !errors.Is(err, contract.ErrCommandIdentityConflict) || !errors.As(err, &operation) || operation.Code != "CommandConflict" || operation.Outcome != "not_committed" {
		t.Fatalf("ConfirmOrder() error = %#v, want CommandConflict/not_committed with identity conflict cause", err)
	}
	stored, found, err := store.GetAttempt(context.Background(), "command-1")
	if err != nil || !found || stored.Error == nil || stored.Error.Code != "CommandIdentityConflict" {
		t.Fatalf("stored attempt was rewritten: (%+v,%v,%v)", stored, found, err)
	}
}

func TestRejectedAttemptFromLegacyWarehouseConflictIsStoredAsCommandConflict(t *testing.T) {
	store := memory.NewProposalStore()
	warehouse := newWarehouseFake()
	createProposal(t, store, proposalFixture("proposal-1", 1))
	warehouse.commitErr = &contract.OperationError{Code: "CommandIdentityConflict", Message: "conflict", Outcome: "not_committed", Cause: contract.ErrCommandIdentityConflict}
	service := newOrdersService(t, store, warehouse)
	_, err := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "proposal-1"})
	var operation *contract.OperationError
	if !errors.Is(err, contract.ErrCommandIdentityConflict) || !errors.As(err, &operation) || operation.Code != "CommandConflict" {
		t.Fatalf("ConfirmOrder() error = %#v, want CommandConflict", err)
	}
	stored, found, getErr := store.GetAttempt(context.Background(), "command-1")
	if getErr != nil || !found || stored.State != contract.OrderAttemptRejected || stored.Error == nil || stored.Error.Code != "CommandConflict" {
		t.Fatalf("stored attempt = (%+v,%v,%v), want Rejected/CommandConflict", stored, found, getErr)
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

func TestRecordedCommandRetryResolvesPending(t *testing.T) {
	store := memory.NewProposalStore()
	createProposal(t, store, proposalFixture("proposal-1", 2))
	reservePending(t, store, "proposal-1", "command-1")
	warehouse := newWarehouseFake()
	order := contract.Order{OrderID: "order-1", ProposalID: "proposal-1", Positions: []contract.AcceptedPosition{{LineNo: 3, DishID: "dish-1", Portions: 2}}}
	warehouse.recorded["command-1"] = commandRecord("command-1", "proposal-1", order)
	service := newOrdersService(t, store, warehouse)

	result, err := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "proposal-1"})
	if err != nil || result.Kind != contract.OrderConfirmed || result.Order == nil || result.Order.OrderID != "order-1" {
		t.Fatalf("ConfirmOrder() = (%+v, %v), want recorded order", result, err)
	}
	record, ok, getErr := store.Get(context.Background(), "proposal-1")
	if getErr != nil || !ok || record.PendingCommandID != "" {
		t.Fatalf("recorded command retry left pending state: (%+v, %v, %v)", record, ok, getErr)
	}
	_, calls := warehouse.commits()
	if calls != 0 {
		t.Fatalf("recorded command retry called CommitAcceptedOrder %d times", calls)
	}
}

func TestProposalLookupWithDifferentCommandResolvesOriginalPending(t *testing.T) {
	store := memory.NewProposalStore()
	createProposal(t, store, proposalFixture("proposal-1", 2))
	reservePending(t, store, "proposal-1", "command-1")
	warehouse := newWarehouseFake()
	warehouse.ordersByProposal["proposal-1"] = contract.Order{
		OrderID:    "order-1",
		ProposalID: "proposal-1",
		Positions:  []contract.AcceptedPosition{{LineNo: 3, DishID: "dish-1", Portions: 2}},
	}
	service := newOrdersService(t, store, warehouse)

	result, err := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-2", ProposalID: "proposal-1"})
	if err != nil || result.Kind != contract.OrderConfirmed || result.Order == nil || result.Order.OrderID != "order-1" {
		t.Fatalf("ConfirmOrder() = (%+v, %v), want proposal's recorded order", result, err)
	}
	record, ok, getErr := store.Get(context.Background(), "proposal-1")
	if getErr != nil || !ok || record.PendingCommandID != "command-1" {
		t.Fatalf("C2 read changed the original attempt marker: (%+v, %v, %v)", record, ok, getErr)
	}
	original, retryErr := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "proposal-1"})
	if retryErr != nil || original.Kind != contract.OrderConfirmed || original.Order == nil || original.Order.OrderID != "order-1" {
		t.Fatalf("original C1 retry = (%+v, %v)", original, retryErr)
	}
	record, ok, getErr = store.Get(context.Background(), "proposal-1")
	if getErr != nil || !ok || record.PendingCommandID != "" {
		t.Fatalf("C1 atomic completion left marker: (%+v, %v, %v)", record, ok, getErr)
	}
	_, calls := warehouse.commits()
	if calls != 0 {
		t.Fatalf("proposal recovery called CommitAcceptedOrder %d times", calls)
	}
}

func TestRecordedOrderReturnsWhenProposalStoreIsMissing(t *testing.T) {
	warehouse := newWarehouseFake()
	order := contract.Order{OrderID: "order-1", ProposalID: "proposal-1", Positions: []contract.AcceptedPosition{{LineNo: 3, DishID: "dish-1", Portions: 2}}}
	warehouse.recorded["command-1"] = commandRecord("command-1", "proposal-1", order)
	service := newOrdersService(t, memory.NewProposalStore(), warehouse)

	result, err := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "proposal-1"})
	if err != nil || result.Kind != contract.OrderConfirmed || result.Order == nil || result.Order.OrderID != "order-1" {
		t.Fatalf("ConfirmOrder() = (%+v, %v), want recorded order without stored proposal", result, err)
	}
}

func TestRecordedOrderReturnsWhenProposalStoreIsUnavailable(t *testing.T) {
	warehouse := newWarehouseFake()
	order := contract.Order{OrderID: "order-1", ProposalID: "proposal-1", Positions: []contract.AcceptedPosition{{LineNo: 3, DishID: "dish-1", Portions: 2}}}
	warehouse.recorded["command-1"] = commandRecord("command-1", "proposal-1", order)
	storeErr := errors.New("temporary proposal store outage")
	baseStore := memory.NewProposalStore()
	store := unavailableProposalStore{ProposalStore: baseStore, ConfirmationAttemptStore: baseStore, err: storeErr}
	service := newOrdersService(t, store, warehouse)

	result, err := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "proposal-1"})
	if err != nil || result.Kind != contract.OrderConfirmed || result.Order == nil || result.Order.OrderID != "order-1" {
		t.Fatalf("ConfirmOrder() = (%+v, %v), want recorded order despite store error %v", result, err, storeErr)
	}
	_, calls := warehouse.commits()
	if calls != 0 {
		t.Fatalf("recorded order recovery committed %d times", calls)
	}
}

func TestRecordedOrderAtomicallyCompletesMatchingPendingMarker(t *testing.T) {
	baseStore := memory.NewProposalStore()
	createProposal(t, baseStore, proposalFixture("proposal-1", 2))
	reservePending(t, baseStore, "proposal-1", "command-1")
	store := baseStore
	warehouse := newWarehouseFake()
	order := contract.Order{OrderID: "order-1", ProposalID: "proposal-1", Positions: []contract.AcceptedPosition{{LineNo: 3, DishID: "dish-1", Portions: 2}}}
	warehouse.recorded["command-1"] = commandRecord("command-1", "proposal-1", order)
	service := newOrdersService(t, store, warehouse)

	result, err := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "proposal-1"})
	if err != nil || result.Kind != contract.OrderConfirmed || result.Order == nil || result.Order.OrderID != "order-1" {
		t.Fatalf("ConfirmOrder() = (%+v, %v), want recorded order", result, err)
	}
	record, ok, getErr := baseStore.Get(context.Background(), "proposal-1")
	if getErr != nil || !ok || record.PendingCommandID != "" {
		t.Fatalf("pending state after atomic completion = (%+v, %v, %v), want empty", record, ok, getErr)
	}
}

func TestCommittedOrderRemainsAuthoritativeWhenLocalCompletionFails(t *testing.T) {
	baseStore := memory.NewProposalStore()
	createProposal(t, baseStore, proposalFixture("proposal-1", 2))
	storeErr := errors.New("temporary local completion failure")
	store := &completeOnceFailureStore{ProposalStore: baseStore, ConfirmationAttemptStore: baseStore, err: storeErr}
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
	request := contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "proposal-1"}

	first, err := service.ConfirmOrder(context.Background(), request)
	var operationError *contract.OperationError
	if !errors.As(err, &operationError) || operationError.Outcome != "unknown" || first.Kind != "" {
		t.Fatalf("first ConfirmOrder() = (%+v, %v), want unknown after local completion error %v", first, err, storeErr)
	}
	attempt, found, attemptErr := baseStore.GetAttempt(context.Background(), "command-1")
	if attemptErr != nil || !found || attempt.State != contract.OrderAttemptPending || attempt.WarehouseResult == nil || attempt.WarehouseResult.Kind != contract.OrderCommitted {
		t.Fatalf("attempt after local completion failure = (%+v,%v,%v)", attempt, found, attemptErr)
	}
	record, ok, getErr := baseStore.Get(context.Background(), "proposal-1")
	if getErr != nil || !ok || record.PendingCommandID != "command-1" {
		t.Fatalf("pending marker after failed completion = (%+v, %v, %v), want command-1", record, ok, getErr)
	}

	second, err := service.ConfirmOrder(context.Background(), request)
	if err != nil || second.Kind != contract.OrderConfirmed || second.Order == nil || second.Order.OrderID != order.OrderID {
		t.Fatalf("repeated ConfirmOrder() = (%+v, %v), want same committed order", second, err)
	}
	record, ok, getErr = baseStore.Get(context.Background(), "proposal-1")
	if getErr != nil || !ok || record.PendingCommandID != "" {
		t.Fatalf("pending marker after successful local completion = (%+v, %v, %v), want empty", record, ok, getErr)
	}
	_, calls := warehouse.commits()
	if calls != 1 {
		t.Fatalf("repeated ConfirmOrder() committed %d times, want one", calls)
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

func TestContextCancellationAfterKnownCommitStillCompletesLocally(t *testing.T) {
	store := memory.NewProposalStore()
	warehouse := newWarehouseFake()
	createProposal(t, store, proposalFixture("proposal-1", 2))
	order := contract.Order{OrderID: "order-1", ProposalID: "proposal-1", Positions: []contract.AcceptedPosition{{LineNo: 3, DishID: "dish-1", Portions: 2}}}
	ctx, cancel := context.WithCancel(context.Background())
	warehouse.commitHook = func(request contract.CommitOrderRequest) (contract.CommitResult, error) {
		warehouse.mu.Lock()
		warehouse.recorded[request.CommandID] = commandRecord(request.CommandID, request.ProposalID, order)
		warehouse.mu.Unlock()
		cancel()
		return contract.CommitResult{Kind: contract.OrderCommitted, Order: &order}, nil
	}
	service := newOrdersService(t, store, warehouse)
	result, err := service.ConfirmOrder(ctx, contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "proposal-1"})
	if err != nil || result.Kind != contract.OrderConfirmed || result.Order == nil {
		t.Fatalf("ConfirmOrder() = (%+v,%v), want locally completed known order", result, err)
	}
	attempt, found, attemptErr := store.GetAttempt(context.Background(), "command-1")
	if attemptErr != nil || !found || attempt.State != contract.OrderAttemptCompleted {
		t.Fatalf("attempt after cancellation = (%+v,%v,%v)", attempt, found, attemptErr)
	}
	_, calls := warehouse.commits()
	if calls != 1 {
		t.Fatalf("commit calls = %d, want one", calls)
	}
}

func TestReplacementIDFailurePreservesKnownWarehouseResultForRetry(t *testing.T) {
	store := memory.NewProposalStore()
	warehouse := newWarehouseFake()
	createProposal(t, store, proposalFixture("proposal-1", 2))
	warehouse.commitResult = contract.CommitResult{Kind: contract.RecheckRequired, OrderEvaluation: &contract.OrderEvaluation{EvaluatedAt: time.Date(2026, time.October, 11, 12, 0, 0, 0, testsupport.Moscow), StateVersion: 8, AcceptedPositions: []contract.AcceptedPosition{{LineNo: 3, DishID: "dish-1", Portions: 1}}, Changes: []contract.Change{}}}
	idCalls := 0
	service, err := application.NewOrdersService(store, warehouse, func() time.Time { return time.Date(2026, time.October, 11, 12, 0, 0, 0, testsupport.Moscow) }, func() string {
		idCalls++
		if idCalls == 1 {
			return ""
		}
		return "replacement-1"
	})
	if err != nil {
		t.Fatal(err)
	}
	first, firstErr := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "proposal-1"})
	var operationError *contract.OperationError
	if !errors.As(firstErr, &operationError) || operationError.Outcome != "unknown" || first.Kind != "" {
		t.Fatalf("first ConfirmOrder() = (%+v,%v), want unknown retained result", first, firstErr)
	}
	attempt, found, attemptErr := store.GetAttempt(context.Background(), "command-1")
	if attemptErr != nil || !found || attempt.WarehouseResult == nil || attempt.WarehouseResult.Kind != contract.RecheckRequired {
		t.Fatalf("attempt = (%+v,%v,%v), want saved recheck", attempt, found, attemptErr)
	}
	second, secondErr := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "proposal-1"})
	if secondErr != nil || second.Kind != contract.RequiresConfirmation || second.Proposal == nil || second.Proposal.ProposalID != "replacement-1" {
		t.Fatalf("retry = (%+v,%v)", second, secondErr)
	}
	_, calls := warehouse.commits()
	if calls != 1 {
		t.Fatalf("warehouse commit calls = %d, want one", calls)
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

func TestConflictingRecordedCommandDoesNotClearDifferentPendingMarker(t *testing.T) {
	store := memory.NewProposalStore()
	createProposal(t, store, proposalFixture("proposal-1", 2))
	reservePending(t, store, "proposal-1", "command-2")
	warehouse := newWarehouseFake()
	otherOrder := contract.Order{OrderID: "other-order", ProposalID: "other-proposal"}
	warehouse.recorded["command-1"] = commandRecord("command-1", "other-proposal", otherOrder)
	service := newOrdersService(t, store, warehouse)

	_, err := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "proposal-1"})
	if !errors.Is(err, contract.ErrCommandIdentityConflict) {
		t.Fatalf("conflicting command C1 error = %v; want ErrCommandIdentityConflict", err)
	}
	record, ok, getErr := store.Get(context.Background(), "proposal-1")
	if getErr != nil || !ok || record.PendingCommandID != "command-2" {
		t.Fatalf("pending marker after unrelated C1 conflict = (%+v, %v, %v); want original unknown C2", record, ok, getErr)
	}

	result, err := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-3", ProposalID: "proposal-1"})
	if err != nil || result.Kind != contract.ConfirmationPending {
		t.Fatalf("new command C3 while C2 is pending = (%+v, %v); want ConfirmationPending", result, err)
	}
	if _, calls := warehouse.commits(); calls != 0 {
		t.Fatalf("unrelated C1 conflict or blocked C3 made %d warehouse commits; want none", calls)
	}

	order := contract.Order{OrderID: "order-1", ProposalID: "proposal-1", Positions: []contract.AcceptedPosition{{LineNo: 3, DishID: "dish-1", Portions: 2}}}
	warehouse.recorded["command-2"] = commandRecord("command-2", "proposal-1", order)
	result, err = service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-2", ProposalID: "proposal-1"})
	if err != nil || result.Kind != contract.OrderConfirmed || result.Order == nil || result.Order.OrderID != order.OrderID {
		t.Fatalf("retry of original pending C2 = (%+v, %v); want its recorded order", result, err)
	}
	record, ok, getErr = store.Get(context.Background(), "proposal-1")
	if getErr != nil || !ok || record.PendingCommandID != "" {
		t.Fatalf("pending marker after recorded C2 retry = (%+v, %v, %v); want empty", record, ok, getErr)
	}
	if _, calls := warehouse.commits(); calls != 0 {
		t.Fatalf("retry of recorded C2 called warehouse CommitAcceptedOrder %d times; want no new commit", calls)
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

func TestKnownInvalidArgumentClearsPendingAndAllowsNewCommand(t *testing.T) {
	store := memory.NewProposalStore()
	warehouse := newWarehouseFake()
	warehouse.commitErr = contract.ErrInvalidArgument
	service := newOrdersService(t, store, warehouse)
	createProposal(t, store, proposalFixture("proposal-1", 1))

	_, err := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "proposal-1"})
	if !errors.Is(err, contract.ErrInvalidArgument) {
		t.Fatalf("first ConfirmOrder() error = %v, want ErrInvalidArgument", err)
	}
	record, ok, getErr := store.Get(context.Background(), "proposal-1")
	if getErr != nil || !ok || record.PendingCommandID != "" {
		t.Fatalf("known invalid argument left pending state: (%+v, %v, %v)", record, ok, getErr)
	}

	warehouse.mu.Lock()
	warehouse.commitErr = nil
	warehouse.commitResult = contract.CommitResult{Kind: contract.OrderCommitted, Order: &contract.Order{OrderID: "order-1", ProposalID: "proposal-1", Positions: []contract.AcceptedPosition{{LineNo: 3, DishID: "dish-1", Portions: 1}}}}
	warehouse.mu.Unlock()
	result, err := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-2", ProposalID: "proposal-1"})
	if err != nil || result.Kind != contract.OrderConfirmed || result.Order == nil || result.Order.OrderID != "order-1" {
		t.Fatalf("retry with valid command = (%+v, %v), want committed order", result, err)
	}
	requests, calls := warehouse.commits()
	if calls != 2 || requests[0].CommandID != "command-1" || requests[1].CommandID != "command-2" {
		t.Fatalf("commit requests = %+v, want rejected C1 then accepted C2", requests)
	}
}

func TestConcurrentCrossProposalReuseReservesOneGlobalCommand(t *testing.T) {
	baseStore := memory.NewProposalStore()
	createProposal(t, baseStore, proposalFixture("proposal-1", 1))
	createProposal(t, baseStore, proposalFixture("proposal-2", 1))
	store := baseStore
	warehouse := newRacingCommandWarehouse()
	ids := testsupport.NewIDs("generated-proposal-")
	service, err := application.NewOrdersService(store, warehouse, func() time.Time { return time.Date(2026, time.October, 10, 12, 0, 0, 0, testsupport.Moscow) }, ids.Next)
	if err != nil {
		t.Fatal(err)
	}

	type response struct {
		proposalID string
		result     contract.ConfirmResult
		err        error
	}
	responses := make(chan response, 2)
	for _, proposalID := range []string{"proposal-1", "proposal-2"} {
		proposalID := proposalID
		go func() {
			result, confirmErr := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: proposalID})
			responses <- response{proposalID: proposalID, result: result, err: confirmErr}
		}()
	}
	first := <-responses
	second := <-responses
	results := map[string]response{first.proposalID: first, second.proposalID: second}
	winner := ""
	for proposalID, value := range results {
		if value.err == nil && value.result.Kind == contract.OrderConfirmed {
			winner = proposalID
			continue
		}
		if !errors.Is(value.err, contract.ErrCommandIdentityConflict) {
			t.Fatalf("%s C1 result = (%+v,%v), want committed order or identity conflict", proposalID, value.result, value.err)
		}
	}
	if winner == "" {
		t.Fatal("neither proposal completed command-1")
	}
	loser := "proposal-1"
	if winner == loser {
		loser = "proposal-2"
	}
	attempt, found, attemptErr := baseStore.GetAttempt(context.Background(), "command-1")
	if attemptErr != nil || !found || attempt.ProposalID != winner || attempt.State != contract.OrderAttemptCompleted {
		t.Fatalf("global C1 binding = (%+v,%v,%v), winner=%s", attempt, found, attemptErr, winner)
	}
	record, ok, getErr := baseStore.Get(context.Background(), loser)
	if getErr != nil || !ok || record.PendingCommandID != "" {
		t.Fatalf("losing proposal pending state = (%+v,%v,%v)", record, ok, getErr)
	}
	if _, err := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: loser}); !errors.Is(err, contract.ErrCommandIdentityConflict) {
		t.Fatalf("losing C1 retry error = %v", err)
	}
	result, err := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-2", ProposalID: loser})
	if err != nil || result.Kind != contract.OrderConfirmed || result.Order == nil || result.Order.ProposalID != loser {
		t.Fatalf("losing proposal C2 = (%+v,%v)", result, err)
	}
	warehouse.mu.Lock()
	commitCalls := warehouse.commitCalls
	warehouse.mu.Unlock()
	if commitCalls != 2 {
		t.Fatalf("warehouse commit calls = %d, want one winner and one new loser command", commitCalls)
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
	if getErr != nil || !ok || old.SupersededBy != "" || old.PendingCommandID != "command-1" {
		t.Fatalf("old proposal after failed replacement = (%+v, %v, %v)", old, ok, getErr)
	}
	attempt, found, attemptErr := store.GetAttempt(context.Background(), "command-1")
	if attemptErr != nil || !found || attempt.State != contract.OrderAttemptPending || attempt.WarehouseResult == nil || attempt.WarehouseResult.Kind != contract.RecheckRequired {
		t.Fatalf("attempt after failed local completion = (%+v, %v, %v), want pending with known RecheckRequired", attempt, found, attemptErr)
	}
	retry, retryErr := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "proposal-1"})
	if retryErr != nil || retry.Kind != contract.RequiresConfirmation || retry.Proposal == nil || retry.Proposal.ProposalID == "generated-proposal-1" {
		t.Fatalf("retry after known result = (%+v, %v), want saved result with a non-colliding replacement", retry, retryErr)
	}
	_, commitCalls := warehouse.commits()
	if commitCalls != 1 {
		t.Fatalf("local completion retry called warehouse %d times, want one", commitCalls)
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
	reservePending(t, store, "proposal-1", "command-1")
	replacement := proposalFixture("proposal-2", 1)
	if err := store.CompleteAttempt(context.Background(), "command-1", contract.ConfirmResult{Kind: contract.ProposalSuperseded, SupersededBy: "proposal-2"}, nil, &replacement); err != nil {
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
	type confirmResponse struct {
		result contract.ConfirmResult
		err    error
	}
	secondDone := make(chan confirmResponse, 1)
	go func() {
		result, err := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-2", ProposalID: "proposal-1"})
		secondDone <- confirmResponse{result: result, err: err}
	}()
	close(release)
	if err := <-firstDone; err == nil {
		t.Fatal("first ConfirmOrder() unexpectedly succeeded")
	}
	if response := <-secondDone; response.err != nil || response.result.Kind != contract.OrderConfirmed {
		t.Fatalf("concurrent second result = (%+v, %v), want recorded order", response.result, response.err)
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

// reservePending records a confirmation attempt for the command and makes it
// the proposal's pending owner, the way ConfirmOrder does before a commit.
func reservePending(t *testing.T, store *memory.ProposalStore, proposalID, commandID string) {
	t.Helper()
	record, ok, err := store.Get(context.Background(), proposalID)
	if err != nil || !ok {
		t.Fatalf("Get(%q) = (%v, %v), want record", proposalID, ok, err)
	}
	attempt := contract.ConfirmationAttempt{
		CommandID:         commandID,
		ProposalID:        proposalID,
		Fingerprint:       confirmFingerprint(proposalID),
		AcceptedPositions: toAcceptedPositions(record.Proposal.AcceptedPositions),
	}
	if _, _, err := store.BeginAttempt(context.Background(), attempt); err != nil {
		t.Fatal(err)
	}
	if err := store.ReserveAttempt(context.Background(), commandID); err != nil {
		t.Fatal(err)
	}
}

func toAcceptedPositions(positions []domain.AcceptedPosition) []contract.AcceptedPosition {
	accepted := make([]contract.AcceptedPosition, len(positions))
	for i, position := range positions {
		accepted[i] = contract.AcceptedPosition(position)
	}
	return accepted
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
	input, err := json.Marshal(struct {
		ProposalID string `json:"proposalID"`
	}{ProposalID: proposalID})
	if err != nil {
		panic(err)
	}
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
