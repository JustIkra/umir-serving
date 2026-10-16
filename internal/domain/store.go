package domain

import "errors"

// Errors every proposal and attempt store reports in the same way, so the
// application and the contract tests do not depend on which store is in use.
var (
	ErrInvalidProposal  = errors.New("invalid proposal")
	ErrProposalExists   = errors.New("proposal already exists")
	ErrProposalMissing  = errors.New("proposal not found")
	ErrPendingMismatch  = errors.New("pending command identity mismatch")
	ErrProposalReplaced = errors.New("proposal already replaced")
	ErrInvalidReplace   = errors.New("invalid proposal replacement")
	// ErrLeaseLost: the proposal lease token no longer belongs to the caller
	// (another holder took the lease over), so the write is refused.
	ErrLeaseLost = errors.New("proposal lease lost")
)

// AcceptedWithin reports whether next is a valid reduction of previous: every
// line of next exists in previous with the same dish and no more portions.
func AcceptedWithin(next, previous []AcceptedPosition) bool {
	limits := make(map[int]AcceptedPosition, len(previous))
	for _, position := range previous {
		if position.Portions <= 0 || position.DishID == "" {
			return false
		}
		if _, exists := limits[position.LineNo]; exists {
			return false
		}
		limits[position.LineNo] = position
	}
	seen := make(map[int]struct{}, len(next))
	for _, position := range next {
		limit, ok := limits[position.LineNo]
		if !ok || position.DishID != limit.DishID || position.Portions <= 0 || position.Portions > limit.Portions {
			return false
		}
		if _, exists := seen[position.LineNo]; exists {
			return false
		}
		seen[position.LineNo] = struct{}{}
	}
	return true
}

// RequestedFromAccepted turns accepted positions into the requested positions
// of a replacement proposal.
func RequestedFromAccepted(positions []AcceptedPosition) []Position {
	requested := make([]Position, len(positions))
	for i, position := range positions {
		requested[i] = Position(position)
	}
	return requested
}
