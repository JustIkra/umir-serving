package application

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/JustIkra/umir-serving/contract"
	"github.com/JustIkra/umir-serving/internal/domain"
)

const (
	localCompletionBudget = 2 * time.Second
	retryCommitBudget     = 10 * time.Second
)

func (s *OrdersService) confirmWithAttempts(ctx context.Context, request contract.ConfirmOrderRequest) (contract.ConfirmResult, error) {
	trace := newConfirmTrace("confirm", request)
	result, err := s.confirmWithAttemptsTraced(ctx, request, trace)
	s.logConfirmOutcome(ctx, trace, result, err)
	return result, err
}

func (s *OrdersService) confirmWithAttemptsTraced(ctx context.Context, request contract.ConfirmOrderRequest, trace *confirmTrace) (contract.ConfirmResult, error) {
	if request.CommandID == "" || request.ProposalID == "" {
		return contract.ConfirmResult{}, contract.ErrInvalidArgument
	}
	unlock, err := s.lockProposal(ctx, request.ProposalID)
	if err != nil {
		return contract.ConfirmResult{}, err
	}
	defer unlock()
	if err := ctx.Err(); err != nil {
		return contract.ConfirmResult{}, err
	}
	fingerprint, err := confirmOrderFingerprint(request.ProposalID)
	if err != nil {
		return contract.ConfirmResult{}, err
	}

	attempt, found, err := s.attempts.GetAttempt(ctx, request.CommandID)
	if err != nil {
		return contract.ConfirmResult{}, err
	}
	if found {
		trace.observeAttempt(attempt)
		if err := requireSavedEpoch(ctx, attempt.Context); err != nil {
			return contract.ConfirmResult{}, err
		}
		if attempt.Context.Epoch != "" {
			ctx = contract.WithWarehouseBinding(ctx, attempt.Context, true)
		}
		if attempt.ProposalID != request.ProposalID || attempt.Fingerprint != fingerprint {
			return contract.ConfirmResult{}, contract.ErrCommandIdentityConflict
		}
		trace.markRetained(attempt)
		if result, terminalErr, terminal := terminalAttempt(attempt); terminal {
			contract.RecordWarehouseContext(ctx, result.Context)
			return result, terminalErr
		}
		if attempt.WarehouseResult != nil {
			return s.completeKnownWarehouseResult(ctx, attempt, *attempt.WarehouseResult)
		}
	}

	if found && attempt.Context.Epoch != "" {
		return s.resolveAttempt(ctx, request, attempt, true)
	}
	proposalForScope, scopeFound, scopeErr := s.store.Get(ctx, request.ProposalID)
	if _, scoped := contract.RequestScopeFromContext(ctx); scoped && (scopeErr != nil || !scopeFound) {
		if scopeErr != nil {
			return contract.ConfirmResult{}, unknownLookupError(scopeErr)
		}
		return contract.ConfirmResult{}, proposalNotFoundUnknown()
	}
	savedContext := contract.WarehouseContext{}
	if scopeFound {
		savedContext = attemptContext(ctx, proposalForScope.Proposal)
		trace.observeSavedContext(savedContext)
		if err := requireSavedEpoch(ctx, savedContext); err != nil {
			return contract.ConfirmResult{}, err
		}
		if savedContext.Epoch != "" {
			ctx = contract.WithWarehouseBinding(ctx, savedContext, false)
		}
	}
	ctx, capture := captureWarehouse(ctx)

	recorded, recordedFound, err := s.warehouse.FindRecordedCommand(ctx, request.CommandID)
	if err != nil {
		return contract.ConfirmResult{}, unknownLookupError(err)
	}
	if recordedFound {
		order, err := recordedOrder(recorded, request.CommandID, request.ProposalID, fingerprint)
		if err != nil {
			if found && attempt.State == contract.OrderAttemptPending && attempt.Pending && errors.Is(err, contract.ErrCommandIdentityConflict) {
				return contract.ConfirmResult{}, s.finishKnownError(ctx, attempt, err)
			}
			return contract.ConfirmResult{}, err
		}
		if !found {
			proposalRecord, proposalFound, lookupErr := s.store.Get(ctx, request.ProposalID)
			var accepted []contract.AcceptedPosition
			if lookupErr == nil && proposalFound {
				accepted = toContractAcceptedPositions(proposalRecord.Proposal.AcceptedPositions)
			}
			attemptPending := lookupErr == nil && proposalFound && proposalRecord.PendingCommandID == request.CommandID
			attempt, _, err = s.attempts.BeginAttempt(ctx, contract.ConfirmationAttempt{Context: savedContext, CommandID: request.CommandID, ProposalID: request.ProposalID, Fingerprint: fingerprint, AcceptedPositions: accepted, Pending: attemptPending})
			if err != nil {
				return contract.ConfirmResult{}, mapAttemptStoreError(err)
			}
		}
		return s.finishReadOrder(ctx, attempt, order, capture.value)
	}
	var order contract.Order
	var orderFound bool
	if savedContext.Epoch == "" {
		order, orderFound, err = s.warehouse.FindOrderByProposal(ctx, request.ProposalID)
	}
	if err != nil {
		return contract.ConfirmResult{}, unknownLookupError(err)
	}
	if orderFound {
		if !found {
			proposalRecord, proposalFound, lookupErr := s.store.Get(ctx, request.ProposalID)
			var accepted []contract.AcceptedPosition
			if lookupErr == nil && proposalFound {
				accepted = toContractAcceptedPositions(proposalRecord.Proposal.AcceptedPositions)
			}
			attemptPending := lookupErr == nil && proposalFound && proposalRecord.PendingCommandID == request.CommandID
			attempt, _, err = s.attempts.BeginAttempt(ctx, contract.ConfirmationAttempt{Context: savedContext, CommandID: request.CommandID, ProposalID: request.ProposalID, Fingerprint: fingerprint, AcceptedPositions: accepted, Pending: attemptPending})
			if err != nil {
				return contract.ConfirmResult{}, mapAttemptStoreError(err)
			}
		}
		return s.finishReadOrder(ctx, attempt, order, capture.value)
	}
	proposalRecord, proposalFound, err := s.store.Get(ctx, request.ProposalID)
	if err != nil {
		return contract.ConfirmResult{}, err
	}
	accepted := []contract.AcceptedPosition(nil)
	if proposalFound {
		accepted = toContractAcceptedPositions(proposalRecord.Proposal.AcceptedPositions)
	}
	if !found {
		attempt, _, err = s.attempts.BeginAttempt(ctx, contract.ConfirmationAttempt{Context: savedContext, CommandID: request.CommandID, ProposalID: request.ProposalID, Fingerprint: fingerprint, AcceptedPositions: accepted})
		if err != nil {
			return contract.ConfirmResult{}, mapAttemptStoreError(err)
		}
	}

	if !proposalFound {
		return contract.ConfirmResult{}, proposalNotFoundUnknown()
	}
	if proposalRecord.SupersededBy != "" {
		result := contract.ConfirmResult{Context: capture.value, Kind: contract.ProposalSuperseded, SupersededBy: proposalRecord.SupersededBy}
		if err := s.attempts.CompleteAttempt(ctx, request.CommandID, result, nil, nil); err != nil {
			return contract.ConfirmResult{}, err
		}
		return result, nil
	}
	if proposalRecord.PendingCommandID != "" && proposalRecord.PendingCommandID != request.CommandID {
		contract.RecordWarehouseContext(ctx, attempt.Context)
		return contract.ConfirmResult{Context: attempt.Context, Kind: contract.ConfirmationPending}, nil
	}
	if len(proposalRecord.Proposal.AcceptedPositions) == 0 {
		proposal := toContractProposal(proposalRecord.Proposal)
		result := contract.ConfirmResult{Context: capture.value, Kind: contract.EmptyOrder, Proposal: &proposal}
		if err := s.attempts.CompleteAttempt(ctx, request.CommandID, result, nil, nil); err != nil {
			return contract.ConfirmResult{}, err
		}
		return result, nil
	}

	if err := s.attempts.ReserveAttempt(ctx, request.CommandID); err != nil {
		if errors.Is(err, errPendingMismatch) {
			return contract.ConfirmResult{Context: attempt.Context, Kind: contract.ConfirmationPending}, nil
		}
		return contract.ConfirmResult{}, mapAttemptStoreError(err)
	}
	trace.reserved = true
	attempt, _, err = s.attempts.GetAttempt(ctx, request.CommandID)
	if err != nil {
		return contract.ConfirmResult{}, err
	}
	if s.attemptReader != nil {
		warehouseAttempt, lookupErr := s.attemptReader.FindOrderAttempt(ctx, request.CommandID, request.ProposalID)
		if lookupErr != nil {
			return contract.ConfirmResult{}, unknownLookupError(lookupErr)
		}
		switch warehouseAttempt.State {
		case contract.OrderAttemptPending:
			return contract.ConfirmResult{Context: attempt.Context, Kind: contract.ConfirmationPending}, nil
		case contract.OrderAttemptCompleted:
			if warehouseAttempt.Result == nil {
				return contract.ConfirmResult{}, contract.ErrInventoryUnavailable
			}
			warehouseAttempt.Result.Context = capture.value
			return s.completeKnownWarehouseResult(ctx, attempt, *warehouseAttempt.Result)
		case contract.OrderAttemptRejected:
			if warehouseAttempt.Error == nil {
				return contract.ConfirmResult{}, contract.ErrInventoryUnavailable
			}
			return contract.ConfirmResult{}, s.completeRejectedAttempt(ctx, attempt, *warehouseAttempt.Error)
		case contract.OrderAttemptUnseen:
		default:
			return contract.ConfirmResult{}, contract.ErrInventoryUnavailable
		}
	}
	return s.commitSavedAttempt(ctx, attempt, capture)
}

