package domain_test

import (
	"testing"

	"github.com/JustIkra/umir-serving/internal/domain"
)

func TestOrderProposalCloneCopiesNestedSlices(t *testing.T) {
	proposal := domain.OrderProposal{
		RequestedPositions: []domain.Position{{LineNo: 2, DishID: "dish-1", Portions: 3}},
		AcceptedPositions:  []domain.AcceptedPosition{{LineNo: 2, DishID: "dish-1", Portions: 1}},
		Changes:            []domain.Change{{LineNo: 2, DishID: "dish-1", Deficits: []domain.Deficit{{ProductID: "product-1", RequiredQuantity: 3000, AvailableQuantity: 1000}}}},
	}
	clone := proposal.Clone()
	clone.RequestedPositions[0].DishID = "changed"
	clone.AcceptedPositions[0].Portions = 8
	clone.Changes[0].Deficits[0].AvailableQuantity = 0
	if proposal.RequestedPositions[0].DishID != "dish-1" || proposal.AcceptedPositions[0].Portions != 1 || proposal.Changes[0].Deficits[0].AvailableQuantity != 1000 {
		t.Fatalf("Clone() shared mutable slices with proposal: %+v", proposal)
	}
}
