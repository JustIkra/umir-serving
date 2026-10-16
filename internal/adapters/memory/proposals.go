package memory

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/JustIkra/umir-serving/contract"
	"github.com/JustIkra/umir-serving/internal/application"
	"github.com/JustIkra/umir-serving/internal/domain"
)

var (
	ErrInvalidProposal         = domain.ErrInvalidProposal
	ErrProposalExists          = domain.ErrProposalExists
	ErrProposalMissing         = domain.ErrProposalMissing
	ErrPendingMismatch         = domain.ErrPendingMismatch
	ErrProposalReplaced        = domain.ErrProposalReplaced
	ErrInvalidReplace          = domain.ErrInvalidReplace
	ErrCommandIdentityConflict = contract.ErrCommandIdentityConflict
)

// activeProposalWindow is how long an unsettled proposal counts as active.
const activeProposalWindow = 24 * time.Hour

// ProposalStore is the in-process store of one restaurant. It is the test and
// fixture counterpart of the PostgreSQL store and follows the same contract.
type ProposalStore struct {
	mu        sync.RWMutex
	now       func() time.Time
	maxActive int
	proposals map[string]*proposalEntry
	attempts  map[string]contract.ConfirmationAttempt
}

type proposalEntry struct {
	record     domain.ProposalRecord
	settled    bool
	leaseHold  string
	leaseUntil time.Time
	leaseSeq   int64
}

// Option configures a ProposalStore.
type Option func(*ProposalStore)

// WithClock sets the time source of leases and bookkeeping.
func WithClock(now func() time.Time) Option { return func(s *ProposalStore) { s.now = now } }

// WithMaxActiveProposals limits the unsettled proposals younger than 24 hours;
// zero means no limit.
func WithMaxActiveProposals(limit int) Option { return func(s *ProposalStore) { s.maxActive = limit } }

func NewProposalStore(options ...Option) *ProposalStore {
	s := &ProposalStore{
		now:       time.Now,
		proposals: make(map[string]*proposalEntry),
		attempts:  make(map[string]contract.ConfirmationAttempt),
	}
	for _, option := range options {
		option(s)
	}
	return s
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
	entry, found := s.proposals[attempt.ProposalID]
	if !found {
		if attempt.Pending || len(attempt.AcceptedPositions) > 0 {
			return contract.ConfirmationAttempt{}, false, ErrProposalMissing
		}
	} else {
		if entry.record.SupersededBy != "" && attempt.Pending {
			return contract.ConfirmationAttempt{}, false, ErrProposalReplaced
		}
		if attempt.Pending && entry.record.PendingCommandID != "" && entry.record.PendingCommandID != attempt.CommandID {
			return contract.ConfirmationAttempt{}, false, contract.ErrConfirmationPending
		}
	}
	now := s.now()
	attempt.State = contract.OrderAttemptPending
	attempt.CreatedAt, attempt.UpdatedAt = now, now
	attempt.CheckCount, attempt.NextCheckAt, attempt.StuckAt, attempt.UnresolvedAt = 0, time.Time{}, time.Time{}, time.Time{}
	if attempt.Pending {
		attempt.NextCheckAt = now
	}
	attempt = attempt.Clone()
	s.attempts[attempt.CommandID] = attempt
	if attempt.Pending {
		entry.record.PendingCommandID = attempt.CommandID
	}
	return attempt.Clone(), true, nil
}

