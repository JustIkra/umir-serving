package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/JustIkra/umir-serving/contract"
	"github.com/JustIkra/umir-serving/internal/domain"
)

type OrdersService struct {
	store     ProposalStore
	warehouse contract.OrdersWarehouse
	now       func() time.Time
	newID     func() string
	locksMu   sync.Mutex
	locks     map[string]*proposalLock
}

type proposalLock struct {
	mu   sync.Mutex
	refs int
}

func NewOrdersService(store ProposalStore, warehouse contract.OrdersWarehouse, now func() time.Time, newID func() string) (*OrdersService, error) {
	if store == nil || warehouse == nil || now == nil || newID == nil {
		return nil, contract.ErrInvalidArgument
	}
	return &OrdersService{
		store:     store,
		warehouse: warehouse,
		now:       now,
		newID:     newID,
		locks:     make(map[string]*proposalLock),
	}, nil
}

func (s *OrdersService) PrepareOrder(ctx context.Context, request contract.PrepareOrderRequest) (contract.OrderProposal, error) {
	positions := make([]contract.Position, len(request.Positions))
	for i, position := range request.Positions {
		if position.DishID == "" || position.Portions <= 0 {
			return contract.OrderProposal{}, contract.ErrInvalidArgument
		}
		positions[i] = contract.Position{LineNo: i + 1, DishID: position.DishID, Portions: position.Portions}
	}
	evaluation, err := s.warehouse.EvaluateOrder(ctx, contract.EvaluateOrderRequest{Positions: positions})
	if err != nil {
		return contract.OrderProposal{}, err
	}
	proposalID := s.newID()
	if proposalID == "" {
		return contract.OrderProposal{}, contract.ErrInvalidArgument
	}
	proposal := domain.OrderProposal{
		ProposalID:         proposalID,
		EvaluatedAt:        evaluation.EvaluatedAt,
		StateVersion:       evaluation.StateVersion,
		RequestedPositions: toDomainPositions(positions),
		AcceptedPositions:  toDomainAcceptedPositions(evaluation.AcceptedPositions),
		Changes:            toDomainChanges(evaluation.Changes),
	}
	if err := s.store.Create(ctx, proposal); err != nil {
		return contract.OrderProposal{}, err
	}
	return toContractProposal(proposal), nil
}

