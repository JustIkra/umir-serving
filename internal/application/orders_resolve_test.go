package application_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/JustIkra/umir-serving/contract"
	"github.com/JustIkra/umir-serving/internal/adapters/memory"
	"github.com/JustIkra/umir-serving/internal/application"
	"github.com/JustIkra/umir-serving/tests/testsupport"
)

type attemptReaderFake struct {
	result contract.OrderAttempt
	err    error
	calls  int
}

func (r *attemptReaderFake) FindOrderAttempt(context.Context, string, string) (contract.OrderAttempt, error) {
	r.calls++
	return r.result, r.err
}

func TestResolveUnseenLocallyPendingAttemptReturnsConfirmationPending(t *testing.T) {
	store := memory.NewProposalStore()
	warehouse := newWarehouseFake()
	reader := &attemptReaderFake{result: contract.OrderAttempt{State: contract.OrderAttemptUnseen}}
	service := newOrdersServiceWithReader(t, store, warehouse, reader)
	createProposal(t, store, proposalFixture("proposal-1", 2))
	_, confirmErr := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "proposal-1"})
	var confirmOutcome *contract.OperationError
	if !errors.As(confirmErr, &confirmOutcome) || confirmOutcome.Outcome != "unknown" {
		t.Fatalf("initial ConfirmOrder() error = %v, want unknown", confirmErr)
	}

	result, err := service.ResolveConfirmation(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "proposal-1"})
	if err != nil || result.Kind != contract.ConfirmationPending {
		t.Fatalf("ResolveConfirmation() = (%+v, %v), want ConfirmationPending", result, err)
	}
	_, commitCalls := warehouse.commits()
	if commitCalls != 1 || reader.calls != 2 {
		t.Fatalf("commit calls = %d, lookup calls = %d, want one commit and the confirm/resolve passive lookups", commitCalls, reader.calls)
	}
}

func TestResolveWithoutLocalAttemptReturnsProposalNotFoundUnknown(t *testing.T) {
	reader := &attemptReaderFake{result: contract.OrderAttempt{State: contract.OrderAttemptUnseen}}
	service := newOrdersServiceWithReader(t, memory.NewProposalStore(), newWarehouseFake(), reader)
	result, err := service.ResolveConfirmation(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "proposal-1"})
	var operationError *contract.OperationError
	if !errors.As(err, &operationError) || operationError.Code != "ProposalNotFound" || operationError.Outcome != "unknown" || result.Kind != "" || reader.calls != 0 {
		t.Fatalf("ResolveConfirmation() = (%+v,%v), warehouse lookup calls=%d", result, err, reader.calls)
	}
}

func TestResolveLocallyPendingUnseenAttemptReturnsConfirmationPending(t *testing.T) {
	store := memory.NewProposalStore()
	warehouse := newWarehouseFake()
	reader := &attemptReaderFake{result: contract.OrderAttempt{State: contract.OrderAttemptUnseen}}
	service := newOrdersServiceWithReader(t, store, warehouse, reader)
	createProposal(t, store, proposalFixture("proposal-1", 2))
	if _, _, err := store.BeginAttempt(context.Background(), contract.ConfirmationAttempt{CommandID: "command-1", ProposalID: "proposal-1", Fingerprint: confirmFingerprint("proposal-1"), AcceptedPositions: []contract.AcceptedPosition{{LineNo: 3, DishID: "dish-1", Portions: 2}}, Pending: true}); err != nil {
		t.Fatal(err)
	}
	result, err := service.ResolveConfirmation(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "proposal-1"})
	if err != nil || result.Kind != contract.ConfirmationPending {
		t.Fatalf("ResolveConfirmation() = (%+v,%v), want ConfirmationPending", result, err)
	}
	_, commits := warehouse.commits()
	if commits != 0 {
		t.Fatalf("ResolveConfirmation() committed %d times", commits)
	}
}

func TestResolvePendingDoesNotCommit(t *testing.T) {
	store := memory.NewProposalStore()
	warehouse := newWarehouseFake()
	reader := &attemptReaderFake{result: contract.OrderAttempt{State: contract.OrderAttemptPending}}
	service := newOrdersServiceWithReader(t, store, warehouse, reader)
	proposal := proposalFixture("proposal-1", 2)
	createProposal(t, store, proposal)
	if _, _, err := store.BeginAttempt(context.Background(), contract.ConfirmationAttempt{CommandID: "command-1", ProposalID: "proposal-1", Fingerprint: confirmFingerprint("proposal-1"), AcceptedPositions: []contract.AcceptedPosition{{LineNo: 3, DishID: "dish-1", Portions: 2}}, Pending: true}); err != nil {
		t.Fatal(err)
	}

	result, err := service.ResolveConfirmation(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "proposal-1"})
	if err != nil || result.Kind != contract.ConfirmationPending {
		t.Fatalf("ResolveConfirmation() = (%+v, %v), want ConfirmationPending", result, err)
	}
	_, commitCalls := warehouse.commits()
	if commitCalls != 0 {
		t.Fatalf("resolve called CommitAcceptedOrder %d times", commitCalls)
	}
}

