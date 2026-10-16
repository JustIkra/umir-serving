package tests_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/JustIkra/umir-serving/contract"
	"github.com/JustIkra/umir-serving/internal/adapters/memory"
	"github.com/JustIkra/umir-serving/internal/domain"
)

// reservePending records a confirmation attempt for the command and makes it
// the proposal's pending owner, the way ConfirmOrder does before a commit.
func reservePending(t *testing.T, store *memory.ProposalStore, proposalID, commandID string) {
	t.Helper()
	record, ok, err := store.Get(context.Background(), proposalID)
	if err != nil || !ok {
		t.Fatalf("Get(%q) = (%v, %v), want record", proposalID, ok, err)
	}
	accepted := make([]contract.AcceptedPosition, len(record.Proposal.AcceptedPositions))
	for i, position := range record.Proposal.AcceptedPositions {
		accepted[i] = contract.AcceptedPosition(position)
	}
	attempt := contract.ConfirmationAttempt{CommandID: commandID, ProposalID: proposalID, Fingerprint: "fingerprint-" + commandID, AcceptedPositions: accepted}
	if _, _, err := store.BeginAttempt(context.Background(), attempt); err != nil {
		t.Fatal(err)
	}
	if err := store.ReserveAttempt(context.Background(), commandID); err != nil {
		t.Fatal(err)
	}
}

func supersededResult(proposalID string) contract.ConfirmResult {
	return contract.ConfirmResult{Kind: contract.ProposalSuperseded, SupersededBy: proposalID}
}

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
	reservePending(t, store, "proposal-1", "command-1")

	candidate := domain.OrderProposal{
		ProposalID:         "proposal-2",
		RequestedPositions: []domain.Position{{LineNo: 99, DishID: "dish-2", Portions: 9}},
		AcceptedPositions:  []domain.AcceptedPosition{{LineNo: 8, DishID: "dish-1", Portions: 2}},
	}
	if err := store.CompleteAttempt(context.Background(), "command-1", supersededResult("proposal-2"), nil, &candidate); err != nil {
		t.Fatal(err)
	}
	first, ok, err := store.Get(context.Background(), "proposal-2")
	if err != nil || !ok {
		t.Fatalf("replacement record = (%v, %v), want stored", ok, err)
	}
	if first.Proposal.RequestedPositions[0] != (domain.Position{LineNo: 8, DishID: "dish-1", Portions: 3}) {
		t.Fatalf("replacement requested positions = %+v, want prior accepted positions", first.Proposal.RequestedPositions)
	}
	old, ok, err := store.Get(context.Background(), "proposal-1")
	if err != nil || !ok || old.SupersededBy != "proposal-2" || old.PendingCommandID != "" {
		t.Fatalf("old record after replacement = (%+v, %v), err %v", old, ok, err)
	}
	candidate.ProposalID = "proposal-3"
	if err := store.CompleteAttempt(context.Background(), "command-1", supersededResult("proposal-2"), nil, &candidate); err != nil {
		t.Fatalf("repeated CompleteAttempt() error = %v, want idempotent success", err)
	}
	if _, ok, err := store.Get(context.Background(), "proposal-3"); err != nil || ok {
		t.Fatalf("repeated replacement stored proposal-3: (%v, %v)", ok, err)
	}
	old, ok, err = store.Get(context.Background(), "proposal-1")
	if err != nil || !ok || old.SupersededBy != "proposal-2" {
		t.Fatalf("old record after repeated replacement = (%+v, %v), err %v", old, ok, err)
	}
	newRecord, ok, err := store.Get(context.Background(), "proposal-2")
	if err != nil || !ok || newRecord.Proposal.RequestedPositions[0] != (domain.Position{LineNo: 8, DishID: "dish-1", Portions: 3}) {
		t.Fatalf("new record = (%+v, %v), err %v", newRecord, ok, err)
	}
	attempt, ok, err := store.GetAttempt(context.Background(), "command-1")
	if err != nil || !ok || attempt.State != contract.OrderAttemptCompleted || attempt.Pending {
		t.Fatalf("attempt after replacement = (%+v, %v), err %v", attempt, ok, err)
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
	reservePending(t, store, "proposal-1", "command-1")
	err := store.CompleteAttempt(context.Background(), "command-1", supersededResult("proposal-taken"), nil, &domain.OrderProposal{ProposalID: "proposal-taken"})
	if !errors.Is(err, memory.ErrProposalExists) {
		t.Fatalf("CompleteAttempt() with an existing replacement ID error = %v, want ErrProposalExists", err)
	}
	err = store.CompleteAttempt(context.Background(), "command-1", supersededResult("proposal-over"), nil, &domain.OrderProposal{
		ProposalID:        "proposal-over",
		AcceptedPositions: []domain.AcceptedPosition{{LineNo: 1, DishID: "dish-1", Portions: 3}},
	})
	if !errors.Is(err, memory.ErrInvalidReplace) {
		t.Fatalf("CompleteAttempt() that increased an accepted position error = %v, want ErrInvalidReplace", err)
	}
	record, ok, err := store.Get(context.Background(), "proposal-1")
	if err != nil || !ok || record.SupersededBy != "" || record.PendingCommandID != "command-1" {
		t.Fatalf("old active record after failure = (%+v, %v), err %v", record, ok, err)
	}
	attempt, ok, err := store.GetAttempt(context.Background(), "command-1")
	if err != nil || !ok || attempt.State != contract.OrderAttemptPending || !attempt.Pending {
		t.Fatalf("attempt after failure = (%+v, %v), err %v", attempt, ok, err)
	}
	if _, ok, err := store.Get(context.Background(), "proposal-over"); err != nil || ok {
		t.Fatalf("failed replacement was stored: (%v, %v)", ok, err)
	}
}