func (s *OrdersService) commitSavedAttempt(ctx context.Context, attempt contract.ConfirmationAttempt, capture *capturedWarehouse) (contract.ConfirmResult, error) {
	commit := contract.CommitOrderRequest{CommandID: attempt.CommandID, ProposalID: attempt.ProposalID, AcceptedPositions: append([]contract.AcceptedPosition(nil), attempt.AcceptedPositions...)}
	result, err := s.warehouse.CommitAcceptedOrder(ctx, commit)
	result.Context = capture.value
	if err != nil {
		return contract.ConfirmResult{}, s.finishKnownError(ctx, attempt, err)
	}
	if result.Kind == contract.CommitPending {
		return contract.ConfirmResult{Context: attempt.Context, Kind: contract.ConfirmationPending}, nil
	}
	return s.completeKnownWarehouseResult(ctx, attempt, result)
}

var errPendingMismatch = errors.New("pending command identity mismatch")

func (s *OrdersService) completeKnownWarehouseResult(ctx context.Context, attempt contract.ConfirmationAttempt, result contract.CommitResult) (contract.ConfirmResult, error) {
	completionContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), localCompletionBudget)
	defer cancel()
	var publicResult contract.ConfirmResult
	var replacement *domain.OrderProposal
	switch result.Kind {
	case contract.OrderCommitted:
		if result.Order == nil || result.Order.OrderID == "" || result.Order.ProposalID != attempt.ProposalID {
			return contract.ConfirmResult{}, unknownLookupError(errors.New("committed order identity is invalid"))
		}
		if !sameAcceptedPositions(result.Order.Positions, attempt.AcceptedPositions) {
			return contract.ConfirmResult{}, unknownLookupError(errors.New("committed order composition differs from attempted input"))
		}
		order := cloneContractOrder(*result.Order)
		publicResult = confirmed(order)
		publicResult.Context = result.Context
	case contract.RecheckRequired:
		if result.OrderEvaluation == nil {
			return contract.ConfirmResult{}, unknownLookupError(errors.New("warehouse recheck result has no evaluation"))
		}
		if err := s.attempts.RememberWarehouseResult(completionContext, attempt.CommandID, result, nil); err != nil {
			return contract.ConfirmResult{}, unknownCompletionError(err)
		}
		if attempt.Replacement != nil {
			saved := *attempt.Replacement
			_, exists, lookupErr := s.store.Get(completionContext, saved.ProposalID)
			if lookupErr != nil {
				return contract.ConfirmResult{}, unknownCompletionError(lookupErr)
			}
			if !exists {
				replacement = &domain.OrderProposal{Context: domainContext(saved.Context), ProposalID: saved.ProposalID, EvaluatedAt: saved.EvaluatedAt, StateVersion: saved.StateVersion, RequestedPositions: toDomainPositions(saved.RequestedPositions), AcceptedPositions: toDomainAcceptedPositions(saved.AcceptedPositions), Changes: toDomainChanges(saved.Changes)}
			}
		}
		if replacement == nil {
			replacementID := s.newID()
			if replacementID == "" {
				return contract.ConfirmResult{}, unknownCompletionError(errors.New("could not generate replacement proposal ID"))
			}
			evaluation := result.OrderEvaluation
			replacement = &domain.OrderProposal{Context: domainContext(result.Context), ProposalID: replacementID, EvaluatedAt: evaluation.EvaluatedAt, StateVersion: evaluation.StateVersion, RequestedPositions: domainRequestedFromContractAccepted(attempt.AcceptedPositions), AcceptedPositions: toDomainAcceptedPositions(evaluation.AcceptedPositions), Changes: toDomainChanges(evaluation.Changes)}
			saved := toContractProposal(*replacement)
			if err := s.attempts.RememberWarehouseResult(completionContext, attempt.CommandID, result, &saved); err != nil {
				return contract.ConfirmResult{}, unknownCompletionError(err)
			}
		}
		proposal := toContractProposal(*replacement)
		publicResult = contract.ConfirmResult{Context: result.Context, Kind: contract.RequiresConfirmation, Proposal: &proposal}
	default:
		return contract.ConfirmResult{}, unknownLookupError(fmt.Errorf("warehouse returned unsupported commit result kind %q", result.Kind))
	}
	if result.Kind != contract.RecheckRequired {
		if err := s.attempts.RememberWarehouseResult(completionContext, attempt.CommandID, result, nil); err != nil {
			return contract.ConfirmResult{}, unknownCompletionError(err)
		}
	}
	if err := s.attempts.CompleteAttempt(completionContext, attempt.CommandID, publicResult, nil, replacement); err != nil {
		return contract.ConfirmResult{}, unknownCompletionError(err)
	}
	contract.RecordWarehouseContext(ctx, publicResult.Context)
	return publicResult, nil
}

