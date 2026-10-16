package storecontract

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/JustIkra/umir-serving/contract"
	"github.com/JustIkra/umir-serving/internal/application"
	"github.com/JustIkra/umir-serving/internal/domain"
)

// Run executes the whole suite against stores made by newFixture.
func Run(t *testing.T, newFixture Factory) {
	cases := []struct {
		name string
		run  func(*testing.T, Factory)
	}{
		{"ProposalIsImmutable", proposalIsImmutable},
		{"TimesRoundTripWithNanoseconds", timesRoundTrip},
		{"AttemptRoundTripKeepsEveryField", attemptRoundTrip},
		{"AttemptSnapshotsAreCopies", attemptSnapshots},
		{"BeginAttemptIsIdempotentAndConflicts", beginAttempt},
		{"BeginAttemptBindsCommandAcrossProposals", beginAttemptAcrossProposals},
		{"ParallelReserveHasOneWinner", parallelReserve},
		{"ReplacementIsAtomic", replacementAtomic},
		{"FailedReplacementKeepsProposalActive", failedReplacement},
		{"ConcurrentReplacementStoresOne", concurrentReplacement},
		{"PendingIdentity", pendingIdentity},
		{"ReplacementIsRememberedOnce", replacementOnce},
		{"LeaseGrantExpiryAndFence", leaseFence},
		{"LeaseOfMissingProposal", leaseMissingProposal},
		{"ActiveProposalLimit", activeLimit},
		{"RestaurantsAreIsolated", isolation},
		{"CheckBookkeeping", checkBookkeeping},
		{"RetentionKeepsPendingAndDeletesTheRest", retention},
		{"RetentionRespectsTheBatchLimit", retentionLimit},
		{"PickDueSelectsReservedAttemptsOnce", pickDue},
		{"ParallelPickersDoNotOverlap", parallelPickers},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { c.run(t, newFixture) })
	}
}

var ctxBG = context.Background

func proposalOf(id string, accepted ...domain.AcceptedPosition) domain.OrderProposal {
	if accepted == nil {
		accepted = []domain.AcceptedPosition{{LineNo: 1, DishID: "dish-1", Portions: 2}}
	}
	return domain.OrderProposal{
		ProposalID:         id,
		EvaluatedAt:        time.Now().UTC(),
		StateVersion:       7,
		RequestedPositions: []domain.Position{{LineNo: 1, DishID: "dish-1", Portions: 3}},
		AcceptedPositions:  accepted,
	}
}

func mustCreate(t *testing.T, s Store, p domain.OrderProposal) {
	t.Helper()
	if err := s.Create(ctxBG(), p); err != nil {
		t.Fatalf("Create(%q): %v", p.ProposalID, err)
	}
}

func accepted(record domain.ProposalRecord) []contract.AcceptedPosition {
	out := make([]contract.AcceptedPosition, len(record.Proposal.AcceptedPositions))
	for i, position := range record.Proposal.AcceptedPositions {
		out[i] = contract.AcceptedPosition(position)
	}
	return out
}

// reservePending records an attempt for the command and makes it the owner of
// the proposal, the way ConfirmOrder does before a commit.
func reservePending(t *testing.T, s Store, proposalID, commandID string) {
	t.Helper()
	record, ok, err := s.Get(ctxBG(), proposalID)
	if err != nil || !ok {
		t.Fatalf("Get(%q) = (%v, %v)", proposalID, ok, err)
	}
	attempt := contract.ConfirmationAttempt{CommandID: commandID, ProposalID: proposalID, Fingerprint: "fp-" + commandID, AcceptedPositions: accepted(record)}
	if _, _, err := s.BeginAttempt(ctxBG(), attempt); err != nil {
		t.Fatalf("BeginAttempt(%q): %v", commandID, err)
	}
	if err := s.ReserveAttempt(ctxBG(), commandID); err != nil {
		t.Fatalf("ReserveAttempt(%q): %v", commandID, err)
	}
}

func superseded(id string) contract.ConfirmResult {
	return contract.ConfirmResult{Kind: contract.ProposalSuperseded, SupersededBy: id}
}

func proposalIsImmutable(t *testing.T, f Factory) {
	s := f(t, Options{}).Store
	p := domain.OrderProposal{
		ProposalID:         "proposal-1",
		EvaluatedAt:        time.Now().UTC(),
		RequestedPositions: []domain.Position{{LineNo: 4, DishID: "dish-1", Portions: 3}},
		AcceptedPositions:  []domain.AcceptedPosition{{LineNo: 4, DishID: "dish-1", Portions: 2}},
		Changes:            []domain.Change{{LineNo: 4, DishID: "dish-1", Deficits: []domain.Deficit{{ProductID: "product-1", RequiredQuantity: 3000, AvailableQuantity: 2000}}}},
	}
	mustCreate(t, s, p)
	p.RequestedPositions[0].DishID = "changed"
	p.AcceptedPositions[0].Portions = 9
	p.Changes[0].Deficits[0].AvailableQuantity = 0
	record, ok, err := s.Get(ctxBG(), "proposal-1")
	if err != nil || !ok {
		t.Fatalf("Get() = (%v, %v)", ok, err)
	}
	if record.Proposal.RequestedPositions[0].DishID != "dish-1" || record.Proposal.AcceptedPositions[0].Portions != 2 || record.Proposal.Changes[0].Deficits[0].AvailableQuantity != 2000 {
		t.Fatalf("stored proposal changed through input: %+v", record.Proposal)
	}
	record.Proposal.RequestedPositions[0].DishID = "changed again"
	record.Proposal.Changes[0].Deficits[0].RequiredQuantity = 0
	again, _ := getProposal(t, s, "proposal-1")
	if again.Proposal.RequestedPositions[0].DishID != "dish-1" || again.Proposal.Changes[0].Deficits[0].RequiredQuantity != 3000 {
		t.Fatalf("stored proposal changed through output: %+v", again.Proposal)
	}
	if err := s.Create(ctxBG(), p); !errors.Is(err, domain.ErrProposalExists) {
		t.Fatalf("second Create error = %v, want ErrProposalExists", err)
	}
	if _, ok, err := s.Get(ctxBG(), "absent"); ok || err != nil {
		t.Fatalf("Get(absent) = (%v, %v)", ok, err)
	}
}

