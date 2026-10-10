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
	store         ProposalStore
	attempts      ConfirmationAttemptStore
	warehouse     contract.OrdersWarehouse
	attemptReader contract.OrderAttemptReader
	now           func() time.Time
	newID         func() string
	locksMu       sync.Mutex
	locks         map[string]*proposalLock
}

type proposalLock struct {
	mu   sync.Mutex
	refs int
}

func NewOrdersService(store ProposalStore, warehouse contract.OrdersWarehouse, now func() time.Time, newID func() string, readers ...contract.OrderAttemptReader) (*OrdersService, error) {
	attempts, ok := store.(ConfirmationAttemptStore)
	if store == nil || !ok || warehouse == nil || now == nil || newID == nil {
		return nil, contract.ErrInvalidArgument
	}
	service := &OrdersService{
		store:     store,
		attempts:  attempts,
		warehouse: warehouse,
		now:       now,
		newID:     newID,
		locks:     make(map[string]*proposalLock),
	}
	if len(readers) > 1 {
		return nil, contract.ErrInvalidArgument
	}
	if len(readers) == 1 {
		service.attemptReader = readers[0]
	}
	return service, nil
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
	return s.confirmWithAttempts(ctx, request)
}

func definiteCommitRejection(err error) bool {
	var operationError *contract.OperationError
	if errors.As(err, &operationError) {
		return operationError.Outcome == "not_committed"
	}
	return errors.Is(err, contract.ErrInventoryUnavailable) ||
		errors.Is(err, contract.ErrInvalidArgument) ||
		errors.Is(err, contract.ErrCommandIdentityConflict)
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

func cloneContractOrder(order contract.Order) contract.Order {
	order.Positions = append([]contract.AcceptedPosition(nil), order.Positions...)
	order.Allocations = append([]contract.Allocation(nil), order.Allocations...)
	return order
}