func (s *OrdersService) ConfirmOrder(ctx context.Context, request contract.ConfirmOrderRequest) (contract.ConfirmResult, error) {
	if request.CommandID == "" || request.ProposalID == "" {
		return contract.ConfirmResult{}, contract.ErrInvalidArgument
	}
	unlock := s.lockProposal(request.ProposalID)
	defer unlock()
	if err := ctx.Err(); err != nil {
		return contract.ConfirmResult{}, err
	}
	fingerprint, err := confirmOrderFingerprint(request.ProposalID)
	if err != nil {
		return contract.ConfirmResult{}, err
	}
	recorded, found, err := s.warehouse.FindRecordedCommand(ctx, request.CommandID)
	if err != nil {
		return contract.ConfirmResult{}, err
	}
	if found {
		order, err := recordedOrder(recorded, request.CommandID, request.ProposalID, fingerprint)
		if err != nil {
			return contract.ConfirmResult{}, err
		}
		return confirmed(order), nil
	}
	order, found, err := s.warehouse.FindOrderByProposal(ctx, request.ProposalID)
	if err != nil {
		return contract.ConfirmResult{}, err
	}
	if found {
		return confirmed(order), nil
	}

	record, found, err := s.store.Get(ctx, request.ProposalID)
	if err != nil {
		return contract.ConfirmResult{}, err
	}
	if !found {
		return contract.ConfirmResult{}, contract.ErrProposalNotFound
	}
	if record.SupersededBy != "" {
		return contract.ConfirmResult{Kind: contract.ProposalSuperseded, SupersededBy: record.SupersededBy}, nil
	}
	if record.PendingCommandID != "" && record.PendingCommandID != request.CommandID {
		return contract.ConfirmResult{Kind: contract.ConfirmationPending}, nil
	}
	if len(record.Proposal.AcceptedPositions) == 0 {
		proposal := toContractProposal(record.Proposal)
		return contract.ConfirmResult{Kind: contract.EmptyOrder, Proposal: &proposal}, nil
	}
	if err := s.store.MarkPending(ctx, request.ProposalID, request.CommandID); err != nil {
		return contract.ConfirmResult{}, err
	}
	commit := contract.CommitOrderRequest{
		CommandID:         request.CommandID,
		ProposalID:        request.ProposalID,
		AcceptedPositions: toContractAcceptedPositions(record.Proposal.AcceptedPositions),
	}
	result, err := s.warehouse.CommitAcceptedOrder(ctx, commit)
	if err != nil {
		if errors.Is(err, contract.ErrInventoryUnavailable) {
			if resolveErr := s.store.ResolvePending(context.WithoutCancel(ctx), request.ProposalID, request.CommandID); resolveErr != nil {
				return contract.ConfirmResult{}, errors.Join(err, fmt.Errorf("resolve pending confirmation: %w", resolveErr))
			}
		}
		return contract.ConfirmResult{}, err
	}
	switch result.Kind {
	case contract.OrderCommitted:
		if result.Order == nil || result.Order.ProposalID != request.ProposalID || result.Order.OrderID == "" {
			return contract.ConfirmResult{}, errors.New("invalid committed order result")
		}
		if err := s.store.ResolvePending(context.WithoutCancel(ctx), request.ProposalID, request.CommandID); err != nil {
			return contract.ConfirmResult{}, err
		}
		return confirmed(*result.Order), nil
	case contract.RecheckRequired:
		if result.OrderEvaluation == nil {
			return contract.ConfirmResult{}, errors.New("missing recheck evaluation")
		}
		replacementID := s.newID()
		if replacementID == "" {
			resolveErr := s.store.ResolvePending(context.WithoutCancel(ctx), request.ProposalID, request.CommandID)
			return contract.ConfirmResult{}, errors.Join(contract.ErrInvalidArgument, resolveErr)
		}
		evaluation := result.OrderEvaluation
		replacement := domain.OrderProposal{
			ProposalID:         replacementID,
			EvaluatedAt:        evaluation.EvaluatedAt,
			StateVersion:       evaluation.StateVersion,
			RequestedPositions: requestedFromAccepted(record.Proposal.AcceptedPositions),
			AcceptedPositions:  toDomainAcceptedPositions(evaluation.AcceptedPositions),
			Changes:            toDomainChanges(evaluation.Changes),
		}
		saved, err := s.store.Replace(context.WithoutCancel(ctx), request.ProposalID, request.CommandID, replacement)
		if err != nil {
			resolveErr := s.store.ResolvePending(context.WithoutCancel(ctx), request.ProposalID, request.CommandID)
			return contract.ConfirmResult{}, errors.Join(err, resolveErr)
		}
		proposal := toContractProposal(saved)
		return contract.ConfirmResult{Kind: contract.RequiresConfirmation, Proposal: &proposal}, nil
	default:
		return contract.ConfirmResult{}, fmt.Errorf("unsupported commit result kind %q", result.Kind)
	}
}

func (s *OrdersService) lockProposal(proposalID string) func() {
	s.locksMu.Lock()
	lock := s.locks[proposalID]
	if lock == nil {
		lock = &proposalLock{}
		s.locks[proposalID] = lock
	}
	lock.refs++
	s.locksMu.Unlock()
	lock.mu.Lock()
	return func() {
		lock.mu.Unlock()
		s.locksMu.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(s.locks, proposalID)
		}
		s.locksMu.Unlock()
	}
}