func TestResolveCompletedAttemptSavesOneReplacementAndReplaysIt(t *testing.T) {
	store := memory.NewProposalStore()
	warehouse := newWarehouseFake()
	reader := &attemptReaderFake{result: contract.OrderAttempt{State: contract.OrderAttemptCompleted, Result: &contract.CommitResult{Kind: contract.RecheckRequired, OrderEvaluation: &contract.OrderEvaluation{EvaluatedAt: time.Date(2026, time.October, 11, 12, 0, 0, 0, testsupport.Moscow), StateVersion: 8, RequestedPositions: []contract.Position{{LineNo: 3, DishID: "dish-1", Portions: 2}}, AcceptedPositions: []contract.AcceptedPosition{{LineNo: 3, DishID: "dish-1", Portions: 1}}, Changes: []contract.Change{}}}}}
	service := newOrdersServiceWithReader(t, store, warehouse, reader)
	createProposal(t, store, proposalFixture("proposal-1", 2))
	if _, _, err := store.BeginAttempt(context.Background(), contract.ConfirmationAttempt{CommandID: "command-1", ProposalID: "proposal-1", Fingerprint: confirmFingerprint("proposal-1"), AcceptedPositions: []contract.AcceptedPosition{{LineNo: 3, DishID: "dish-1", Portions: 2}}, Pending: true}); err != nil {
		t.Fatal(err)
	}

	first, err := service.ResolveConfirmation(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "proposal-1"})
	if err != nil || first.Kind != contract.RequiresConfirmation || first.Proposal == nil {
		var operation *contract.OperationError
		errors.As(err, &operation)
		t.Fatalf("first ResolveConfirmation() = (%+v, %v), cause %v", first, err, operation.Cause)
	}
	reader.result = contract.OrderAttempt{State: contract.OrderAttemptPending}
	second, err := service.ResolveConfirmation(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "proposal-1"})
	if err != nil || second.Kind != first.Kind || second.Proposal == nil || second.Proposal.ProposalID != first.Proposal.ProposalID {
		t.Fatalf("retry ResolveConfirmation() = (%+v, %v), want same saved replacement", second, err)
	}
	_, commitCalls := warehouse.commits()
	if commitCalls != 0 {
		t.Fatalf("resolve called CommitAcceptedOrder %d times", commitCalls)
	}
}

func newOrdersServiceWithReader(t *testing.T, store application.ProposalStore, warehouse *warehouseFake, reader contract.OrderAttemptReader) *application.OrdersService {
	t.Helper()
	ids := testsupport.NewIDs("generated-proposal-")
	service, err := application.NewOrdersServiceWithOptions(store, warehouse, func() time.Time { return time.Date(2026, time.October, 10, 12, 0, 0, 0, testsupport.Moscow) }, ids.Next, application.OrdersServiceOptions{LeaseClock: time.Now, Wait: testWait}, reader)
	if err != nil {
		t.Fatalf("NewOrdersService() error = %v", err)
	}
	return service
}

// A non-scoped Warehouse may still answer a rejected attempt with the old
// CommandIdentityConflict code; the stored attempt must carry the public code.
func TestRejectedWarehouseAttemptWithLegacyConflictCodeIsStoredAsCommandConflict(t *testing.T) {
	legacy := contract.OrderAttempt{State: contract.OrderAttemptRejected, Error: &contract.AttemptError{Code: "CommandIdentityConflict", Message: "conflict", Outcome: "not_committed"}}
	for name, run := range map[string]func(*application.OrdersService) error{
		"ResolveConfirmation": func(service *application.OrdersService) error {
			_, err := service.ResolveConfirmation(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "proposal-1"})
			return err
		},
		"ConfirmOrder": func(service *application.OrdersService) error {
			_, err := service.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "proposal-1"})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			store := memory.NewProposalStore()
			warehouse := newWarehouseFake()
			reader := &attemptReaderFake{result: legacy}
			service := newOrdersServiceWithReader(t, store, warehouse, reader)
			createProposal(t, store, proposalFixture("proposal-1", 2))
			if _, _, err := store.BeginAttempt(context.Background(), contract.ConfirmationAttempt{CommandID: "command-1", ProposalID: "proposal-1", Fingerprint: confirmFingerprint("proposal-1"), AcceptedPositions: []contract.AcceptedPosition{{LineNo: 3, DishID: "dish-1", Portions: 2}}, Pending: true}); err != nil {
				t.Fatal(err)
			}
			err := run(service)
			var operation *contract.OperationError
			if !errors.Is(err, contract.ErrCommandIdentityConflict) || !errors.As(err, &operation) || operation.Code != "CommandConflict" {
				t.Fatalf("error = %#v, want CommandConflict", err)
			}
			stored, found, getErr := store.GetAttempt(context.Background(), "command-1")
			if getErr != nil || !found || stored.State != contract.OrderAttemptRejected || stored.Error == nil || stored.Error.Code != "CommandConflict" {
				t.Fatalf("stored attempt = (%+v,%v,%v), want Rejected/CommandConflict", stored, found, getErr)
			}
		})
	}
}