func (s *OrdersService) finishReadOrder(ctx context.Context, attempt contract.ConfirmationAttempt, order contract.Order, observation contract.WarehouseContext) (contract.ConfirmResult, error) {
	if order.OrderID == "" || order.ProposalID != attempt.ProposalID {
		return contract.ConfirmResult{}, unknownLookupError(errors.New("recorded order identity is invalid"))
	}
	if len(attempt.AcceptedPositions) > 0 && !sameAcceptedPositions(order.Positions, attempt.AcceptedPositions) {
		return contract.ConfirmResult{}, unknownLookupError(errors.New("recorded order composition differs from serving attempt"))
	}
	if attempt.State == contract.OrderAttemptPending && attempt.Pending {
		commit := contract.CommitResult{Context: observation, Kind: contract.OrderCommitted, Order: &order}
		return s.completeKnownWarehouseResult(ctx, attempt, commit)
	}
	result := confirmed(order)
	result.Context = observation
	if err := s.attempts.CompleteAttempt(ctx, attempt.CommandID, result, nil, nil); err != nil {
		return contract.ConfirmResult{}, unknownCompletionError(err)
	}
	return result, nil
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

func (s *OrdersService) finishKnownError(ctx context.Context, attempt contract.ConfirmationAttempt, err error) error {
	if !definiteCommitRejection(err) {
		return unknownLookupError(err)
	}
	value := attemptErrorFrom(err)
	completionContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), localCompletionBudget)
	defer cancel()
	if completeErr := s.attempts.CompleteAttempt(completionContext, attempt.CommandID, contract.ConfirmResult{}, &value, nil); completeErr != nil {
		return errors.Join(err, unknownCompletionError(completeErr))
	}
	return errorFromAttempt(value)
}