func confirmOrderFingerprint(proposalID string) (string, error) {
	input, err := json.Marshal(struct {
		ProposalID string `json:"proposalID"`
	}{ProposalID: proposalID})
	if err != nil {
		return "", err
	}
	payload := append([]byte("ConfirmOrder\x00"), input...)
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

func recordedOrder(record contract.CommandRecord, commandID, proposalID, fingerprint string) (contract.Order, error) {
	if record.CommandID != commandID || record.CommandType != "ConfirmOrder" || record.CommandFingerprint != fingerprint {
		return contract.Order{}, contract.ErrCommandIdentityConflict
	}
	if record.Result.Kind != contract.OrderPlaced {
		return contract.Order{}, fmt.Errorf("unsupported recorded command result kind %q", record.Result.Kind)
	}
	if record.Result.Order == nil || record.Result.Order.OrderID == "" || record.Result.Order.ProposalID != proposalID {
		return contract.Order{}, errors.New("invalid recorded order result")
	}
	return cloneContractOrder(*record.Result.Order), nil
}

func confirmed(order contract.Order) contract.ConfirmResult {
	order = cloneContractOrder(order)
	return contract.ConfirmResult{Kind: contract.OrderConfirmed, Order: &order}
}

func toDomainPositions(positions []contract.Position) []domain.Position {
	result := make([]domain.Position, len(positions))
	for i, position := range positions {
		result[i] = domain.Position{LineNo: position.LineNo, DishID: position.DishID, Portions: position.Portions}
	}
	return result
}

func toDomainAcceptedPositions(positions []contract.AcceptedPosition) []domain.AcceptedPosition {
	result := make([]domain.AcceptedPosition, len(positions))
	for i, position := range positions {
		result[i] = domain.AcceptedPosition{LineNo: position.LineNo, DishID: position.DishID, Portions: position.Portions}
	}
	return result
}

func toContractAcceptedPositions(positions []domain.AcceptedPosition) []contract.AcceptedPosition {
	result := make([]contract.AcceptedPosition, len(positions))
	for i, position := range positions {
		result[i] = contract.AcceptedPosition{LineNo: position.LineNo, DishID: position.DishID, Portions: position.Portions}
	}
	return result
}

func toDomainChanges(changes []contract.Change) []domain.Change {
	result := make([]domain.Change, len(changes))
	for i, change := range changes {
		deficits := make([]domain.Deficit, len(change.Deficits))
		for j, deficit := range change.Deficits {
			deficits[j] = domain.Deficit{ProductID: deficit.ProductID, RequiredQuantity: deficit.RequiredQuantity, AvailableQuantity: deficit.AvailableQuantity}
		}
		result[i] = domain.Change{LineNo: change.LineNo, DishID: change.DishID, RequestedPortions: change.RequestedPortions, AcceptedPortions: change.AcceptedPortions, Deficits: deficits}
	}
	return result
}

func toContractProposal(proposal domain.OrderProposal) contract.OrderProposal {
	proposal = proposal.Clone()
	result := contract.OrderProposal{
		ProposalID:         proposal.ProposalID,
		EvaluatedAt:        proposal.EvaluatedAt,
		StateVersion:       proposal.StateVersion,
		RequestedPositions: make([]contract.Position, len(proposal.RequestedPositions)),
		AcceptedPositions:  toContractAcceptedPositions(proposal.AcceptedPositions),
		Changes:            make([]contract.Change, len(proposal.Changes)),
	}
	for i, position := range proposal.RequestedPositions {
		result.RequestedPositions[i] = contract.Position{LineNo: position.LineNo, DishID: position.DishID, Portions: position.Portions}
	}
	for i, change := range proposal.Changes {
		deficits := make([]contract.Deficit, len(change.Deficits))
		for j, deficit := range change.Deficits {
			deficits[j] = contract.Deficit{ProductID: deficit.ProductID, RequiredQuantity: deficit.RequiredQuantity, AvailableQuantity: deficit.AvailableQuantity}
		}
		result.Changes[i] = contract.Change{LineNo: change.LineNo, DishID: change.DishID, RequestedPortions: change.RequestedPortions, AcceptedPortions: change.AcceptedPortions, Deficits: deficits}
	}
	return result
}

func requestedFromAccepted(positions []domain.AcceptedPosition) []domain.Position {
	requested := make([]domain.Position, len(positions))
	for i, position := range positions {
		requested[i] = domain.Position{LineNo: position.LineNo, DishID: position.DishID, Portions: position.Portions}
	}
	return requested
}

func cloneContractOrder(order contract.Order) contract.Order {
	order.Positions = append([]contract.AcceptedPosition(nil), order.Positions...)
	order.Allocations = append([]contract.Allocation(nil), order.Allocations...)
	return order
}
