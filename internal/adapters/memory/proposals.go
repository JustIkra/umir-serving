package memory

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"

	"github.com/JustIkra/umir-serving/contract"
	"github.com/JustIkra/umir-serving/internal/domain"
)

var (
	ErrInvalidProposal         = errors.New("invalid proposal")
	ErrProposalExists          = errors.New("proposal already exists")
	ErrProposalMissing         = errors.New("proposal not found")
	ErrPendingMismatch         = errors.New("pending command identity mismatch")
	ErrProposalReplaced        = errors.New("proposal already replaced")
	ErrInvalidReplace          = errors.New("invalid proposal replacement")
	ErrCommandIdentityConflict = contract.ErrCommandIdentityConflict
)

type ProposalStore struct {
	mu        sync.RWMutex
	proposals map[string]domain.ProposalRecord
	attempts  map[string]contract.ConfirmationAttempt
}

func NewProposalStore() *ProposalStore {
	return &ProposalStore{
		proposals: make(map[string]domain.ProposalRecord),
		attempts:  make(map[string]contract.ConfirmationAttempt),
	}
}

func (s *ProposalStore) BeginAttempt(ctx context.Context, attempt contract.ConfirmationAttempt) (contract.ConfirmationAttempt, bool, error) {
	if err := ctx.Err(); err != nil {
		return contract.ConfirmationAttempt{}, false, err
	}
	if attempt.CommandID == "" || attempt.ProposalID == "" || attempt.Fingerprint == "" {
		return contract.ConfirmationAttempt{}, false, ErrInvalidProposal
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return contract.ConfirmationAttempt{}, false, err
	}
	if existing, found := s.attempts[attempt.CommandID]; found {
		if existing.ProposalID != attempt.ProposalID || existing.Fingerprint != attempt.Fingerprint || !sameAcceptedPositions(existing.AcceptedPositions, attempt.AcceptedPositions) {
			return existing.Clone(), false, contract.ErrCommandIdentityConflict
		}
		return existing.Clone(), false, nil
	}
	proposal, found := s.proposals[attempt.ProposalID]
	if !found {
		if attempt.Pending || len(attempt.AcceptedPositions) > 0 {
			return contract.ConfirmationAttempt{}, false, ErrProposalMissing
		}
	} else {
		if proposal.SupersededBy != "" && attempt.Pending {
			return contract.ConfirmationAttempt{}, false, ErrProposalReplaced
		}
		if attempt.Pending && proposal.PendingCommandID != "" && proposal.PendingCommandID != attempt.CommandID {
			return contract.ConfirmationAttempt{}, false, contract.ErrConfirmationPending
		}
	}
	attempt.State = contract.OrderAttemptPending
	attempt = attempt.Clone()
	s.attempts[attempt.CommandID] = attempt
	if attempt.Pending {
		proposal.PendingCommandID = attempt.CommandID
		s.proposals[attempt.ProposalID] = proposal
	}
	return attempt.Clone(), true, nil
}

func (s *ProposalStore) GetAttempt(ctx context.Context, commandID string) (contract.ConfirmationAttempt, bool, error) {
	if err := ctx.Err(); err != nil {
		return contract.ConfirmationAttempt{}, false, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return contract.ConfirmationAttempt{}, false, err
	}
	attempt, found := s.attempts[commandID]
	return attempt.Clone(), found, nil
}

func (s *ProposalStore) ReserveAttempt(ctx context.Context, commandID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	attempt, found := s.attempts[commandID]
	if !found || attempt.State != contract.OrderAttemptPending {
		return ErrProposalMissing
	}
	proposal, found := s.proposals[attempt.ProposalID]
	if !found {
		return ErrProposalMissing
	}
	if proposal.SupersededBy != "" {
		return ErrProposalReplaced
	}
	if proposal.PendingCommandID != "" && proposal.PendingCommandID != commandID {
		return contract.ErrConfirmationPending
	}
	proposal.PendingCommandID = commandID
	attempt.Pending = true
	s.proposals[attempt.ProposalID] = proposal
	s.attempts[commandID] = attempt
	return nil
}

func (s *ProposalStore) RememberWarehouseResult(ctx context.Context, commandID string, result contract.CommitResult, replacement *contract.OrderProposal) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	attempt, found := s.attempts[commandID]
	if !found {
		return ErrProposalMissing
	}
	if attempt.State != contract.OrderAttemptPending {
		if attempt.WarehouseResult != nil && sameCommitResult(*attempt.WarehouseResult, result) {
			return nil
		}
		return ErrCommandIdentityConflict
	}
	if attempt.WarehouseResult != nil && !sameCommitResult(*attempt.WarehouseResult, result) {
		return ErrCommandIdentityConflict
	}
	clonedResult := result.Clone()
	attempt.WarehouseResult = &clonedResult
	if replacement != nil {
		clonedReplacement := replacement.Clone()
		attempt.Replacement = &clonedReplacement
	}
	s.attempts[commandID] = attempt
	return nil
}

func sameCommitResult(left, right contract.CommitResult) bool {
	return reflect.DeepEqual(left.Clone(), right.Clone())
}

func (s *ProposalStore) CompleteAttempt(ctx context.Context, commandID string, result contract.ConfirmResult, rejection *contract.AttemptError, replacement *domain.OrderProposal) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	attempt, found := s.attempts[commandID]
	if !found {
		return ErrProposalMissing
	}
	wantState := contract.OrderAttemptCompleted
	if rejection != nil {
		wantState = contract.OrderAttemptRejected
	}
	if attempt.State != contract.OrderAttemptPending {
		if attempt.State == wantState && reflect.DeepEqual(attempt.Result, &result) && reflect.DeepEqual(attempt.Error, rejection) {
			return nil
		}
		return ErrCommandIdentityConflict
	}
	proposal, found := s.proposals[attempt.ProposalID]
	if !found && (attempt.Pending || replacement != nil) {
		return ErrProposalMissing
	}
	if attempt.Pending {
		if proposal.PendingCommandID != commandID {
			return ErrPendingMismatch
		}
		if replacement != nil {
			if replacement.ProposalID == "" || replacement.ProposalID == attempt.ProposalID || !acceptedPositionsWithin(replacement.AcceptedPositions, proposal.Proposal.AcceptedPositions) {
				return ErrInvalidReplace
			}
			if _, exists := s.proposals[replacement.ProposalID]; exists {
				return ErrProposalExists
			}
			replacement = cloneProposalPointer(replacement)
			replacement.RequestedPositions = requestedFromAccepted(proposal.Proposal.AcceptedPositions)
			proposal.PendingCommandID = ""
			proposal.SupersededBy = replacement.ProposalID
			s.proposals[replacement.ProposalID] = domain.ProposalRecord{Proposal: *replacement}
		} else {
			proposal.PendingCommandID = ""
		}
		s.proposals[attempt.ProposalID] = proposal
	}
	attempt.State = wantState
	if rejection == nil {
		clonedResult := result.Clone()
		attempt.Result = &clonedResult
	} else {
		clonedError := *rejection
		attempt.Error = &clonedError
	}
	attempt.WarehouseResult = nil
	attempt.Replacement = nil
	attempt.Pending = false
	s.attempts[commandID] = attempt
	return nil
}

func cloneProposalPointer(proposal *domain.OrderProposal) *domain.OrderProposal {
	cloned := proposal.Clone()
	return &cloned
}

func sameAcceptedPositions(left, right []contract.AcceptedPosition) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
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
		requested[i] = domain.Position(position)
	}
	return requested
}