func (s *OrdersService) completeRejectedAttempt(ctx context.Context, attempt contract.ConfirmationAttempt, value contract.AttemptError) error {
	if value.Outcome != "not_committed" {
		return unknownCompletionError(errors.New("warehouse rejection outcome is not terminal"))
	}
	if value.Code == legacyCommandIdentityConflictCode {
		value.Code = "CommandConflict"
	}
	completionContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), localCompletionBudget)
	defer cancel()
	if err := s.attempts.CompleteAttempt(completionContext, attempt.CommandID, contract.ConfirmResult{}, &value, nil); err != nil {
		return unknownCompletionError(err)
	}
	return errorFromAttempt(value)
}

func (s *OrdersService) ResolveConfirmation(ctx context.Context, request contract.ConfirmOrderRequest) (contract.ConfirmResult, error) {
	trace := newConfirmTrace("resolve", request)
	result, err := s.resolveConfirmation(ctx, request, trace)
	s.logConfirmOutcome(ctx, trace, result, err)
	return result, err
}

func (s *OrdersService) resolveConfirmation(ctx context.Context, request contract.ConfirmOrderRequest, trace *confirmTrace) (contract.ConfirmResult, error) {
	if request.CommandID == "" || request.ProposalID == "" {
		return contract.ConfirmResult{}, contract.ErrInvalidArgument
	}
	if s.attempts == nil || s.attemptReader == nil {
		return contract.ConfirmResult{}, proposalNotFoundUnknown()
	}
	unlock, err := s.lockProposal(ctx, request.ProposalID)
	if err != nil {
		return contract.ConfirmResult{}, err
	}
	defer unlock()
	fingerprint, err := confirmOrderFingerprint(request.ProposalID)
	if err != nil {
		return contract.ConfirmResult{}, err
	}
	attempt, found, err := s.attempts.GetAttempt(ctx, request.CommandID)
	if err != nil {
		return contract.ConfirmResult{}, err
	}
	if !found {
		return contract.ConfirmResult{}, proposalNotFoundUnknown()
	}
	trace.observeAttempt(attempt)
	if attempt.ProposalID != request.ProposalID || attempt.Fingerprint != fingerprint {
		return contract.ConfirmResult{}, contract.ErrCommandIdentityConflict
	}
	trace.markRetained(attempt)
	return s.resolveAttempt(ctx, request, attempt, false)
}

