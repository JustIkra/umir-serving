package tests_test

import (
	"context"
	"sync"
	"testing"

	"github.com/JustIkra/umir-serving/internal/adapters/memory"
	"github.com/JustIkra/umir-serving/internal/domain"
)

func TestProposalImmutable(t *testing.T) {
	store := memory.NewProposalStore()
	proposal := domain.OrderProposal{
		ProposalID:         "proposal-1",
		RequestedPositions: []domain.Position{{LineNo: 4, DishID: "dish-1", Portions: 3}},
		AcceptedPositions:  []domain.AcceptedPosition{{LineNo: 4, DishID: "dish-1", Portions: 2}},
		Changes:            []domain.Change{{LineNo: 4, DishID: "dish-1", Deficits: []domain.Deficit{{ProductID: "product-1", RequiredQuantity: 3000, AvailableQuantity: 2000}}}},
	}
	if err := store.Create(context.Background(), proposal); err != nil {
		t.Fatal(err)
	}
	proposal.RequestedPositions[0].DishID = "changed"
	proposal.AcceptedPositions[0].Portions = 9
	proposal.Changes[0].Deficits[0].AvailableQuantity = 0

	record, ok, err := store.Get(context.Background(), "proposal-1")
	if err != nil || !ok {
		t.Fatalf("Get() = (%v, %v), want record", ok, err)
	}
	if record.Proposal.RequestedPositions[0].DishID != "dish-1" || record.Proposal.AcceptedPositions[0].Portions != 2 || record.Proposal.Changes[0].Deficits[0].AvailableQuantity != 2000 {
		t.Fatalf("stored proposal changed through input: %+v", record.Proposal)
	}
	record.Proposal.RequestedPositions[0].DishID = "changed again"
	record.Proposal.AcceptedPositions[0].Portions = 10
	record.Proposal.Changes[0].Deficits[0].RequiredQuantity = 0

	again, ok, err := store.Get(context.Background(), "proposal-1")
	if err != nil || !ok {
		t.Fatalf("second Get() = (%v, %v), want record", ok, err)
	}
	if again.Proposal.RequestedPositions[0].DishID != "dish-1" || again.Proposal.AcceptedPositions[0].Portions != 2 || again.Proposal.Changes[0].Deficits[0].RequiredQuantity != 3000 {
		t.Fatalf("stored proposal changed through output: %+v", again.Proposal)
	}
}

func TestReplaceAtomic(t *testing.T) {
	store := memory.NewProposalStore()
	original := domain.OrderProposal{
		ProposalID:         "proposal-1",
		RequestedPositions: []domain.Position{{LineNo: 8, DishID: "dish-1", Portions: 4}},
		AcceptedPositions:  []domain.AcceptedPosition{{LineNo: 8, DishID: "dish-1", Portions: 3}},
	}
	if err := store.Create(context.Background(), original); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkPending(context.Background(), "proposal-1", "command-1"); err != nil {
		t.Fatal(err)
	}

	candidate := domain.OrderProposal{
		ProposalID:         "proposal-2",
		RequestedPositions: []domain.Position{{LineNo: 99, DishID: "dish-2", Portions: 9}},
		AcceptedPositions:  []domain.AcceptedPosition{{LineNo: 8, DishID: "dish-1", Portions: 2}},
	}
	first, err := store.Replace(context.Background(), "proposal-1", "command-1", candidate)
	if err != nil {
		t.Fatal(err)
	}
	if first.RequestedPositions[0] != (domain.Position{LineNo: 8, DishID: "dish-1", Portions: 3}) {
		t.Fatalf("replacement requested positions = %+v, want prior accepted positions", first.RequestedPositions)
	}
	old, ok, err := store.Get(context.Background(), "proposal-1")
	if err != nil || !ok || old.SupersededBy != "proposal-2" || old.PendingCommandID != "" {
		t.Fatalf("old record after replacement = (%+v, %v), err %v", old, ok, err)
	}
	candidate.ProposalID = "proposal-3"
	repeated, err := store.Replace(context.Background(), "proposal-1", "command-1", candidate)
	if err != nil {
		t.Fatal(err)
	}
	if repeated.ProposalID != "proposal-2" {
		t.Fatalf("repeated replacement ID = %q, want proposal-2", repeated.ProposalID)
	}
	newRecord, ok, err := store.Get(context.Background(), "proposal-2")
	if err != nil || !ok || newRecord.Proposal.RequestedPositions[0] != (domain.Position{LineNo: 8, DishID: "dish-1", Portions: 3}) {
		t.Fatalf("new record = (%+v, %v), err %v", newRecord, ok, err)
	}
}

