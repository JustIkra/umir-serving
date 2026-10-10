package testsupport_test

import (
	"testing"
	"time"

	"github.com/JustIkra/umir-serving/tests/testsupport"
)

func TestClockAdvancesInMoscowTime(t *testing.T) {
	clock := testsupport.NewClock(time.Date(2026, time.October, 10, 12, 0, 0, 0, time.UTC))
	before := clock.Now()
	after := clock.Advance(30 * time.Minute)
	zone, offset := before.Zone()
	if before.Hour() != 15 || zone != "MSK" || offset != 3*60*60 {
		t.Fatalf("clock did not normalize to Moscow time: %v", before)
	}
	if !after.Equal(before.Add(30 * time.Minute)) {
		t.Fatalf("Advance() = %v, want %v", after, before.Add(30*time.Minute))
	}
}

func TestIDsAreUniqueAndSequential(t *testing.T) {
	ids := testsupport.NewIDs("proposal-")
	if first, second := ids.Next(), ids.Next(); first != "proposal-1" || second != "proposal-2" {
		t.Fatalf("generated IDs = %q, %q", first, second)
	}
}

func TestProposalFixtureContainsAcceptedPositions(t *testing.T) {
	proposal := testsupport.Proposal("proposal-1")
	if proposal.ProposalID != "proposal-1" || len(proposal.RequestedPositions) != 1 || len(proposal.AcceptedPositions) != 1 || proposal.RequestedPositions[0].LineNo != 3 || proposal.AcceptedPositions[0].Portions != 2 {
		t.Fatalf("proposal fixture is incomplete: %+v", proposal)
	}
}