func (s *OrdersService) resolveAttempt(ctx context.Context, request contract.ConfirmOrderRequest, attempt contract.ConfirmationAttempt, retry bool) (contract.ConfirmResult, error) {
	if err := requireSavedEpoch(ctx, attempt.Context); err != nil {
		return contract.ConfirmResult{}, err
	}
	if attempt.Context.Epoch != "" {
		ctx = contract.WithWarehouseBinding(ctx, attempt.Context, true)
	}
	if result, terminalErr, terminal := terminalAttempt(attempt); terminal {
		contract.RecordWarehouseContext(ctx, result.Context)
		return result, terminalErr
	}
	if attempt.WarehouseResult != nil {
		return s.completeKnownWarehouseResult(ctx, attempt, *attempt.WarehouseResult)
	}
	if s.attemptReader == nil {
		return contract.ConfirmResult{}, proposalNotFoundUnknown()
	}
	ctx, capture := captureWarehouse(ctx)
	warehouseAttempt, err := s.attemptReader.FindOrderAttempt(ctx, request.CommandID, request.ProposalID)
	if err != nil {
		return contract.ConfirmResult{}, unknownLookupError(err)
	}
	if attempt.Context.Epoch != "" && capture.value.Epoch != "" && capture.value.Epoch != attempt.Context.Epoch {
		return contract.ConfirmResult{}, &contract.OperationError{Code: "RuntimeReset", Outcome: "indeterminate"}
	}
	// Redelivery needs provenance from the owner attempt read itself; the
	// /commands lookup below may record its own observation.
	attemptObservation := capture.value
	switch warehouseAttempt.State {
	case contract.OrderAttemptUnseen:
		if attempt.State == contract.OrderAttemptPending && attempt.Pending {
			if result, handled, err := s.finishFromRecordedCommand(ctx, attempt, capture); handled {
				return result, err
			}
			if retry && len(attempt.AcceptedPositions) > 0 && retryScopeMatches(ctx, attempt.Context, attemptObservation) {
				ctx = contract.WithWarehouseBinding(ctx, attempt.Context, false)
				bounded, cancel := context.WithTimeout(ctx, retryCommitBudget)
				defer cancel()
				return s.commitSavedAttempt(bounded, attempt, capture)
			}
			return contract.ConfirmResult{Context: attempt.Context, Kind: contract.ConfirmationPending}, nil
		}
		return contract.ConfirmResult{}, proposalNotFoundUnknown()
	case contract.OrderAttemptPending:
		if result, handled, err := s.finishFromRecordedCommand(ctx, attempt, capture); handled {
			return result, err
		}
		return contract.ConfirmResult{Context: attempt.Context, Kind: contract.ConfirmationPending}, nil
	case contract.OrderAttemptCompleted:
		if warehouseAttempt.Result == nil || warehouseAttempt.Result.Kind == contract.CommitPending {
			return contract.ConfirmResult{}, unknownLookupError(errors.New("completed warehouse attempt has no result"))
		}
		warehouseAttempt.Result.Context = capture.value
		return s.completeKnownWarehouseResult(ctx, attempt, *warehouseAttempt.Result)
	case contract.OrderAttemptRejected:
		if warehouseAttempt.Error == nil || warehouseAttempt.Error.Outcome != "not_committed" {
			return contract.ConfirmResult{}, unknownLookupError(errors.New("rejected warehouse attempt is malformed"))
		}
		return contract.ConfirmResult{}, s.completeRejectedAttempt(ctx, attempt, *warehouseAttempt.Error)
	default:
		return contract.ConfirmResult{}, unknownLookupError(fmt.Errorf("unknown order attempt state %q", warehouseAttempt.State))
	}
}