// timesRoundTrip: timestamptz truncates to microseconds, so exact instants are
// kept outside it.
func timesRoundTrip(t *testing.T, f Factory) {
	s := f(t, Options{}).Store
	at := time.Date(2026, 10, 16, 9, 30, 15, 123456789, time.FixedZone("MSK", 3*3600))
	p := proposalOf("proposal-ns")
	p.EvaluatedAt = at
	p.Context = domain.ProposalContext{OrgID: "org", TenantID: "tenant", Epoch: "epoch", TimeZone: "Europe/Moscow", SettingsVersion: "3", EvaluatedAt: at, StateVersion: 9007199254740993}
	p.StateVersion = 9007199254740993
	mustCreate(t, s, p)
	record, ok, err := s.Get(ctxBG(), "proposal-ns")
	if err != nil || !ok {
		t.Fatalf("Get: %v %v", ok, err)
	}
	if !record.Proposal.EvaluatedAt.Equal(at) || !record.Proposal.Context.EvaluatedAt.Equal(at) || record.Proposal.StateVersion != p.StateVersion || record.Proposal.Context.StateVersion != p.StateVersion {
		t.Fatalf("instant or version lost: %+v", record.Proposal)
	}
	if record.Proposal.Context != p.Context && !record.Proposal.Context.EvaluatedAt.Equal(at) {
		t.Fatalf("context differs: %+v", record.Proposal.Context)
	}
}

func fullAttempt(command, proposal string) contract.ConfirmationAttempt {
	at := time.Date(2026, 10, 16, 9, 30, 15, 987654321, time.UTC)
	return contract.ConfirmationAttempt{
		Context:           contract.WarehouseContext{OrgID: "org", TenantID: "tenant", Epoch: "11111111-1111-4111-8111-111111111111", TimeZone: "Europe/Moscow", SettingsVersion: "4", EvaluatedAt: at, StateVersion: 77},
		CommandID:         command,
		ProposalID:        proposal,
		Fingerprint:       "fingerprint",
		AcceptedPositions: []contract.AcceptedPosition{{LineNo: 1, DishID: "dish-1", Portions: 2}},
	}
}

func attemptRoundTrip(t *testing.T, f Factory) {
	s := f(t, Options{}).Store
	mustCreate(t, s, proposalOf("proposal-1"))
	in := fullAttempt("command-1", "proposal-1")
	got, created, err := s.BeginAttempt(ctxBG(), in)
	if err != nil || !created {
		t.Fatalf("BeginAttempt = %v %v", created, err)
	}
	if got.State != contract.OrderAttemptPending || got.Pending || got.CreatedAt.IsZero() {
		t.Fatalf("new attempt = %+v", got)
	}
	if err := s.ReserveAttempt(ctxBG(), "command-1"); err != nil {
		t.Fatal(err)
	}
	order := contract.Order{OrderID: "order-1", ProposalID: "proposal-1", Positions: in.AcceptedPositions, Allocations: []contract.Allocation{{ProductID: "p", BatchID: "b", Quantity: 5}}}
	commit := contract.CommitResult{Context: in.Context, Kind: contract.OrderCommitted, Order: &order}
	if err := s.RememberWarehouseResult(ctxBG(), "command-1", commit, nil); err != nil {
		t.Fatal(err)
	}
	read, found, err := s.GetAttempt(ctxBG(), "command-1")
	if err != nil || !found || !read.Pending || read.WarehouseResult == nil || read.WarehouseResult.Order == nil || read.WarehouseResult.Order.OrderID != "order-1" {
		t.Fatalf("after remember: %+v %v %v", read, found, err)
	}
	if !read.Context.EvaluatedAt.Equal(in.Context.EvaluatedAt) || read.Context != (contract.WarehouseContext{OrgID: "org", TenantID: "tenant", Epoch: in.Context.Epoch, TimeZone: "Europe/Moscow", SettingsVersion: "4", EvaluatedAt: read.Context.EvaluatedAt, StateVersion: 77}) {
		t.Fatalf("context = %+v", read.Context)
	}
	result := contract.ConfirmResult{Context: in.Context, Kind: contract.OrderConfirmed, Order: &order}
	if err := s.CompleteAttempt(ctxBG(), "command-1", result, nil, nil); err != nil {
		t.Fatal(err)
	}
	done, _ := getAttempt(t, s, "command-1")
	if done.State != contract.OrderAttemptCompleted || done.Pending || done.Result == nil || done.Result.Order.OrderID != "order-1" || done.Fingerprint != "fingerprint" || len(done.AcceptedPositions) != 1 {
		t.Fatalf("completed = %+v", done)
	}
	if !done.Result.Context.EvaluatedAt.Equal(in.Context.EvaluatedAt) {
		t.Fatalf("result context instant lost: %v", done.Result.Context.EvaluatedAt)
	}
	// A rejected attempt keeps its error.
	mustCreate(t, s, proposalOf("proposal-2"))
	if _, _, err := s.BeginAttempt(ctxBG(), fullAttempt("command-2", "proposal-2")); err != nil {
		t.Fatal(err)
	}
	rejection := contract.AttemptError{Code: "CommandConflict", Message: "m", Outcome: "not_committed"}
	if err := s.CompleteAttempt(ctxBG(), "command-2", contract.ConfirmResult{}, &rejection, nil); err != nil {
		t.Fatal(err)
	}
	rej, _ := getAttempt(t, s, "command-2")
	if rej.State != contract.OrderAttemptRejected || rej.Error == nil || *rej.Error != rejection {
		t.Fatalf("rejected = %+v", rej)
	}
}

