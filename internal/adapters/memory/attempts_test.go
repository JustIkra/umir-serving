package memory

import (
	"context"
	"errors"
	"testing"

	"github.com/JustIkra/umir-serving/contract"
	"github.com/JustIkra/umir-serving/internal/domain"
)

func TestBeginAttemptBindsCommandAcrossProposals(t *testing.T) {
	store := NewProposalStore()
	createAttemptProposal(t, store, "proposal-1")
	createAttemptProposal(t, store, "proposal-2")
	first := contract.ConfirmationAttempt{
		CommandID:         "command-1",
		ProposalID:        "proposal-1",
		Fingerprint:       "fingerprint-1",
		AcceptedPositions: []contract.AcceptedPosition{{LineNo: 1, DishID: "dish-1", Portions: 2}},
	}
	if _, created, err := store.BeginAttempt(context.Background(), first); err != nil || !created {
		t.Fatalf("BeginAttempt(first) = created %v, error %v; want true, nil", created, err)
	}

	second := first
	second.ProposalID = "proposal-2"
	second.AcceptedPositions = []contract.AcceptedPosition{{LineNo: 1, DishID: "dish-2", Portions: 2}}
	if _, created, err := store.BeginAttempt(context.Background(), second); !errors.Is(err, ErrCommandIdentityConflict) || created {
		t.Fatalf("BeginAttempt(second) = created %v, error %v; want false, command identity conflict", created, err)
	}

	record, found, err := store.GetAttempt(context.Background(), "command-1")
	if err != nil || !found || record.ProposalID != "proposal-1" || record.AcceptedPositions[0].DishID != "dish-1" {
		t.Fatalf("GetAttempt() = %#v, found %v, error %v; want original binding", record, found, err)
	}
}

func TestAttemptSnapshotsDoNotExposeMutableSlices(t *testing.T) {
	store := NewProposalStore()
	createAttemptProposal(t, store, "proposal-1")
	attempt := contract.ConfirmationAttempt{
		CommandID:         "command-1",
		ProposalID:        "proposal-1",
		Fingerprint:       "fingerprint-1",
		AcceptedPositions: []contract.AcceptedPosition{{LineNo: 1, DishID: "dish-1", Portions: 2}},
	}
	if _, _, err := store.BeginAttempt(context.Background(), attempt); err != nil {
		t.Fatal(err)
	}
	attempt.AcceptedPositions[0].DishID = "mutated-input"
	record, found, err := store.GetAttempt(context.Background(), "command-1")
	if err != nil || !found || record.AcceptedPositions[0].DishID != "dish-1" {
		t.Fatalf("GetAttempt() = %#v, found %v, error %v; want copied original input", record, found, err)
	}
	record.AcceptedPositions[0].DishID = "mutated-output"
	record, found, err = store.GetAttempt(context.Background(), "command-1")
	if err != nil || !found || record.AcceptedPositions[0].DishID != "dish-1" {
		t.Fatalf("GetAttempt() returned shared slice: %#v", record.AcceptedPositions)
	}
}

func TestCompleteAttemptAtomicallySavesResultAndReplacesProposal(t *testing.T) {
	store := NewProposalStore()
	createAttemptProposal(t, store, "proposal-1")
	attempt := contract.ConfirmationAttempt{
		CommandID:         "command-1",
		ProposalID:        "proposal-1",
		Fingerprint:       "fingerprint-1",
		AcceptedPositions: []contract.AcceptedPosition{{LineNo: 1, DishID: "dish-1", Portions: 2}},
		Pending:           true,
	}
	if _, _, err := store.BeginAttempt(context.Background(), attempt); err != nil {
		t.Fatal(err)
	}
	replacement := domain.OrderProposal{ProposalID: "proposal-2", AcceptedPositions: []domain.AcceptedPosition{{LineNo: 1, DishID: "dish-1", Portions: 1}}}
	result := contract.ConfirmResult{Kind: contract.RequiresConfirmation}
	if err := store.CompleteAttempt(context.Background(), "command-1", result, nil, &replacement); err != nil {
		t.Fatal(err)
	}
	record, found, err := store.GetAttempt(context.Background(), "command-1")
	if err != nil || !found || record.State != contract.OrderAttemptCompleted || record.Result == nil || record.Result.Kind != result.Kind {
		t.Fatalf("GetAttempt() = %#v, found %v, error %v", record, found, err)
	}
	old, found, err := store.Get(context.Background(), "proposal-1")
	if err != nil || !found || old.SupersededBy != "proposal-2" || old.PendingCommandID != "" {
		t.Fatalf("old proposal = %#v, found %v, error %v", old, found, err)
	}
	newRecord, found, err := store.Get(context.Background(), "proposal-2")
	if err != nil || !found || newRecord.Proposal.AcceptedPositions[0].Portions != 1 {
		t.Fatalf("replacement = %#v, found %v, error %v", newRecord, found, err)
	}
}

func createAttemptProposal(t *testing.T, store *ProposalStore, proposalID string) {
	t.Helper()
	if err := store.Create(context.Background(), domain.OrderProposal{ProposalID: proposalID, AcceptedPositions: []domain.AcceptedPosition{{LineNo: 1, DishID: "dish-1", Portions: 2}}}); err != nil {
		t.Fatal(err)
	}
}