// finishFromRecordedCommand lets a retained attempt whose owner attempt is
// Pending or Unseen look at /commands before it reports ConfirmationPending: the
// owner may have recorded the order and lost the reply. handled is false only
// when /commands has no record, so the caller keeps its own answer. It never
// commits. A lookup failure is an unknown outcome and leaves the attempt as is.
func (s *OrdersService) finishFromRecordedCommand(ctx context.Context, attempt contract.ConfirmationAttempt, capture *capturedWarehouse) (contract.ConfirmResult, bool, error) {
	recorded, found, err := s.warehouse.FindRecordedCommand(ctx, attempt.CommandID)
	if err != nil {
		return contract.ConfirmResult{}, true, unknownLookupError(err)
	}
	if attempt.Context.Epoch != "" && capture.value.Epoch != "" && capture.value.Epoch != attempt.Context.Epoch {
		return contract.ConfirmResult{}, true, &contract.OperationError{Code: "RuntimeReset", Outcome: "indeterminate"}
	}
	if !found {
		return contract.ConfirmResult{}, false, nil
	}
	order, err := recordedOrder(recorded, attempt.CommandID, attempt.ProposalID, attempt.Fingerprint)
	if err != nil {
		if errors.Is(err, contract.ErrCommandIdentityConflict) {
			return contract.ConfirmResult{}, true, s.finishKnownError(ctx, attempt, err)
		}
		return contract.ConfirmResult{}, true, unknownLookupError(err)
	}
	result, err := s.finishReadOrder(ctx, attempt, order, capture.value)
	return result, true, err
}

func retryScopeMatches(ctx context.Context, saved, observed contract.WarehouseContext) bool {
	scope, ok := contract.RequestScopeFromContext(ctx)
	return ok && saved.Epoch != "" && observed.Epoch == saved.Epoch &&
		observed.OrgID == saved.OrgID && observed.TenantID == saved.TenantID &&
		observed.TimeZone == saved.TimeZone && observed.SettingsVersion == saved.SettingsVersion &&
		scope.OrgID == saved.OrgID && scope.TenantID == saved.TenantID &&
		scope.TimeZone == saved.TimeZone && scope.SettingsVersion == saved.SettingsVersion
}

func terminalAttempt(attempt contract.ConfirmationAttempt) (contract.ConfirmResult, error, bool) {
	switch attempt.State {
	case contract.OrderAttemptCompleted:
		if attempt.Result != nil {
			return attempt.Result.Clone(), nil, true
		}
	case contract.OrderAttemptRejected:
		if attempt.Error != nil {
			return contract.ConfirmResult{}, errorFromAttempt(*attempt.Error), true
		}
	}
	return contract.ConfirmResult{}, nil, false
}