func attemptSnapshots(t *testing.T, f Factory) {
	s := f(t, Options{}).Store
	mustCreate(t, s, proposalOf("proposal-1"))
	in := fullAttempt("command-1", "proposal-1")
	if _, _, err := s.BeginAttempt(ctxBG(), in); err != nil {
		t.Fatal(err)
	}
	in.AcceptedPositions[0].DishID = "mutated-input"
	got, _ := getAttempt(t, s, "command-1")
	if got.AcceptedPositions[0].DishID != "dish-1" {
		t.Fatal("stored attempt changed through input")
	}
	got.AcceptedPositions[0].DishID = "mutated-output"
	again, _ := getAttempt(t, s, "command-1")
	if again.AcceptedPositions[0].DishID != "dish-1" {
		t.Fatal("stored attempt changed through output")
	}
}

func beginAttempt(t *testing.T, f Factory) {
	s := f(t, Options{}).Store
	mustCreate(t, s, proposalOf("proposal-1"))
	in := fullAttempt("command-1", "proposal-1")
	first, created, err := s.BeginAttempt(ctxBG(), in)
	if err != nil || !created {
		t.Fatalf("first = %v %v", created, err)
	}
	again, created, err := s.BeginAttempt(ctxBG(), in)
	if err != nil || created || again.CommandID != first.CommandID {
		t.Fatalf("repeat = %+v %v %v", again, created, err)
	}
	other := in
	other.Fingerprint = "different"
	if _, _, err := s.BeginAttempt(ctxBG(), other); !errors.Is(err, contract.ErrCommandIdentityConflict) {
		t.Fatalf("different fingerprint error = %v", err)
	}
	other = in
	other.AcceptedPositions = []contract.AcceptedPosition{{LineNo: 1, DishID: "dish-9", Portions: 1}}
	if _, _, err := s.BeginAttempt(ctxBG(), other); !errors.Is(err, contract.ErrCommandIdentityConflict) {
		t.Fatalf("different positions error = %v", err)
	}
	if _, _, err := s.BeginAttempt(ctxBG(), contract.ConfirmationAttempt{CommandID: "x", ProposalID: "", Fingerprint: "f"}); !errors.Is(err, domain.ErrInvalidProposal) {
		t.Fatalf("invalid attempt error = %v", err)
	}
	if _, found, err := s.GetAttempt(ctxBG(), "absent"); found || err != nil {
		t.Fatalf("GetAttempt(absent) = %v %v", found, err)
	}
}

func beginAttemptAcrossProposals(t *testing.T, f Factory) {
	s := f(t, Options{}).Store
	mustCreate(t, s, proposalOf("proposal-1"))
	mustCreate(t, s, proposalOf("proposal-2"))
	first := fullAttempt("command-1", "proposal-1")
	if _, created, err := s.BeginAttempt(ctxBG(), first); err != nil || !created {
		t.Fatal(created, err)
	}
	second := first
	second.ProposalID = "proposal-2"
	second.AcceptedPositions = []contract.AcceptedPosition{{LineNo: 1, DishID: "dish-2", Portions: 2}}
	if _, created, err := s.BeginAttempt(ctxBG(), second); !errors.Is(err, contract.ErrCommandIdentityConflict) || created {
		t.Fatalf("second = %v %v", created, err)
	}
	record, found, err := s.GetAttempt(ctxBG(), "command-1")
	if err != nil || !found || record.ProposalID != "proposal-1" || record.AcceptedPositions[0].DishID != "dish-1" {
		t.Fatalf("original binding lost: %+v", record)
	}
	// An attempt that claims the proposal or carries positions needs it to exist.
	ghost := fullAttempt("command-ghost", "proposal-ghost")
	if _, _, err := s.BeginAttempt(ctxBG(), ghost); !errors.Is(err, domain.ErrProposalMissing) {
		t.Fatalf("attempt for a missing proposal error = %v", err)
	}
	ghost.AcceptedPositions = nil
	if _, created, err := s.BeginAttempt(ctxBG(), ghost); err != nil || !created {
		t.Fatalf("attempt without positions for a missing proposal = %v %v", created, err)
	}
}

func parallelReserve(t *testing.T, f Factory) {
	s := f(t, Options{}).Store
	mustCreate(t, s, proposalOf("proposal-1"))
	const n = 16
	for i := range n {
		id := fmt.Sprintf("command-%d", i)
		if _, _, err := s.BeginAttempt(ctxBG(), fullAttempt(id, "proposal-1")); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	results := make([]error, n)
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			<-start
			results[i] = s.ReserveAttempt(ctxBG(), fmt.Sprintf("command-%d", i))
		})
	}
	close(start)
	wg.Wait()
	winners := 0
	for i, err := range results {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, contract.ErrConfirmationPending):
		default:
			t.Fatalf("command %d: unexpected error %v", i, err)
		}
	}
	if winners != 1 {
		t.Fatalf("%d winners, want exactly 1", winners)
	}
	record, _ := getProposal(t, s, "proposal-1")
	if record.PendingCommandID == "" {
		t.Fatal("proposal has no pending owner")
	}
}

