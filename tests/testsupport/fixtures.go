package testsupport

import (
	"time"

	"github.com/JustIkra/umir-serving/internal/domain"
)

func Proposal(proposalID string) domain.OrderProposal {
	return domain.OrderProposal{
		ProposalID:   proposalID,
		EvaluatedAt:  time.Date(2026, time.October, 10, 12, 0, 0, 0, Moscow),
		StateVersion: 7,
		RequestedPositions: []domain.Position{{
			LineNo:   3,
			DishID:   "dish-1",
			Portions: 2,
		}},
		AcceptedPositions: []domain.AcceptedPosition{{
			LineNo:   3,
			DishID:   "dish-1",
			Portions: 2,
		}},
	}
}