func TestReplaceFailureKeepsActive(t *testing.T) {
	store := memory.NewProposalStore()
	if err := store.Create(context.Background(), domain.OrderProposal{ProposalID: "proposal-1", AcceptedPositions: []domain.AcceptedPosition{{LineNo: 1, DishID: "dish-1", Portions: 2}}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(context.Background(), domain.OrderProposal{ProposalID: "proposal-taken"}); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkPending(context.Background(), "proposal-1", "command-1"); err != nil {
		t.Fatal(err)
	}
	_, err := store.Replace(context.Background(), "proposal-1", "command-1", domain.OrderProposal{ProposalID: "proposal-taken"})
	if err == nil {
		t.Fatal("Replace() succeeded with an existing replacement ID")
	}
	_, err = store.Replace(context.Background(), "proposal-1", "command-1", domain.OrderProposal{
		ProposalID:        "proposal-over",
		AcceptedPositions: []domain.AcceptedPosition{{LineNo: 1, DishID: "dish-1", Portions: 3}},
	})
	if err == nil {
		t.Fatal("Replace() increased an accepted position")
	}
	record, ok, err := store.Get(context.Background(), "proposal-1")
	if err != nil || !ok || record.SupersededBy != "" || record.PendingCommandID != "command-1" {
		t.Fatalf("old active record after failure = (%+v, %v), err %v", record, ok, err)
	}
}

func TestConcurrentReplacement(t *testing.T) {
	store := memory.NewProposalStore()
	if err := store.Create(context.Background(), domain.OrderProposal{ProposalID: "proposal-1", AcceptedPositions: []domain.AcceptedPosition{{LineNo: 1, DishID: "dish-1", Portions: 1}}}); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkPending(context.Background(), "proposal-1", "command-1"); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan domain.OrderProposal, 2)
	errors := make(chan error, 2)
	var workers sync.WaitGroup
	for _, id := range []string{"proposal-2", "proposal-3"} {
		workers.Add(1)
		go func(proposalID string) {
			defer workers.Done()
			<-start
			proposal, err := store.Replace(context.Background(), "proposal-1", "command-1", domain.OrderProposal{ProposalID: proposalID})
			if err != nil {
				errors <- err
				return
			}
			results <- proposal
		}(id)
	}
	close(start)
	workers.Wait()
	close(results)
	close(errors)
	for err := range errors {
		t.Fatal(err)
	}
	var firstID string
	for result := range results {
		if firstID == "" {
			firstID = result.ProposalID
		} else if result.ProposalID != firstID {
			t.Fatalf("concurrent replacements returned %q and %q", firstID, result.ProposalID)
		}
	}
	old, ok, err := store.Get(context.Background(), "proposal-1")
	if err != nil || !ok || old.SupersededBy != firstID || old.PendingCommandID != "" {
		t.Fatalf("old record after concurrent replacement = (%+v, %v), err %v", old, ok, err)
	}
}

func TestPendingIdentity(t *testing.T) {
	store := memory.NewProposalStore()
	if err := store.Create(context.Background(), domain.OrderProposal{ProposalID: "proposal-1"}); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkPending(context.Background(), "proposal-1", "command-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkPending(context.Background(), "proposal-1", "command-2"); err == nil {
		t.Fatal("MarkPending() accepted a different pending command")
	}
	if err := store.ResolvePending(context.Background(), "proposal-1", "command-2"); err == nil {
		t.Fatal("ResolvePending() accepted a different command ID")
	}
	record, ok, err := store.Get(context.Background(), "proposal-1")
	if err != nil || !ok || record.PendingCommandID != "command-1" {
		t.Fatalf("pending record = (%+v, %v), err %v", record, ok, err)
	}
	if err := store.ResolvePending(context.Background(), "proposal-1", "command-1"); err != nil {
		t.Fatal(err)
	}
	record, ok, err = store.Get(context.Background(), "proposal-1")
	if err != nil || !ok || record.PendingCommandID != "" {
		t.Fatalf("resolved record = (%+v, %v), err %v", record, ok, err)
	}
}
