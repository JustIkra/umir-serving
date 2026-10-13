package application_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/JustIkra/umir-serving/internal/application"

	"github.com/JustIkra/umir-serving/contract"
	"github.com/JustIkra/umir-serving/internal/adapters/memory"
)

func TestS3ResetIsIndeterminateInConfirmationAndResolution(t *testing.T) {
	for _, stage := range []string{"command", "order", "commit", "resolve"} {
		t.Run(stage, func(t *testing.T) {
			store := memory.NewProposalStore()
			w := newWarehouseFake()
			reader := &attemptReaderFake{result: contract.OrderAttempt{State: contract.OrderAttemptUnseen}}
			s := newOrdersServiceWithReader(t, store, w, reader)
			createProposal(t, store, proposalFixture("proposal-1", 2))
			reset := &contract.OperationError{Code: "RuntimeReset", Outcome: "indeterminate"}
			switch stage {
			case "command":
				w.findCommandErr = reset
			case "order":
				w.findProposalErr = reset
			case "commit":
				w.commitErr = reset
			case "resolve":
				w.commitErr = errors.New("reply lost")
			}
			req := contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: "proposal-1"}
			_, err := s.ConfirmOrder(context.Background(), req)
			if stage == "resolve" {
				reader.err = reset
				_, err = s.ResolveConfirmation(context.Background(), req)
			}
			var e *contract.OperationError
			if !errors.As(err, &e) || e.Code != "RuntimeReset" || e.Outcome != "indeterminate" {
				t.Fatalf("reset=%v", err)
			}
		})
	}
}

type s3ObservedWarehouse struct {
	*warehouseFake
	context contract.WarehouseContext
}

func (w *s3ObservedWarehouse) EvaluateOrder(ctx context.Context, r contract.EvaluateOrderRequest) (contract.OrderEvaluation, error) {
	contract.RecordWarehouseContext(ctx, w.context)
	return w.warehouseFake.EvaluateOrder(ctx, r)
}
func (w *s3ObservedWarehouse) FindRecordedCommand(ctx context.Context, id string) (contract.CommandRecord, bool, error) {
	contract.RecordWarehouseContext(ctx, w.context)
	return w.warehouseFake.FindRecordedCommand(ctx, id)
}
func (w *s3ObservedWarehouse) FindOrderByProposal(ctx context.Context, id string) (contract.Order, bool, error) {
	contract.RecordWarehouseContext(ctx, w.context)
	return w.warehouseFake.FindOrderByProposal(ctx, id)
}
func (w *s3ObservedWarehouse) CommitAcceptedOrder(ctx context.Context, r contract.CommitOrderRequest) (contract.CommitResult, error) {
	contract.RecordWarehouseContext(ctx, w.context)
	return w.warehouseFake.CommitAcceptedOrder(ctx, r)
}
func (w *s3ObservedWarehouse) FindOrderAttempt(ctx context.Context, _, _ string) (contract.OrderAttempt, error) {
	contract.RecordWarehouseContext(ctx, w.context)
	return contract.OrderAttempt{State: contract.OrderAttemptUnseen}, nil
}

func TestS3RetainedScopedUnknownNeverRecommitsAfterSettingsChange(t *testing.T) {
	store := memory.NewProposalStore()
	fake := newWarehouseFake()
	fake.evaluation.RequestedPositions = []contract.Position{{LineNo: 1, DishID: "dish-1", Portions: 2}}
	fake.evaluation.AcceptedPositions = []contract.AcceptedPosition{{LineNo: 1, DishID: "dish-1", Portions: 2}}
	original := contract.WarehouseContext{OrgID: "22222222-2222-4222-8222-222222222222", TenantID: "33333333-3333-4333-8333-333333333333", Epoch: "44444444-4444-4444-8444-444444444444", TimeZone: "Europe/Moscow", SettingsVersion: "1", EvaluatedAt: fake.evaluation.EvaluatedAt, StateVersion: fake.evaluation.StateVersion}
	w := &s3ObservedWarehouse{warehouseFake: fake, context: original}
	s, err := application.NewOrdersService(store, w, func() time.Time { return original.EvaluatedAt }, func() string { return "proposal-1" }, w)
	if err != nil {
		t.Fatal(err)
	}
	scope := contract.RequestScope{OrgID: original.OrgID, TenantID: original.TenantID, TimeZone: original.TimeZone, SettingsVersion: "1"}
	ctx := contract.WithRequestScope(context.Background(), scope)
	p, err := s.PrepareOrder(ctx, contract.PrepareOrderRequest{Positions: []contract.Position{{DishID: "dish-1", Portions: 2}}})
	if err != nil {
		t.Fatal(err)
	}
	req := contract.ConfirmOrderRequest{CommandID: "command-1", ProposalID: p.ProposalID}
	fake.commitErr = errors.New("lost reply")
	if _, err = s.ConfirmOrder(ctx, req); err == nil {
		t.Fatal("lost reply reported success")
	}
	scope.TimeZone = "Europe/Berlin"
	scope.SettingsVersion = "2"
	ctx = contract.WithRequestScope(context.Background(), scope)
	result, err := s.ConfirmOrder(ctx, req)
	_, commits := fake.commits()
	if err != nil || result.Kind != contract.ConfirmationPending || commits != 1 {
		t.Fatalf("retained retry=%+v/%v commits=%d", result, err, commits)
	}
	if _, err = s.ResolveConfirmation(ctx, req); err != nil {
		t.Fatal(err)
	}
	_, commits = fake.commits()
	if commits != 1 {
		t.Fatalf("passive resolution committed: %d", commits)
	}
}
