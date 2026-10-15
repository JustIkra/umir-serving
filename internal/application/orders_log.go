package application

import (
	"context"
	"errors"
	"log/slog"
	"unicode/utf8"

	"github.com/JustIkra/umir-serving/contract"
)

// Event names written by OrdersService. Records carry identifiers, codes and
// versions only: never tokens, request bodies, error texts or addresses.
const (
	eventConfirmationUnknown         = "confirmation.unknown"
	eventConfirmationPendingStarted  = "confirmation.pending.started"
	eventConfirmationPendingResolved = "confirmation.pending.resolved"

	maxLoggedIDRunes = 256
)

// WithLogger sets the logger of the service and returns it. Call it while
// assembling the service, before the first request. A nil logger discards.
func (s *OrdersService) WithLogger(logger *slog.Logger) *OrdersService {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	s.log = logger
	return s
}

// confirmTrace follows one ConfirmOrder or ResolveConfirmation call so that the
// outcome can be logged once, where the call ends.
type confirmTrace struct {
	operation       string
	request         contract.ConfirmOrderRequest
	warehouseEpoch  string
	settingsVersion string
	// retainedPending is true when the call started from a stored attempt that
	// had already been reserved for a Warehouse commit and had no outcome.
	retainedPending bool
	// reserved is true when this call reserved the attempt itself.
	reserved bool
}

func newConfirmTrace(operation string, request contract.ConfirmOrderRequest) *confirmTrace {
	return &confirmTrace{operation: operation, request: request}
}

func (t *confirmTrace) observeAttempt(attempt contract.ConfirmationAttempt) {
	t.warehouseEpoch, t.settingsVersion = attempt.Context.Epoch, attempt.Context.SettingsVersion
	t.retainedPending = attempt.State == contract.OrderAttemptPending && attempt.Pending && attempt.Result == nil && attempt.Error == nil
}

func (t *confirmTrace) observeSavedContext(saved contract.WarehouseContext) {
	if t.warehouseEpoch == "" {
		t.warehouseEpoch, t.settingsVersion = saved.Epoch, saved.SettingsVersion
	}
}

func (s *OrdersService) logConfirmOutcome(ctx context.Context, t *confirmTrace, result contract.ConfirmResult, err error) {
	outcome := classifyConfirmOutcome(result, err)
	emit := func(level slog.Level, event string, extra ...slog.Attr) {
		attrs := []slog.Attr{
			slog.String("event", event),
			slog.String("operation", t.operation),
			slog.String("command_id", boundedID(t.request.CommandID)),
			slog.String("proposal_id", boundedID(t.request.ProposalID)),
		}
		if scope, ok := contract.RequestScopeFromContext(ctx); ok {
			attrs = append(attrs, slog.String("tenant_id", boundedID(scope.TenantID)), slog.String("org_id", boundedID(scope.OrgID)))
			if scope.RuntimeEpoch != "" {
				attrs = append(attrs, slog.String("runtime_epoch", boundedID(scope.RuntimeEpoch)))
			}
		}
		if t.warehouseEpoch != "" {
			attrs = append(attrs, slog.String("warehouse_epoch", boundedID(t.warehouseEpoch)), slog.String("settings_version", boundedID(t.settingsVersion)))
		}
		s.log.LogAttrs(context.WithoutCancel(ctx), level, event, append(attrs, extra...)...)
	}
	if t.reserved && (outcome.state == outcomePending || outcome.state == outcomeUnknown) {
		emit(slog.LevelInfo, eventConfirmationPendingStarted)
	}
	switch outcome.state {
	case outcomeUnknown:
		level := slog.LevelError
		if outcome.code == "ProposalNotFound" {
			level = slog.LevelWarn // a Resolve for a command this process never saw
		}
		extra := []slog.Attr{slog.String("error_code", outcome.code), slog.String("outcome", outcome.outcome)}
		if outcome.cause != "" {
			extra = append(extra, slog.String("cause", outcome.cause))
		}
		emit(level, eventConfirmationUnknown, extra...)
	case outcomeTerminal:
		if t.retainedPending {
			emit(slog.LevelInfo, eventConfirmationPendingResolved, slog.String("result", outcome.code))
		}
	}
}

type outcomeState int

const (
	outcomeOther outcomeState = iota // a failure that says nothing about the attempt
	outcomePending
	outcomeUnknown
	outcomeTerminal
)

type confirmOutcome struct {
	state   outcomeState
	outcome string // "unknown" or "indeterminate" for outcomeUnknown
	code    string // error code, or result kind of a terminal outcome
	cause   string // "deadline" or "canceled" when the context ended the call
}

func classifyConfirmOutcome(result contract.ConfirmResult, err error) confirmOutcome {
	if err == nil {
		if result.Kind == contract.ConfirmationPending {
			return confirmOutcome{state: outcomePending}
		}
		return confirmOutcome{state: outcomeTerminal, code: string(result.Kind)}
	}
	cause := ""
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		cause = "deadline"
	case errors.Is(err, context.Canceled):
		cause = "canceled"
	}
	var operation *contract.OperationError
	switch {
	case errors.As(err, &operation):
		if operation.Outcome == "unknown" || operation.Outcome == "indeterminate" {
			return confirmOutcome{state: outcomeUnknown, outcome: operation.Outcome, code: operation.Code, cause: cause}
		}
		if operation.Outcome == "not_committed" {
			return confirmOutcome{state: outcomeTerminal, code: operation.Code}
		}
	case cause != "":
		return confirmOutcome{state: outcomeUnknown, outcome: "unknown", code: "ContextDone", cause: cause}
	case definiteCommitRejection(err):
		return confirmOutcome{state: outcomeTerminal, code: "Rejected"}
	}
	return confirmOutcome{state: outcomeOther}
}

// boundedID keeps client-supplied identifiers from growing a record.
func boundedID(value string) string {
	if utf8.RuneCountInString(value) <= maxLoggedIDRunes {
		return value
	}
	runes := []rune(value)
	return string(runes[:maxLoggedIDRunes])
}