// legacyCommandIdentityConflictCode was written to stored attempts before the
// public v2 code CommandConflict was adopted. It is only read, never written.
const legacyCommandIdentityConflictCode = "CommandIdentityConflict"

func attemptErrorFrom(err error) contract.AttemptError {
	var remote *contract.OperationError
	if errors.As(err, &remote) && remote.Outcome == "not_committed" {
		code := remote.Code
		if code == legacyCommandIdentityConflictCode {
			code = "CommandConflict"
		}
		return contract.AttemptError{Code: code, Message: remote.Message, Outcome: remote.Outcome}
	}
	code := "InventoryUnavailable"
	switch {
	case errors.Is(err, contract.ErrInvalidArgument):
		code = "InvalidArgument"
	case errors.Is(err, contract.ErrCommandIdentityConflict):
		code = "CommandConflict"
	case errors.Is(err, contract.ErrProductNotFound):
		code = "ProductNotFound"
	case errors.Is(err, contract.ErrDishNotFound):
		code = "DishNotFound"
	case errors.Is(err, contract.ErrProposalSuperseded):
		code = "ProposalSuperseded"
	}
	return contract.AttemptError{Code: code, Message: publicErrorMessage(err), Outcome: "not_committed"}
}

func errorFromAttempt(value contract.AttemptError) error {
	var cause error
	switch value.Code {
	case "InvalidArgument":
		cause = contract.ErrInvalidArgument
	case "InventoryUnavailable":
		cause = contract.ErrInventoryUnavailable
	case "CommandConflict", legacyCommandIdentityConflictCode: // the old code may exist in stored attempts
		cause = contract.ErrCommandIdentityConflict
		value.Code = "CommandConflict"
	case "ProductNotFound":
		cause = contract.ErrProductNotFound
	case "DishNotFound":
		cause = contract.ErrDishNotFound
	case "ProposalSuperseded":
		cause = contract.ErrProposalSuperseded
	}
	return &contract.OperationError{Code: value.Code, Message: value.Message, Outcome: value.Outcome, Cause: cause}
}

func publicErrorMessage(err error) string {
	if err == nil {
		return "operation failed"
	}
	var remote *contract.OperationError
	if errors.As(err, &remote) && remote.Message != "" {
		return remote.Message
	}
	return err.Error()
}

func proposalNotFoundUnknown() error {
	return &contract.OperationError{Code: "ProposalNotFound", Message: "proposal outcome is unknown", Outcome: "unknown", Cause: contract.ErrProposalNotFound}
}

func unknownCompletionError(err error) error {
	return &contract.OperationError{Code: "InventoryUnavailable", Message: "warehouse outcome is known but local completion is pending", Outcome: "unknown", Cause: err}
}

func unknownLookupError(err error) error {
	var operation *contract.OperationError
	if errors.As(err, &operation) {
		value := *operation
		if value.Code != "RuntimeReset" || value.Outcome != "indeterminate" {
			value.Outcome = "unknown"
		}
		return &value
	}
	return &contract.OperationError{Code: "InventoryUnavailable", Message: "warehouse attempt outcome is unknown", Outcome: "unknown", Cause: err}
}

func mapAttemptStoreError(err error) error {
	switch {
	case errors.Is(err, contract.ErrCommandIdentityConflict):
		return contract.ErrCommandIdentityConflict
	case errors.Is(err, contract.ErrConfirmationPending):
		return errPendingMismatch
	case errors.Is(err, contract.ErrProposalNotFound):
		return contract.ErrProposalNotFound
	default:
		return err
	}
}

func domainRequestedFromContractAccepted(values []contract.AcceptedPosition) []domain.Position {
	result := make([]domain.Position, len(values))
	for index, value := range values {
		result[index] = domain.Position{LineNo: value.LineNo, DishID: value.DishID, Portions: value.Portions}
	}
	return result
}

func requireSavedEpoch(ctx context.Context, saved contract.WarehouseContext) error {
	if current, ok := contract.WarehouseBindingFromContext(ctx); ok && saved.Epoch != "" && current.Context.Epoch != saved.Epoch {
		return &contract.OperationError{Code: "RuntimeReset", Outcome: "indeterminate"}
	}
	return nil
}