func replacementAtomic(t *testing.T, f Factory) {
	s := f(t, Options{}).Store
	original := domain.OrderProposal{ProposalID: "proposal-1", EvaluatedAt: time.Now(), RequestedPositions: []domain.Position{{LineNo: 8, DishID: "dish-1", Portions: 4}}, AcceptedPositions: []domain.AcceptedPosition{{LineNo: 8, DishID: "dish-1", Portions: 3}}}
	mustCreate(t, s, original)
	reservePending(t, s, "proposal-1", "command-1")
	candidate := domain.OrderProposal{ProposalID: "proposal-2", EvaluatedAt: time.Now(), RequestedPositions: []domain.Position{{LineNo: 99, DishID: "dish-2", Portions: 9}}, AcceptedPositions: []domain.AcceptedPosition{{LineNo: 8, DishID: "dish-1", Portions: 2}}}
	if err := s.CompleteAttempt(ctxBG(), "command-1", superseded("proposal-2"), nil, &candidate); err != nil {
		t.Fatal(err)
	}
	next, ok, err := s.Get(ctxBG(), "proposal-2")
	if err != nil || !ok || next.Proposal.RequestedPositions[0] != (domain.Position{LineNo: 8, DishID: "dish-1", Portions: 3}) {
		t.Fatalf("replacement = %+v %v %v", next, ok, err)
	}
	old, ok, err := s.Get(ctxBG(), "proposal-1")
	if err != nil || !ok || old.SupersededBy != "proposal-2" || old.PendingCommandID != "" {
		t.Fatalf("old = %+v %v %v", old, ok, err)
	}
	candidate.ProposalID = "proposal-3"
	if err := s.CompleteAttempt(ctxBG(), "command-1", superseded("proposal-2"), nil, &candidate); err != nil {
		t.Fatalf("repeated completion error = %v, want success without effect", err)
	}
	if _, ok := getProposal(t, s, "proposal-3"); ok {
		t.Fatal("repeated completion stored another replacement")
	}
	attempt, _ := getAttempt(t, s, "command-1")
	if attempt.State != contract.OrderAttemptCompleted || attempt.Pending {
		t.Fatalf("attempt = %+v", attempt)
	}
	// A different outcome for a finished command is a conflict.
	if err := s.CompleteAttempt(ctxBG(), "command-1", contract.ConfirmResult{Kind: contract.EmptyOrder}, nil, nil); !errors.Is(err, contract.ErrCommandIdentityConflict) {
		t.Fatalf("different outcome error = %v", err)
	}
}

func failedReplacement(t *testing.T, f Factory) {
	s := f(t, Options{}).Store
	mustCreate(t, s, proposalOf("proposal-1"))
	mustCreate(t, s, proposalOf("proposal-taken"))
	reservePending(t, s, "proposal-1", "command-1")
	err := s.CompleteAttempt(ctxBG(), "command-1", superseded("proposal-taken"), nil, &domain.OrderProposal{ProposalID: "proposal-taken", EvaluatedAt: time.Now(), AcceptedPositions: []domain.AcceptedPosition{{LineNo: 1, DishID: "dish-1", Portions: 1}}})
	if !errors.Is(err, domain.ErrProposalExists) {
		t.Fatalf("existing replacement ID error = %v", err)
	}
	err = s.CompleteAttempt(ctxBG(), "command-1", superseded("proposal-over"), nil, &domain.OrderProposal{ProposalID: "proposal-over", EvaluatedAt: time.Now(), AcceptedPositions: []domain.AcceptedPosition{{LineNo: 1, DishID: "dish-1", Portions: 3}}})
	if !errors.Is(err, domain.ErrInvalidReplace) {
		t.Fatalf("increased position error = %v", err)
	}
	record, ok := getProposal(t, s, "proposal-1")
	if !ok || record.SupersededBy != "" || record.PendingCommandID != "command-1" {
		t.Fatalf("old proposal after failure = %+v", record)
	}
	attempt, _ := getAttempt(t, s, "command-1")
	if attempt.State != contract.OrderAttemptPending || !attempt.Pending {
		t.Fatalf("attempt after failure = %+v", attempt)
	}
	if _, ok := getProposal(t, s, "proposal-over"); ok {
		t.Fatal("failed replacement was stored")
	}
}

