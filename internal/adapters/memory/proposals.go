package memory

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/JustIkra/umir-serving/internal/domain"
)

var (
	ErrInvalidProposal  = errors.New("invalid proposal")
	ErrProposalExists   = errors.New("proposal already exists")
	ErrProposalMissing  = errors.New("proposal not found")
	ErrPendingMismatch  = errors.New("pending command identity mismatch")
	ErrProposalReplaced = errors.New("proposal already replaced")
	ErrInvalidReplace   = errors.New("invalid proposal replacement")
)

type ProposalStore struct {
	mu        sync.RWMutex
	proposals map[string]domain.ProposalRecord
}

func NewProposalStore() *ProposalStore {
	return &ProposalStore{
		proposals: make(map[string]domain.ProposalRecord),
	}
}

func (s *ProposalStore) Create(ctx context.Context, proposal domain.OrderProposal) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if proposal.ProposalID == "" {
		return ErrInvalidProposal
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, exists := s.proposals[proposal.ProposalID]; exists {
		return fmt.Errorf("%w: %s", ErrProposalExists, proposal.ProposalID)
	}
	s.proposals[proposal.ProposalID] = domain.ProposalRecord{Proposal: proposal.Clone()}
	return nil
}

func (s *ProposalStore) Get(ctx context.Context, proposalID string) (domain.ProposalRecord, bool, error) {
	if err := ctx.Err(); err != nil {
		return domain.ProposalRecord{}, false, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return domain.ProposalRecord{}, false, err
	}
	record, ok := s.proposals[proposalID]
	if !ok {
		return domain.ProposalRecord{}, false, nil
	}
	record.Proposal = record.Proposal.Clone()
	return record, true, nil
}

func (s *ProposalStore) MarkPending(ctx context.Context, proposalID, commandID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if commandID == "" {
		return ErrInvalidProposal
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	record, ok := s.proposals[proposalID]
	if !ok {
		return ErrProposalMissing
	}
	if record.SupersededBy != "" {
		return ErrProposalReplaced
	}
	if record.PendingCommandID != "" && record.PendingCommandID != commandID {
		return ErrPendingMismatch
	}
	record.PendingCommandID = commandID
	s.proposals[proposalID] = record
	return nil
}

func (s *ProposalStore) ResolvePending(ctx context.Context, proposalID, commandID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	record, ok := s.proposals[proposalID]
	if !ok {
		return ErrProposalMissing
	}
	if record.PendingCommandID != commandID || commandID == "" {
		return ErrPendingMismatch
	}
	record.PendingCommandID = ""
	s.proposals[proposalID] = record
	return nil
}

func (s *ProposalStore) Replace(ctx context.Context, oldID, commandID string, replacement domain.OrderProposal) (domain.OrderProposal, error) {
	if err := ctx.Err(); err != nil {
		return domain.OrderProposal{}, err
	}
	if commandID == "" || replacement.ProposalID == "" || oldID == replacement.ProposalID {
		return domain.OrderProposal{}, ErrInvalidReplace
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return domain.OrderProposal{}, err
	}
	old, ok := s.proposals[oldID]
	if !ok {
		return domain.OrderProposal{}, ErrProposalMissing
	}
	if old.SupersededBy != "" {
		current, exists := s.proposals[old.SupersededBy]
		if !exists {
			return domain.OrderProposal{}, ErrInvalidReplace
		}
		return current.Proposal.Clone(), nil
	}
	if old.PendingCommandID != commandID {
		return domain.OrderProposal{}, ErrPendingMismatch
	}
	if previous, exists := s.proposals[replacement.ProposalID]; exists {
		return domain.OrderProposal{}, fmt.Errorf("%w: %s", ErrProposalExists, previous.Proposal.ProposalID)
	}
	if !acceptedPositionsWithin(replacement.AcceptedPositions, old.Proposal.AcceptedPositions) {
		return domain.OrderProposal{}, ErrInvalidReplace
	}
	replacement = replacement.Clone()
	replacement.RequestedPositions = requestedFromAccepted(old.Proposal.AcceptedPositions)
	old.PendingCommandID = ""
	old.SupersededBy = replacement.ProposalID
	s.proposals[oldID] = old
	s.proposals[replacement.ProposalID] = domain.ProposalRecord{Proposal: replacement}
	return replacement.Clone(), nil
}

func acceptedPositionsWithin(next, previous []domain.AcceptedPosition) bool {
	limits := make(map[int]domain.AcceptedPosition, len(previous))
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

func requestedFromAccepted(positions []domain.AcceptedPosition) []domain.Position {
	requested := make([]domain.Position, len(positions))
	for i, position := range positions {
		requested[i] = domain.Position{LineNo: position.LineNo, DishID: position.DishID, Portions: position.Portions}
	}
	return requested
}
