package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
	log           *slog.Logger
	leaseHolder   string
	leaseTTL      time.Duration
	leaseWait     time.Duration
	maxPositions  int
}

func NewOrdersService(store ProposalStore, warehouse contract.OrdersWarehouse, now func() time.Time, newID func() string, readers ...contract.OrderAttemptReader) (*OrdersService, error) {
	return NewOrdersServiceWithOptions(store, warehouse, now, newID, OrdersServiceOptions{}, readers...)
}

type OrdersServiceOptions struct {
	LeaseHolder  string
	LeaseTTL     time.Duration
	LeaseWait    time.Duration
	MaxPositions int
}

func NewOrdersServiceWithOptions(store ProposalStore, warehouse contract.OrdersWarehouse, now func() time.Time, newID func() string, options OrdersServiceOptions, readers ...contract.OrderAttemptReader) (*OrdersService, error) {
	attempts, ok := store.(ConfirmationAttemptStore)
	if store == nil || !ok || warehouse == nil || now == nil || newID == nil {
		return nil, contract.ErrInvalidArgument
	}
	if options.LeaseHolder == "" {
		options.LeaseHolder = "serving"
	}
	if options.LeaseTTL == 0 {
		options.LeaseTTL = 30 * time.Second
	}
	if options.LeaseWait == 0 {
		options.LeaseWait = 1500 * time.Millisecond
	}
	if options.MaxPositions == 0 {
		options.MaxPositions = contract.MaxOrderPositions
	}
	if options.LeaseTTL < time.Second || options.LeaseWait < 0 || options.MaxPositions < 1 {
		return nil, contract.ErrInvalidArgument
	}
	service := &OrdersService{
		store:        store,
		attempts:     attempts,
		warehouse:    warehouse,
		now:          now,
		newID:        newID,
		log:          slog.New(slog.DiscardHandler),
		leaseHolder:  options.LeaseHolder,
		leaseTTL:     options.LeaseTTL,
		leaseWait:    options.LeaseWait,
		maxPositions: options.MaxPositions,
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
	if len(request.Positions) > s.maxPositions {
		return contract.OrderProposal{}, contract.ErrInvalidArgument
	}
	positions := make([]contract.Position, len(request.Positions))
	for i, position := range request.Positions {
		if position.DishID == "" || position.Portions <= 0 {
			return contract.OrderProposal{}, contract.ErrInvalidArgument
		}
		positions[i] = contract.Position{LineNo: i + 1, DishID: position.DishID, Portions: position.Portions}
	}
	proposalID := s.newID()
	if !contract.ValidClientID(proposalID) {
		return contract.OrderProposal{}, contract.ErrInvalidArgument
	}
	ctx, capture := captureWarehouse(ctx)
	evaluation, err := s.warehouse.EvaluateOrder(ctx, contract.EvaluateOrderRequest{Positions: positions})
	if err != nil {
		return contract.OrderProposal{}, err
	}
	proposal := domain.OrderProposal{
		Context:            domainContext(capture.value),
		ProposalID:         proposalID,
		EvaluatedAt:        evaluation.EvaluatedAt,
		StateVersion:       evaluation.StateVersion,
		RequestedPositions: toDomainPositions(positions),
		AcceptedPositions:  toDomainAcceptedPositions(evaluation.AcceptedPositions),
		Changes:            toDomainChanges(evaluation.Changes),
	}
	if err := s.store.Create(ctx, proposal); err != nil {
		if errors.Is(err, contract.ErrStorageUnavailable) {
			return contract.OrderProposal{}, storageUnavailable(err, "not_committed")
		}
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
		Context: contractContext(proposal.Context), WarehouseEpoch: proposal.Context.Epoch,
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

type capturedWarehouse struct {
	parent context.Context
	value  contract.WarehouseContext
}

func (c *capturedWarehouse) Record(value contract.WarehouseContext) {
	c.value = value
	contract.RecordWarehouseContext(c.parent, value)
}
func (c *capturedWarehouse) RecordPreparation(value contract.WarehousePreparation) {
	contract.RecordWarehousePreparation(c.parent, value)
}
func captureWarehouse(ctx context.Context) (context.Context, *capturedWarehouse) {
	capture := &capturedWarehouse{parent: ctx}
	return contract.WithWarehouseObserver(ctx, capture), capture
}
func domainContext(value contract.WarehouseContext) domain.ProposalContext {
	return domain.ProposalContext{OrgID: value.OrgID, TenantID: value.TenantID, Epoch: value.Epoch, TimeZone: value.TimeZone, SettingsVersion: value.SettingsVersion, EvaluatedAt: value.EvaluatedAt, StateVersion: value.StateVersion}
}
func contractContext(value domain.ProposalContext) contract.WarehouseContext {
	return contract.WarehouseContext{OrgID: value.OrgID, TenantID: value.TenantID, Epoch: value.Epoch, TimeZone: value.TimeZone, SettingsVersion: value.SettingsVersion, EvaluatedAt: value.EvaluatedAt, StateVersion: value.StateVersion}
}
func attemptContext(ctx context.Context, proposal domain.OrderProposal) contract.WarehouseContext {
	value := contractContext(proposal.Context)
	if scope, ok := contract.RequestScopeFromContext(ctx); ok && value.Epoch != "" {
		value.TimeZone = scope.TimeZone
		value.SettingsVersion = scope.SettingsVersion
	}
	return value
}