func concurrentReplacement(t *testing.T, f Factory) {
	s := f(t, Options{}).Store
	mustCreate(t, s, proposalOf("proposal-1", domain.AcceptedPosition{LineNo: 1, DishID: "dish-1", Portions: 1}))
	reservePending(t, s, "proposal-1", "command-1")
	ids := []string{"proposal-2", "proposal-3"}
	errs := make(chan error, len(ids))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Go(func() {
			<-start
			errs <- s.CompleteAttempt(ctxBG(), "command-1", superseded("winner"), nil, &domain.OrderProposal{ProposalID: id, EvaluatedAt: time.Now(), AcceptedPositions: []domain.AcceptedPosition{{LineNo: 1, DishID: "dish-1", Portions: 1}}})
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	old, _ := getProposal(t, s, "proposal-1")
	if old.PendingCommandID != "" || (old.SupersededBy != "proposal-2" && old.SupersededBy != "proposal-3") {
		t.Fatalf("old = %+v", old)
	}
	stored := 0
	for _, id := range ids {
		if _, ok := getProposal(t, s, id); ok {
			stored++
			if id != old.SupersededBy {
				t.Fatalf("stored %q but old points to %q", id, old.SupersededBy)
			}
		}
	}
	if stored != 1 {
		t.Fatalf("stored %d replacements, want 1", stored)
	}
}

func pendingIdentity(t *testing.T, f Factory) {
	s := f(t, Options{}).Store
	mustCreate(t, s, proposalOf("proposal-1"))
	reservePending(t, s, "proposal-1", "command-1")
	rival := contract.ConfirmationAttempt{CommandID: "command-2", ProposalID: "proposal-1", Fingerprint: "fp-command-2", Pending: true}
	if _, _, err := s.BeginAttempt(ctxBG(), rival); !errors.Is(err, contract.ErrConfirmationPending) {
		t.Fatalf("pending rival error = %v", err)
	}
	rival.Pending = false
	if _, _, err := s.BeginAttempt(ctxBG(), rival); err != nil {
		t.Fatal(err)
	}
	if err := s.ReserveAttempt(ctxBG(), "command-2"); !errors.Is(err, contract.ErrConfirmationPending) {
		t.Fatalf("Reserve of a rival error = %v", err)
	}
	// Finishing a command that does not own the marker leaves the marker alone.
	if err := s.CompleteAttempt(ctxBG(), "command-2", contract.ConfirmResult{Kind: contract.EmptyOrder}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteAttempt(ctxBG(), "command-missing", contract.ConfirmResult{Kind: contract.EmptyOrder}, nil, nil); !errors.Is(err, domain.ErrProposalMissing) {
		t.Fatalf("unknown command error = %v", err)
	}
	record, _ := getProposal(t, s, "proposal-1")
	if record.PendingCommandID != "command-1" {
		t.Fatalf("pending owner = %q", record.PendingCommandID)
	}
	if err := s.CompleteAttempt(ctxBG(), "command-1", contract.ConfirmResult{Kind: contract.EmptyOrder}, nil, nil); err != nil {
		t.Fatal(err)
	}
	record, _ = getProposal(t, s, "proposal-1")
	if record.PendingCommandID != "" {
		t.Fatalf("pending owner after completion = %q", record.PendingCommandID)
	}
	// The same command reserving again is idempotent.
	mustCreate(t, s, proposalOf("proposal-9"))
	reservePending(t, s, "proposal-9", "command-9")
	if err := s.ReserveAttempt(ctxBG(), "command-9"); err != nil {
		t.Fatalf("repeated Reserve error = %v", err)
	}
}

func replacementOnce(t *testing.T, f Factory) {
	s := f(t, Options{}).Store
	mustCreate(t, s, proposalOf("proposal-1"))
	reservePending(t, s, "proposal-1", "command-1")
	ctx := fullAttempt("x", "y").Context
	evaluation := &contract.OrderEvaluation{EvaluatedAt: ctx.EvaluatedAt, StateVersion: 5}
	recheck := contract.CommitResult{Context: ctx, Kind: contract.RecheckRequired, OrderEvaluation: evaluation}
	first := contract.OrderProposal{ProposalID: "replacement-a", Context: ctx, EvaluatedAt: ctx.EvaluatedAt, StateVersion: 5}
	second := contract.OrderProposal{ProposalID: "replacement-b", Context: ctx, EvaluatedAt: ctx.EvaluatedAt, StateVersion: 5}
	if err := s.RememberWarehouseResult(ctxBG(), "command-1", recheck, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.RememberWarehouseResult(ctxBG(), "command-1", recheck, &first); err != nil {
		t.Fatal(err)
	}
	if err := s.RememberWarehouseResult(ctxBG(), "command-1", recheck, &second); err != nil {
		t.Fatal(err)
	}
	attempt, _ := getAttempt(t, s, "command-1")
	if attempt.Replacement == nil || attempt.Replacement.ProposalID != "replacement-a" {
		t.Fatalf("replacement = %+v, want the first one", attempt.Replacement)
	}
	other := contract.CommitResult{Context: ctx, Kind: contract.OrderCommitted, Order: &contract.Order{OrderID: "o", ProposalID: "proposal-1"}}
	if err := s.RememberWarehouseResult(ctxBG(), "command-1", other, nil); !errors.Is(err, contract.ErrCommandIdentityConflict) {
		t.Fatalf("different warehouse result error = %v", err)
	}
}

func leaseFence(t *testing.T, f Factory) {
	fx := f(t, Options{})
	s := fx.Store
	mustCreate(t, s, proposalOf("proposal-1"))
	reservePendingAttempt := func() {
		record, _ := getProposal(t, s, "proposal-1")
		if _, _, err := s.BeginAttempt(ctxBG(), contract.ConfirmationAttempt{CommandID: "command-1", ProposalID: "proposal-1", Fingerprint: "fp", AcceptedPositions: accepted(record)}); err != nil {
			t.Fatal(err)
		}
	}
	reservePendingAttempt()
	a, ok, err := s.AcquireLease(ctxBG(), "proposal-1", "holder-a", 30*time.Second)
	if err != nil || !ok || a.Seq < 1 || a.Holder != "holder-a" {
		t.Fatalf("first lease = %+v %v %v", a, ok, err)
	}
	if _, ok, err := s.AcquireLease(ctxBG(), "proposal-1", "holder-b", 30*time.Second); err != nil || ok {
		t.Fatalf("second holder got the lease: %v %v", ok, err)
	}
	// Expiry alone does not forbid the old holder to write...
	fx.Advance(31 * time.Second)
	if err := s.ReserveAttempt(application.WithLease(ctxBG(), a), "command-1"); err != nil {
		t.Fatalf("write after expiry without takeover: %v", err)
	}
	// ...but a takeover does.
	b, ok, err := s.AcquireLease(ctxBG(), "proposal-1", "holder-b", 30*time.Second)
	if err != nil || !ok || b.Seq <= a.Seq {
		t.Fatalf("takeover = %+v %v %v", b, ok, err)
	}
	if err := s.CompleteAttempt(application.WithLease(ctxBG(), a), "command-1", contract.ConfirmResult{Kind: contract.EmptyOrder}, nil, nil); !errors.Is(err, domain.ErrLeaseLost) {
		t.Fatalf("stale write error = %v, want ErrLeaseLost", err)
	}
	if err := s.RememberWarehouseResult(application.WithLease(ctxBG(), a), "command-1", contract.CommitResult{Kind: contract.OrderCommitted, Order: &contract.Order{OrderID: "o", ProposalID: "proposal-1"}}, nil); !errors.Is(err, domain.ErrLeaseLost) {
		t.Fatalf("stale remember error = %v, want ErrLeaseLost", err)
	}
	proposal, _ := getProposal(t, s, "proposal-1")
	staleAttempt := contract.ConfirmationAttempt{CommandID: "command-2", ProposalID: "proposal-1", Fingerprint: "fp-command-2", AcceptedPositions: accepted(proposal)}
	if _, created, err := s.BeginAttempt(application.WithLease(ctxBG(), a), staleAttempt); !errors.Is(err, domain.ErrLeaseLost) || created {
		t.Fatalf("stale BeginAttempt = created %v, error %v; want ErrLeaseLost without creation", created, err)
	}
	if _, found, err := s.GetAttempt(ctxBG(), "command-2"); err != nil || found {
		t.Fatalf("stale attempt persisted = found %v, error %v; want no attempt", found, err)
	}
	if _, created, err := s.BeginAttempt(application.WithLease(ctxBG(), b), staleAttempt); err != nil || !created {
		t.Fatalf("current BeginAttempt = created %v, error %v; want successful creation", created, err)
	}
	if err := s.CompleteAttempt(application.WithLease(ctxBG(), b), "command-1", contract.ConfirmResult{Kind: contract.EmptyOrder}, nil, nil); err != nil {
		t.Fatalf("fresh write refused: %v", err)
	}
	// A stale release does not free the new holder's lease.
	if err := s.ReleaseLease(ctxBG(), a); err != nil {
		t.Fatal(err)
	}
	if _, ok := acquire(t, s, "proposal-1", "holder-c", 30*time.Second); ok {
		t.Fatal("stale release freed the lease of the current holder")
	}
	if err := s.ReleaseLease(ctxBG(), b); err != nil {
		t.Fatal(err)
	}
	if c, ok, err := s.AcquireLease(ctxBG(), "proposal-1", "holder-c", 30*time.Second); err != nil || !ok || c.Seq <= b.Seq {
		t.Fatalf("lease after release = %+v %v %v", c, ok, err)
	}
	if _, _, err := s.AcquireLease(ctxBG(), "proposal-1", "", time.Second); err == nil {
		t.Fatal("lease without a holder was accepted")
	}
}

func leaseMissingProposal(t *testing.T, f Factory) {
	s := f(t, Options{}).Store
	lease, ok, err := s.AcquireLease(ctxBG(), "never-existed", "holder", time.Second)
	if err != nil || !ok || lease.Seq != 0 {
		t.Fatalf("lease of a missing proposal = %+v %v %v", lease, ok, err)
	}
	if err := s.ReleaseLease(ctxBG(), lease); err != nil {
		t.Fatal(err)
	}
}

func activeLimit(t *testing.T, f Factory) {
	fx := f(t, Options{MaxActiveProposals: 3})
	s := fx.Store
	for i := range 3 {
		p := proposalOf(fmt.Sprintf("proposal-%d", i))
		p.EvaluatedAt = time.Now()
		mustCreate(t, s, p)
	}
	p := proposalOf("proposal-over")
	p.EvaluatedAt = time.Now()
	if err := s.Create(ctxBG(), p); !errors.Is(err, contract.ErrLimitExceeded) {
		t.Fatalf("fourth proposal error = %v, want ErrLimitExceeded", err)
	}
	if _, ok := getProposal(t, s, "proposal-over"); ok {
		t.Fatal("proposal over the limit was stored")
	}
	// A settled proposal no longer counts.
	reservePending(t, s, "proposal-0", "command-0")
	order := contract.Order{OrderID: "o", ProposalID: "proposal-0"}
	if err := s.CompleteAttempt(ctxBG(), "command-0", contract.ConfirmResult{Kind: contract.OrderConfirmed, Order: &order}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctxBG(), p); err != nil {
		t.Fatalf("Create after settling: %v", err)
	}
	// The limit is per restaurant: the other restaurant is not affected.
	for i := range 3 {
		q := proposalOf(fmt.Sprintf("proposal-%d", i))
		q.EvaluatedAt = time.Now()
		mustCreate(t, fx.Other, q)
	}
}

func isolation(t *testing.T, f Factory) {
	fx := f(t, Options{})
	mustCreate(t, fx.Store, proposalOf("same-id"))
	if _, ok := getProposal(t, fx.Other, "same-id"); ok {
		t.Fatal("restaurant B sees a proposal of restaurant A")
	}
	mustCreate(t, fx.Other, proposalOf("same-id"))
	if _, _, err := fx.Store.BeginAttempt(ctxBG(), fullAttempt("command-1", "same-id")); err != nil {
		t.Fatal(err)
	}
	if _, found := getAttempt(t, fx.Other, "command-1"); found {
		t.Fatal("restaurant B sees an attempt of restaurant A")
	}
	a, ok := acquire(t, fx.Store, "same-id", "h", time.Minute)
	b, okB := acquire(t, fx.Other, "same-id", "h2", time.Minute)
	if !ok || !okB || a.Seq == 0 || b.Seq == 0 {
		t.Fatalf("leases of different restaurants collide: %+v %v / %+v %v", a, ok, b, okB)
	}
}

func checkBookkeeping(t *testing.T, f Factory) {
	fx := f(t, Options{})
	s := fx.Store
	mustCreate(t, s, proposalOf("proposal-1"))
	reservePending(t, s, "proposal-1", "command-1")
	if err := s.RecordCheck(ctxBG(), "command-1", 2*time.Second, false); err != nil {
		t.Fatal(err)
	}
	a, _ := getAttempt(t, s, "command-1")
	if a.CheckCount != 1 || a.NextCheckAt.IsZero() || !a.StuckAt.IsZero() {
		t.Fatalf("after one check: %+v", a)
	}
	if err := s.RecordCheck(ctxBG(), "command-1", time.Second, true); err != nil {
		t.Fatal(err)
	}
	b, _ := getAttempt(t, s, "command-1")
	if b.CheckCount != 2 || b.StuckAt.IsZero() {
		t.Fatalf("after stuck: %+v", b)
	}
	fx.Advance(time.Hour)
	if err := s.RecordCheck(ctxBG(), "command-1", time.Second, true); err != nil {
		t.Fatal(err)
	}
	c, _ := getAttempt(t, s, "command-1")
	if !c.StuckAt.Equal(b.StuckAt) {
		t.Fatal("stuck_at moved on a later check")
	}
	if err := s.MarkUnresolved(ctxBG(), "command-1"); err != nil {
		t.Fatal(err)
	}
	d, _ := getAttempt(t, s, "command-1")
	if d.UnresolvedAt.IsZero() || d.State != contract.OrderAttemptPending || !d.Pending {
		t.Fatalf("unresolved attempt: %+v", d)
	}
	if err := s.RecordCheck(ctxBG(), "absent", time.Second, false); !errors.Is(err, domain.ErrProposalMissing) {
		t.Fatalf("check of an unknown command error = %v", err)
	}
}

func getProposal(t *testing.T, s Store, id string) (domain.ProposalRecord, bool) {
	t.Helper()
	record, ok, err := s.Get(ctxBG(), id)
	if err != nil {
		t.Fatalf("Get(%q): %v", id, err)
	}
	return record, ok
}

func getAttempt(t *testing.T, s Store, id string) (contract.ConfirmationAttempt, bool) {
	t.Helper()
	attempt, ok, err := s.GetAttempt(ctxBG(), id)
	if err != nil {
		t.Fatalf("GetAttempt(%q): %v", id, err)
	}
	return attempt, ok
}

func acquire(t *testing.T, s Store, proposalID, holder string, ttl time.Duration) (application.Lease, bool) {
	t.Helper()
	lease, ok, err := s.AcquireLease(ctxBG(), proposalID, holder, ttl)
	if err != nil {
		t.Fatalf("AcquireLease(%q): %v", proposalID, err)
	}
	return lease, ok
}

func proposalAt(id string, at time.Time) domain.OrderProposal {
	p := proposalOf(id)
	p.EvaluatedAt = at
	return p
}

func terminal(t *testing.T, s Store, proposalID, commandID string) {
	t.Helper()
	reservePending(t, s, proposalID, commandID)
	if err := s.CompleteAttempt(ctxBG(), commandID, contract.ConfirmResult{Kind: contract.EmptyOrder}, nil, nil); err != nil {
		t.Fatal(err)
	}
}

func sweepAll(t *testing.T, fx Fixture) application.SweepResult {
	t.Helper()
	var total application.SweepResult
	for range 20 {
		r, err := fx.Sweep(ctxBG(), 1000)
		if err != nil {
			t.Fatalf("Sweep: %v", err)
		}
		total.Proposals += r.Proposals
		total.Attempts += r.Attempts
		total.ClosedRestaurant += r.ClosedRestaurant
		if r.Proposals+r.Attempts+r.ClosedRestaurant == 0 {
			break
		}
	}
	return total
}

func retention(t *testing.T, f Factory) {
	fx := f(t, Options{})
	s := fx.Store
	now := fx.Now()
	mustCreate(t, s, proposalAt("lonely", now))       // no attempt: 24 h
	mustCreate(t, s, proposalAt("with-pending", now)) // pending attempt: never
	mustCreate(t, s, proposalAt("with-done", now))    // terminal attempt: 30 days
	mustCreate(t, s, proposalAt("with-unsent", now))  // attempt without a sent commit: 24 h
	reservePending(t, s, "with-pending", "cmd-pending")
	terminal(t, s, "with-done", "cmd-done")
	if _, _, err := s.BeginAttempt(ctxBG(), fullAttempt("cmd-unsent", "with-unsent")); err != nil {
		t.Fatal(err)
	}
	fx.Advance(23 * time.Hour)
	sweepAll(t, fx)
	if _, ok := getProposal(t, s, "lonely"); !ok {
		t.Fatal("a proposal was deleted before 24 hours")
	}
	fx.Advance(2 * time.Hour) // 25 h
	sweepAll(t, fx)
	if _, ok := getProposal(t, s, "lonely"); ok {
		t.Fatal("a proposal without an attempt survived 24 hours")
	}
	if _, ok := getAttempt(t, s, "cmd-unsent"); ok {
		t.Fatal("an attempt without a sent commit survived 24 hours")
	}
	for _, id := range []string{"with-pending", "with-done", "with-unsent"} {
		if _, ok := getProposal(t, s, id); !ok && id != "with-unsent" {
			t.Fatalf("proposal %q with an attempt was deleted early", id)
		}
	}
	if _, ok := getAttempt(t, s, "cmd-pending"); !ok {
		t.Fatal("a pending attempt was deleted")
	}
	if _, ok := getAttempt(t, s, "cmd-done"); !ok {
		t.Fatal("a terminal attempt was deleted before 30 days")
	}
	fx.Advance(28 * 24 * time.Hour) // 29 days
	sweepAll(t, fx)
	if _, ok := getAttempt(t, s, "cmd-done"); !ok {
		t.Fatal("a terminal attempt was deleted before 30 days")
	}
	fx.Advance(2 * 24 * time.Hour) // 31 days
	sweepAll(t, fx)
	if _, ok := getAttempt(t, s, "cmd-done"); ok {
		t.Fatal("a terminal attempt survived 30 days")
	}
	if _, ok := getProposal(t, s, "with-done"); ok {
		t.Fatal("the proposal of a deleted terminal attempt survived")
	}
	// A pending attempt, even a very old one, is never deleted (C25).
	fx.Advance(400 * 24 * time.Hour)
	sweepAll(t, fx)
	if a, ok := getAttempt(t, s, "cmd-pending"); !ok || !a.Pending {
		t.Fatal("a pending attempt did not survive a year")
	}
	if _, ok := getProposal(t, s, "with-pending"); !ok {
		t.Fatal("the proposal of a pending attempt was deleted")
	}
}

func retentionLimit(t *testing.T, f Factory) {
	fx := f(t, Options{})
	now := fx.Now()
	for i := range 5 {
		mustCreate(t, fx.Store, proposalAt(fmt.Sprintf("old-%d", i), now))
	}
	fx.Advance(25 * time.Hour)
	r, err := fx.Sweep(ctxBG(), 2)
	if err != nil || r.Proposals != 2 {
		t.Fatalf("Sweep(limit 2) = %+v, %v; want 2 proposals", r, err)
	}
	if r := sweepAll(t, fx); r.Proposals != 3 {
		t.Fatalf("remaining sweep = %+v, want 3 proposals", r)
	}
}

func pickDue(t *testing.T, f Factory) {
	fx := f(t, Options{})
	s := fx.Store
	now := fx.Now()
	for _, id := range []string{"p-due", "p-unreserved", "p-done", "p-unresolved"} {
		mustCreate(t, s, proposalAt(id, now))
	}
	mustCreate(t, fx.Other, proposalAt("p-other", now))
	reservePending(t, s, "p-due", "cmd-due")
	reservePending(t, fx.Other, "p-other", "cmd-other")
	reservePending(t, s, "p-unresolved", "cmd-unresolved")
	if err := s.MarkUnresolved(ctxBG(), "cmd-unresolved"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.BeginAttempt(ctxBG(), fullAttempt("cmd-unreserved", "p-unreserved")); err != nil {
		t.Fatal(err)
	}
	terminal(t, s, "p-done", "cmd-done")
	due, err := fx.PickDue(ctxBG(), 10, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]application.DueAttempt{}
	for _, d := range due {
		got[d.CommandID] = d
	}
	if len(got) != 2 || got["cmd-due"].RestaurantID != fx.RestaurantID || got["cmd-due"].OrgID != fx.OrgID || got["cmd-other"].CommandID == "" || got["cmd-other"].RestaurantID == fx.RestaurantID {
		t.Fatalf("picked %+v, want cmd-due and cmd-other with their own restaurants", due)
	}
	again, err := fx.PickDue(ctxBG(), 10, 5*time.Second)
	if err != nil || len(again) != 0 {
		t.Fatalf("second pick = %+v, %v; the shift must hide picked rows", again, err)
	}
	fx.Advance(6 * time.Second)
	later, err := fx.PickDue(ctxBG(), 10, 5*time.Second)
	if err != nil || len(later) != 2 {
		t.Fatalf("pick after the shift = %+v, %v; want both again", later, err)
	}
	// A backoff set by RecordCheck hides the attempt until it is due.
	if err := s.RecordCheck(ctxBG(), "cmd-due", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	fx.Advance(10 * time.Second)
	partial, err := fx.PickDue(ctxBG(), 10, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range partial {
		if d.CommandID == "cmd-due" {
			t.Fatal("an attempt was picked before its backoff ended")
		}
	}
	if _, err := fx.PickDue(ctxBG(), 0, time.Second); err == nil {
		t.Fatal("limit 0 was accepted")
	}
}

func parallelPickers(t *testing.T, f Factory) {
	fx := f(t, Options{})
	now := fx.Now()
	const n = 30
	for i := range n {
		mustCreate(t, fx.Store, proposalAt(fmt.Sprintf("p-%d", i), now))
		reservePending(t, fx.Store, fmt.Sprintf("p-%d", i), fmt.Sprintf("c-%d", i))
	}
	var mu sync.Mutex
	seen := map[string]int{}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			due, err := fx.PickDue(ctxBG(), 10, time.Minute)
			if err != nil {
				t.Errorf("PickDue: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, d := range due {
				seen[d.CommandID]++
			}
		})
	}
	wg.Wait()
	for id, count := range seen {
		if count != 1 {
			t.Fatalf("attempt %s was picked %d times", id, count)
		}
	}
}