func (s *ProposalStore) GetAttempt(ctx context.Context, commandID string) (contract.ConfirmationAttempt, bool, error) {
	if err := ctx.Err(); err != nil {
		return contract.ConfirmationAttempt{}, false, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	attempt, found := s.attempts[commandID]
	return attempt.Clone(), found, nil
}

// fenced refuses a write whose lease token was overtaken by another holder.
func (s *ProposalStore) fenced(ctx context.Context, proposalID string) error {
	lease, ok := application.LeaseFromContext(ctx)
	if !ok || lease.ProposalID != proposalID {
		return nil
	}
	if entry, found := s.proposals[proposalID]; found && entry.leaseSeq != lease.Seq {
		return domain.ErrLeaseLost
	}
	return nil
}

func (s *ProposalStore) ReserveAttempt(ctx context.Context, commandID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	attempt, found := s.attempts[commandID]
	if !found || attempt.State != contract.OrderAttemptPending {
		return ErrProposalMissing
	}
	entry, found := s.proposals[attempt.ProposalID]
	if !found {
		return ErrProposalMissing
	}
	if err := s.fenced(ctx, attempt.ProposalID); err != nil {
		return err
	}
	if entry.record.SupersededBy != "" {
		return ErrProposalReplaced
	}
	if entry.record.PendingCommandID != "" && entry.record.PendingCommandID != commandID {
		return contract.ErrConfirmationPending
	}
	entry.record.PendingCommandID = commandID
	if !attempt.Pending {
		attempt.Pending = true
		attempt.NextCheckAt = s.now()
		attempt.UpdatedAt = attempt.NextCheckAt
	}
	s.attempts[commandID] = attempt
	return nil
}

func (s *ProposalStore) RememberWarehouseResult(ctx context.Context, commandID string, result contract.CommitResult, replacement *contract.OrderProposal) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	attempt, found := s.attempts[commandID]
	if !found {
		return ErrProposalMissing
	}
	if err := s.fenced(ctx, attempt.ProposalID); err != nil {
		return err
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
	// The replacement is written once and never overwritten.
	if replacement != nil && attempt.Replacement == nil {
		clonedReplacement := replacement.Clone()
		attempt.Replacement = &clonedReplacement
	}
	attempt.UpdatedAt = s.now()
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
	if err := s.fenced(ctx, attempt.ProposalID); err != nil {
		return err
	}
	entry, found := s.proposals[attempt.ProposalID]
	if !found && (attempt.Pending || replacement != nil) {
		return ErrProposalMissing
	}
	var newEntry *proposalEntry
	if attempt.Pending {
		if entry.record.PendingCommandID != commandID {
			return ErrPendingMismatch
		}
		if replacement != nil {
			if replacement.ProposalID == "" || replacement.ProposalID == attempt.ProposalID || !domain.AcceptedWithin(replacement.AcceptedPositions, entry.record.Proposal.AcceptedPositions) {
				return ErrInvalidReplace
			}
			if _, exists := s.proposals[replacement.ProposalID]; exists {
				return ErrProposalExists
			}
			cloned := replacement.Clone()
			cloned.RequestedPositions = domain.RequestedFromAccepted(entry.record.Proposal.AcceptedPositions)
			newEntry = &proposalEntry{record: domain.ProposalRecord{Proposal: cloned}}
		}
	}
	// All checks passed: from here the change is applied as a whole.
	if attempt.Pending {
		entry.record.PendingCommandID = ""
		if newEntry != nil {
			entry.record.SupersededBy = newEntry.record.Proposal.ProposalID
			entry.settled = true
			s.proposals[newEntry.record.Proposal.ProposalID] = newEntry
		}
	}
	if entry != nil && rejection == nil && result.Kind == contract.OrderConfirmed {
		entry.settled = true
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
	attempt.NextCheckAt = time.Time{}
	attempt.UpdatedAt = s.now()
	s.attempts[commandID] = attempt
	return nil
}

func (s *ProposalStore) AcquireLease(ctx context.Context, proposalID, holder string, ttl time.Duration) (application.Lease, bool, error) {
	if err := ctx.Err(); err != nil {
		return application.Lease{}, false, err
	}
	if proposalID == "" || holder == "" || ttl <= 0 {
		return application.Lease{}, false, ErrInvalidProposal
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, found := s.proposals[proposalID]
	if !found {
		return application.Lease{ProposalID: proposalID, Holder: holder}, true, nil
	}
	now := s.now()
	if entry.leaseHold != "" && entry.leaseUntil.After(now) {
		return application.Lease{}, false, nil
	}
	entry.leaseSeq++
	entry.leaseHold, entry.leaseUntil = holder, now.Add(ttl)
	return application.Lease{ProposalID: proposalID, Holder: holder, Seq: entry.leaseSeq}, true, nil
}

func (s *ProposalStore) ReleaseLease(_ context.Context, lease application.Lease) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry, found := s.proposals[lease.ProposalID]; found && entry.leaseSeq == lease.Seq && entry.leaseHold == lease.Holder {
		entry.leaseHold, entry.leaseUntil = "", time.Time{}
	}
	return nil
}

func (s *ProposalStore) RecordCheck(ctx context.Context, commandID string, delay time.Duration, stuck bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	attempt, found := s.attempts[commandID]
	if !found {
		return ErrProposalMissing
	}
	now := s.now()
	attempt.CheckCount++
	attempt.NextCheckAt = now.Add(delay)
	if stuck && attempt.StuckAt.IsZero() {
		attempt.StuckAt = now
	}
	attempt.UpdatedAt = now
	s.attempts[commandID] = attempt
	return nil
}

func (s *ProposalStore) MarkUnresolved(ctx context.Context, commandID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	attempt, found := s.attempts[commandID]
	if !found {
		return ErrProposalMissing
	}
	if attempt.UnresolvedAt.IsZero() {
		attempt.UnresolvedAt = s.now()
		attempt.UpdatedAt = attempt.UnresolvedAt
		s.attempts[commandID] = attempt
	}
	return nil
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
	if _, exists := s.proposals[proposal.ProposalID]; exists {
		return fmt.Errorf("%w: %s", ErrProposalExists, proposal.ProposalID)
	}
	if s.maxActive > 0 {
		cutoff, active := s.now().Add(-activeProposalWindow), 0
		for _, entry := range s.proposals {
			if !entry.settled && entry.record.Proposal.EvaluatedAt.After(cutoff) {
				active++
			}
		}
		if active >= s.maxActive {
			return contract.ErrLimitExceeded
		}
	}
	s.proposals[proposal.ProposalID] = &proposalEntry{record: domain.ProposalRecord{Proposal: proposal.Clone()}}
	return nil
}

func (s *ProposalStore) Get(ctx context.Context, proposalID string) (domain.ProposalRecord, bool, error) {
	if err := ctx.Err(); err != nil {
		return domain.ProposalRecord{}, false, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.proposals[proposalID]
	if !ok {
		return domain.ProposalRecord{}, false, nil
	}
	record := entry.record
	record.Proposal = record.Proposal.Clone()
	return record, true, nil
}