func TestConcurrentReplacement(t *testing.T) {
	store := memory.NewProposalStore()
	if err := store.Create(context.Background(), domain.OrderProposal{ProposalID: "proposal-1", AcceptedPositions: []domain.AcceptedPosition{{LineNo: 1, DishID: "dish-1", Portions: 1}}}); err != nil {
		t.Fatal(err)
	}
	reservePending(t, store, "proposal-1", "command-1")
	ids := []string{"proposal-2", "proposal-3"}
	start := make(chan struct{})
	errs := make(chan error, len(ids))
	var workers sync.WaitGroup
	for _, id := range ids {
		workers.Add(1)
		go func(proposalID string) {
			defer workers.Done()
			<-start
			// Every caller carries the same outcome, as a repeated Confirm does;
			// only the proposed replacement ID differs.
			errs <- store.CompleteAttempt(context.Background(), "command-1", supersededResult("winner"), nil, &domain.OrderProposal{ProposalID: proposalID})
		}(id)
	}
	close(start)
	workers.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	old, ok, err := store.Get(context.Background(), "proposal-1")
	if err != nil || !ok || old.PendingCommandID != "" || (old.SupersededBy != "proposal-2" && old.SupersededBy != "proposal-3") {
		t.Fatalf("old record after concurrent replacement = (%+v, %v), err %v", old, ok, err)
	}
	stored := 0
	for _, id := range ids {
		if _, ok, err := store.Get(context.Background(), id); err != nil {
			t.Fatal(err)
		} else if ok {
			stored++
			if id != old.SupersededBy {
				t.Fatalf("stored replacement %q, but old record points to %q", id, old.SupersededBy)
			}
		}
	}
	if stored != 1 {
		t.Fatalf("concurrent replacements stored %d proposals, want exactly one", stored)
	}
}

func TestPendingIdentity(t *testing.T) {
	store := memory.NewProposalStore()
	if err := store.Create(context.Background(), domain.OrderProposal{ProposalID: "proposal-1"}); err != nil {
		t.Fatal(err)
	}
	reservePending(t, store, "proposal-1", "command-1")
	if _, _, err := store.BeginAttempt(context.Background(), contract.ConfirmationAttempt{CommandID: "command-2", ProposalID: "proposal-1", Fingerprint: "fingerprint-command-2", Pending: true}); !errors.Is(err, contract.ErrConfirmationPending) {
		t.Fatalf("BeginAttempt() of a different pending command error = %v, want ErrConfirmationPending", err)
	}
	if _, _, err := store.BeginAttempt(context.Background(), contract.ConfirmationAttempt{CommandID: "command-2", ProposalID: "proposal-1", Fingerprint: "fingerprint-command-2"}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReserveAttempt(context.Background(), "command-2"); !errors.Is(err, contract.ErrConfirmationPending) {
		t.Fatalf("ReserveAttempt() of a different command error = %v, want ErrConfirmationPending", err)
	}
	// Finishing a command that does not own the marker must leave it alone.
	if err := store.CompleteAttempt(context.Background(), "command-2", contract.ConfirmResult{Kind: contract.EmptyOrder}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteAttempt(context.Background(), "command-missing", contract.ConfirmResult{Kind: contract.EmptyOrder}, nil, nil); !errors.Is(err, memory.ErrProposalMissing) {
		t.Fatalf("CompleteAttempt() of an unknown command error = %v, want ErrProposalMissing", err)
	}
	record, ok, err := store.Get(context.Background(), "proposal-1")
	if err != nil || !ok || record.PendingCommandID != "command-1" {
		t.Fatalf("pending record = (%+v, %v), err %v", record, ok, err)
	}
	if err := store.CompleteAttempt(context.Background(), "command-1", contract.ConfirmResult{Kind: contract.EmptyOrder}, nil, nil); err != nil {
		t.Fatal(err)
	}
	record, ok, err = store.Get(context.Background(), "proposal-1")
	if err != nil || !ok || record.PendingCommandID != "" {
		t.Fatalf("resolved record = (%+v, %v), err %v", record, ok, err)
	}
}
