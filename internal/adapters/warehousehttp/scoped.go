package warehousehttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/JustIkra/umir-serving/contract"
)

type TokenVerifier interface {
	Verify(context.Context, string) (contract.RequestScope, error)
}

type ScopedOptions struct {
	TenantID   string
	Epoch      string
	ServiceKey string
	Verifier   TokenVerifier
}

func NewScopedClient(baseURL string, client *http.Client, timeout time.Duration, options ScopedOptions) (*Client, error) {
	if !contract.ValidEntityID(options.TenantID) || !contract.ValidEntityID(options.Epoch) || options.ServiceKey == "" || options.Verifier == nil || timeout > 5*time.Second {
		return nil, contract.ErrInvalidArgument
	}
	c, err := NewClient(baseURL, client, timeout)
	if err != nil {
		return nil, err
	}
	c.scoped = &options
	return c, nil
}

func (c *Client) authorize(ctx context.Context, method, path string) (contract.RequestScope, error) {
	if err := ctx.Err(); err != nil {
		return contract.RequestScope{}, err
	}
	scope, ok := contract.RequestScopeFromContext(ctx)
	forbidden := &contract.OperationError{Code: "Forbidden", Message: "warehouse consumer scope is not authorized", Outcome: "unavailable"}
	if !ok || scope.TenantID != c.scoped.TenantID || scope.Token() == "" {
		return contract.RequestScope{}, forbidden
	}
	verified, err := c.scoped.Verifier.Verify(ctx, scope.Token())
	if err != nil {
		return contract.RequestScope{}, err
	}
	if verified.OrgID != scope.OrgID || verified.TenantID != scope.TenantID || verified.UserID != scope.UserID || verified.Operation != scope.Operation || verified.TimeZone != scope.TimeZone || verified.SettingsVersion != scope.SettingsVersion || verified.LifecycleVersion != scope.LifecycleVersion || !slices.Equal(verified.Permissions, scope.Permissions) {
		return contract.RequestScope{}, forbidden
	}
	allowed := false
	switch {
	case method == http.MethodGet && path == "/recommendation-inputs":
		allowed = scope.Allows("consumer.recommendations.read") && scope.Operation == "recommendations"
	case method == http.MethodGet && (path == "/products" || path == "/dish-catalog" || path == "/dish"):
		allowed = scope.Allows("catalog.read") && ((path == "/products" && scope.Operation == "servingProducts") || (path == "/dish-catalog" && scope.Operation == "servingDishes") || (path == "/dish" && scope.Operation == "servingDish"))
	case method == http.MethodGet && path == "/menu-evaluation":
		allowed = scope.Allows("consumer.menu.evaluate") && slices.Contains([]string{"menu", "stopList", "recommendations"}, scope.Operation)
	case method == http.MethodPost && path == "/order-evaluations":
		allowed = scope.Allows("consumer.order.evaluate") && slices.Contains([]string{"prepareOrder", "confirmOrder"}, scope.Operation)
	case method == http.MethodGet && (path == "/commands" || path == "/orders" || path == "/order-attempts"):
		allowed = scope.Allows("consumer.order.resolve") && slices.Contains([]string{"confirmOrder", "resolveOrder"}, scope.Operation)
	case method == http.MethodPost && path == "/order-commits":
		allowed = scope.Allows("consumer.order.commit") && scope.Operation == "confirmOrder"
	}
	if !allowed {
		return contract.RequestScope{}, forbidden
	}
	return scope, nil
}

type wireSettings struct {
	TimeZone        string `json:"timeZone"`
	SettingsVersion string `json:"settingsVersion"`
}
type wireBusinessContext struct {
	OrgID          string       `json:"orgID"`
	TenantID       string       `json:"tenantID"`
	RuntimeEpoch   string       `json:"runtimeEpoch"`
	WarehouseEpoch string       `json:"warehouseEpoch,omitempty"`
	Settings       wireSettings `json:"settings"`
	EvaluatedAt    string       `json:"evaluatedAt"`
	StateVersion   string       `json:"stateVersion"`
}

type wireEnvelope struct {
	Context wireBusinessContext `json:"context"`
	Result  json.RawMessage     `json:"result"`
}

func (c *Client) decodeScoped(ctx context.Context, scope contract.RequestScope, path string, query url.Values, payload any, data []byte, output any) error {
	if path == "/recommendation-inputs" {
		if err := validateFINEnvelope(data); err != nil {
			return protocolError(err)
		}
	}
	var envelope wireEnvelope
	if err := decodeStrict(data, &envelope); err != nil {
		return protocolError(err)
	}
	m := envelope.Context
	at, err := parseInstant(m.EvaluatedAt)
	if err != nil {
		return protocolError(err)
	}
	expectedEpoch := c.scoped.Epoch
	expectedZone, expectedVersion := scope.TimeZone, scope.SettingsVersion
	recordedZone, recordedVersion := m.Settings.TimeZone, m.Settings.SettingsVersion
	checkSettings := true
	if binding, ok := contract.WarehouseBindingFromContext(ctx); ok {
		if binding.Context.OrgID != scope.OrgID || binding.Context.TenantID != scope.TenantID || !contract.ValidEntityID(binding.Context.Epoch) {
			return protocolError(errors.New("invalid retained warehouse scope"))
		}
		expectedEpoch = binding.Context.Epoch
		if binding.Retained {
			expectedZone = binding.Context.TimeZone
			expectedVersion = binding.Context.SettingsVersion
			if isPassiveRead(path) {
				// The Warehouse echoes the settings of the presented token, which
				// may be newer than the ones the attempt was made under. A passive
				// read of a known outcome is bound by org, tenant and epoch only,
				// and the observation keeps the settings saved with the attempt.
				checkSettings = false
				recordedZone, recordedVersion = expectedZone, expectedVersion
			}
		}
	}
	version, err := parseInt64(m.StateVersion, false)
	if err != nil || m.OrgID != scope.OrgID || m.TenantID != scope.TenantID || m.RuntimeEpoch != expectedEpoch || (m.WarehouseEpoch != "" && m.WarehouseEpoch != m.RuntimeEpoch) || (checkSettings && (m.Settings.TimeZone != expectedZone || m.Settings.SettingsVersion != expectedVersion)) {
		return protocolError(errors.New("warehouse context differs from selected scope"))
	}
	if err := decodeStrict(envelope.Result, output); err != nil {
		return protocolError(err)
	}
	if err := c.validateScopedResult(path, query, payload, envelope.Result, at, version); err != nil {
		return protocolError(err)
	}
	contract.RecordWarehouseContext(ctx, contract.WarehouseContext{OrgID: m.OrgID, TenantID: m.TenantID, Epoch: m.RuntimeEpoch, TimeZone: recordedZone, SettingsVersion: recordedVersion, EvaluatedAt: at, StateVersion: version})
	return nil
}

func isPassiveRead(path string) bool {
	return path == "/order-attempts" || path == "/commands" || path == "/orders"
}

func (c *Client) validateScopedResult(path string, query url.Values, payload any, data []byte, at time.Time, version int64) error {
	var metadata struct {
		EvaluatedAt  string `json:"evaluatedAt"`
		StateVersion string `json:"stateVersion"`
	}
	if err := json.Unmarshal(data, &metadata); err != nil {
		return err
	}
	if path != "/order-commits" && path != "/commands" && path != "/orders" && path != "/order-attempts" {
		evaluated, err := parseInstant(metadata.EvaluatedAt)
		if err != nil || !evaluated.Equal(at) || metadata.StateVersion != strconv.FormatInt(version, 10) {
			return errors.New("result metadata differs from envelope")
		}
	}
	switch path {
	case "/recommendation-inputs":
		var input wireRecommendationInputs
		if err := decodeStrict(data, &input); err != nil {
			return err
		}
		_, err := c.recommendationInputsFromWire(input)
		return err
	case "/menu-evaluation":
		var menu wireMenuEvaluation
		if err := decodeStrict(data, &menu); err != nil {
			return err
		}
		return validateMenu(menu, parseInstant)
	case "/products":
		var products struct {
			EvaluatedAt  string        `json:"evaluatedAt"`
			StateVersion string        `json:"stateVersion"`
			Items        []wireProduct `json:"items"`
		}
		if err := decodeStrict(data, &products); err != nil {
			return err
		}
		if products.Items == nil {
			return errors.New("products array is missing")
		}
		seen := make(map[string]bool)
		for _, p := range products.Items {
			if _, err := productFromWire(p); err != nil {
				return err
			}
			if seen[p.ProductID] || !validUnit(p.BaseUnit) {
				return errors.New("invalid or duplicate product")
			}
			seen[p.ProductID] = true
		}
	case "/dish-catalog":
		var dishes struct {
			EvaluatedAt  string     `json:"evaluatedAt"`
			StateVersion string     `json:"stateVersion"`
			Items        []wireDish `json:"items"`
		}
		if err := decodeStrict(data, &dishes); err != nil {
			return err
		}
		if dishes.Items == nil {
			return errors.New("dish array is missing")
		}
		seen := make(map[string]bool)
		for _, d := range dishes.Items {
			if err := validateDish(d); err != nil {
				return err
			}
			if seen[d.DishID] {
				return errors.New("duplicate dish")
			}
			seen[d.DishID] = true
		}
	case "/dish":
		var details struct {
			EvaluatedAt  string   `json:"evaluatedAt"`
			StateVersion string   `json:"stateVersion"`
			Dish         wireDish `json:"dish"`
		}
		if err := decodeStrict(data, &details); err != nil {
			return err
		}
		if details.Dish.DishID != query.Get("dishID") {
			return errors.New("dish details differ from query")
		}
		return validateDish(details.Dish)
	case "/order-evaluations":
		var evaluation wireOrderEvaluation
		if err := decodeStrict(data, &evaluation); err != nil {
			return err
		}
		value, err := evaluationFromWire(evaluation, parseInstant)
		if err != nil {
			return err
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		var input struct {
			Positions []wirePosition `json:"positions"`
		}
		if err := decodeStrict(encoded, &input); err != nil {
			return err
		}
		return validateEvaluation(value, input.Positions)
	case "/order-commits":
		var commit wireCommitResult
		if err := decodeStrict(data, &commit); err != nil {
			return err
		}
		result, err := validateScopedCommit(commit, at, version)
		if err != nil {
			return err
		}
		{
			encoded, err := json.Marshal(payload)
			if err != nil {
				return err
			}
			var input struct {
				CommandID         string         `json:"commandID"`
				ProposalID        string         `json:"proposalID"`
				AcceptedPositions []wirePosition `json:"acceptedPositions"`
			}
			if err := decodeStrict(encoded, &input); err != nil {
				return err
			}
			expected := make([]contract.AcceptedPosition, len(input.AcceptedPositions))
			for i, p := range input.AcceptedPositions {
				expected[i], err = acceptedPositionFromWire(p)
				if err != nil {
					return err
				}
			}
			if result.Kind == contract.RecheckRequired {
				return validateEvaluation(*result.OrderEvaluation, input.AcceptedPositions)
			}
			if result.Order.ProposalID != input.ProposalID || !samePositions(result.Order.Positions, expected) {
				return errors.New("committed order differs from input")
			}
		}
	case "/commands":
		var lookup struct {
			Found  *bool              `json:"found"`
			Record *wireCommandRecord `json:"record,omitempty"`
		}
		if err := decodeStrict(data, &lookup); err != nil {
			return err
		}
		if lookup.Record != nil {
			if lookup.Record.CommandID != query.Get("commandID") {
				return errors.New("command identity differs from query")
			}
			result, err := commandResultFromWire(lookup.Record.Result, parseInstant)
			if err != nil {
				return err
			}
			if result.Order != nil {
				return validateScopedOrder(*result.Order, at)
			}
		}
	case "/orders":
		var lookup struct {
			Found *bool      `json:"found"`
			Order *wireOrder `json:"order,omitempty"`
		}
		if err := decodeStrict(data, &lookup); err != nil {
			return err
		}
		if lookup.Order != nil {
			order, err := orderFromWire(*lookup.Order, parseInstant)
			if err != nil {
				return err
			}
			if order.OrderID != query.Get("orderID") {
				return errors.New("order identity differs from query")
			}
			return validateScopedOrder(order, at)
		}
	case "/order-attempts":
		var attempt wireOrderAttempt
		if err := decodeStrict(data, &attempt); err != nil {
			return err
		}
		if attempt.State == contract.OrderAttemptCompleted && attempt.Result != nil {
			_, err := validateScopedCommit(*attempt.Result, at, version)
			return err
		}
	}
	return nil
}

func validateScopedOrder(order contract.Order, at time.Time) error {
	if len(order.Positions) == 0 || len(order.Allocations) == 0 || order.PlacedAt.After(at) {
		return errors.New("invalid scoped order composition or instant")
	}
	return nil
}

func validateScopedCommit(commit wireCommitResult, at time.Time, version int64) (contract.CommitResult, error) {
	result, err := commitResultFromWire(commit, parseInstant)
	if err != nil {
		return contract.CommitResult{}, err
	}
	switch result.Kind {
	case contract.CommitPending:
		return contract.CommitResult{}, errors.New("v2 commit requires a terminal result")
	case contract.RecheckRequired:
		if !result.OrderEvaluation.EvaluatedAt.Equal(at) || result.OrderEvaluation.StateVersion != version {
			return contract.CommitResult{}, errors.New("recheck metadata differs from envelope")
		}
	case contract.OrderCommitted:
		if err := validateScopedOrder(*result.Order, at); err != nil {
			return contract.CommitResult{}, err
		}
	}
	return result, nil
}

func validUnit(unit string) bool { return unit == "g" || unit == "ml" || unit == "pcs" }

func validateMenu(menu wireMenuEvaluation, parseTime func(string) (time.Time, error)) error {
	at, err := parseTime(menu.EvaluatedAt)
	if err != nil {
		return err
	}
	if menu.Dishes == nil {
		return errors.New("menu array is missing")
	}
	seen := make(map[string]bool)
	batchExpiry := make(map[[2]string]time.Time)
	productUnits := make(map[string]string)
	for _, dish := range menu.Dishes {
		if dish.DishID == "" || dish.DishName == "" || dish.IsAvailable == nil || dish.Allocations == nil || seen[dish.DishID] {
			return errors.New("invalid menu dish")
		}
		seen[dish.DishID] = true
		deficits, err := deficitsFromWire(dish.Deficits)
		if err != nil {
			return err
		}
		if (*dish.IsAvailable && (len(deficits) > 0 || len(dish.Allocations) == 0)) || (!*dish.IsAvailable && len(dish.Allocations) > 0) {
			return errors.New("availability differs from allocations")
		}
		allocations := make(map[[2]string]bool)
		for _, a := range dish.Allocations {
			key := [2]string{a.ProductID, a.BatchID}
			expires, err := parseTime(a.ExpiresAt)
			if err != nil || !expires.After(at) || a.ProductID == "" || a.ProductName == "" || a.BatchID == "" || !validUnit(a.BaseUnit) || allocations[key] {
				return errors.New("invalid or expired allocation")
			}
			if _, err := parseInt64(a.Quantity, true); err != nil {
				return err
			}
			if previous, ok := batchExpiry[key]; ok && !previous.Equal(expires) {
				return errors.New("inconsistent menu batch expiry")
			}
			if previous, ok := productUnits[a.ProductID]; ok && previous != a.BaseUnit {
				return errors.New("inconsistent menu product unit")
			}
			batchExpiry[key] = expires
			productUnits[a.ProductID] = a.BaseUnit
			allocations[key] = true
		}
	}
	return nil
}

type wireScopedError struct {
	Code      string `json:"code"`
	Outcome   string `json:"outcome"`
	Message   string `json:"message"`
	Retryable *bool  `json:"retryable"`
	Details   *struct {
		ExpectedEpoch     string        `json:"expectedEpoch,omitempty"`
		CurrentEpoch      string        `json:"currentEpoch,omitempty"`
		CurrentSettings   *wireSettings `json:"currentSettings,omitempty"`
		RefreshRequired   *bool         `json:"refreshRequired,omitempty"`
		RetryAfterSeconds *int          `json:"retryAfterSeconds,omitempty"`
		ScopeID           string        `json:"scopeID,omitempty"`
	} `json:"details,omitempty"`
	FieldErrors *struct {
		Login           string `json:"login,omitempty"`
		Password        string `json:"password,omitempty"`
		CurrentPassword string `json:"currentPassword,omitempty"`
		NewPassword     string `json:"newPassword,omitempty"`
		DisplayName     string `json:"displayName,omitempty"`
		Email           string `json:"email,omitempty"`
	} `json:"fieldErrors,omitempty"`
	SessionRevoked *bool `json:"sessionRevoked,omitempty"`
}

func decodeScopedError(method, path string, response *http.Response, data []byte) error {
	if err := requireJSON(response.Header.Get("Content-Type")); err != nil {
		return exchangeError(method, err)
	}
	var value wireScopedError
	if err := decodeStrict(data, &value); err != nil || value.Code == "" || value.Message == "" || value.Retryable == nil {
		return exchangeError(method, errors.New("invalid scoped warehouse error"))
	}
	status := 0
	var cause error
	switch value.Code {
	case "InvalidArgument", "InvalidRequest":
		status = http.StatusBadRequest
		cause = contract.ErrInvalidArgument
	case "Unauthenticated", "InvalidToken":
		status = http.StatusUnauthorized
	case "Forbidden":
		status = http.StatusForbidden
	case "ScopeDeleted":
		status = http.StatusGone
	case "CommandConflict":
		status = http.StatusConflict
		cause = contract.ErrCommandIdentityConflict
	case "RuntimeReset", "SettingsChanged", "RefreshRequired", "ProductConflict", "DishConflict":
		status = http.StatusConflict
	case "NotFound":
		status = http.StatusNotFound
	case "ProposalNotFound":
		status = http.StatusNotFound
		cause = contract.ErrProposalNotFound
	case "RuntimeNotReady", "InventoryUnavailable", "CalculationUnavailable", "AuthUnavailable", "ServiceUnavailable", "TimeZoneDataUnavailable":
		status = http.StatusServiceUnavailable
		cause = contract.ErrInventoryUnavailable
	case "TechnicalError":
		status = http.StatusInternalServerError
	case "UpstreamUnavailable":
		status = http.StatusBadGateway
	case "UpstreamTimeout":
		status = http.StatusGatewayTimeout
	case "RateLimited":
		status = http.StatusTooManyRequests
	case "LimitExceeded":
		status = http.StatusTooManyRequests
		if path == "/recommendation-inputs" {
			status = http.StatusServiceUnavailable
			value.Code = "CalculationUnavailable"
			cause = contract.ErrInventoryUnavailable
		}
	default:
		return exchangeError(method, errors.New("unknown scoped warehouse error"))
	}
	if response.StatusCode != status {
		return exchangeError(method, errors.New("scoped warehouse error status mismatch"))
	}
	if value.Code == "RuntimeReset" {
		if value.Outcome != "indeterminate" {
			return exchangeError(method, errors.New("reset outcome mismatch"))
		}
	} else if method == http.MethodGet {
		if value.Outcome != "unavailable" {
			return exchangeError(method, errors.New("read error outcome mismatch"))
		}
	} else if value.Outcome != "not_committed" && value.Outcome != "unknown" && value.Outcome != "unavailable" {
		return exchangeError(method, errors.New("mutation error outcome mismatch"))
	}
	return &contract.OperationError{Code: value.Code, Message: value.Message, Outcome: value.Outcome, Cause: cause, Retryable: *value.Retryable}
}

func (c *Client) preparation(scope contract.RequestScope, headers http.Header) (*contract.WarehousePreparation, error) {
	names := []string{"X-Umir-Committed-Version", "X-Umir-Invalidation-Required", "X-Umir-Runtime-Epoch"}
	present := false
	for _, name := range names {
		if len(headers.Values(name)) > 0 {
			present = true
		}
	}
	if !present {
		return nil, nil
	}
	for _, name := range names {
		if len(headers.Values(name)) != 1 {
			return nil, errors.New("incomplete preparation metadata")
		}
	}
	version, err := parseInt64(headers.Get(names[0]), false)
	if err != nil || headers.Get(names[1]) != "true" || headers.Get(names[2]) != c.scoped.Epoch {
		return nil, errors.New("invalid preparation metadata")
	}
	return &contract.WarehousePreparation{OrgID: scope.OrgID, TenantID: scope.TenantID, Epoch: c.scoped.Epoch, CommittedVersion: version}, nil
}

func validateDish(dish wireDish) error {
	if _, err := dishFromWire(dish); err != nil {
		return err
	}
	if len(dish.Recipe) == 0 {
		return errors.New("empty recipe")
	}
	seen := make(map[string]bool)
	for _, ingredient := range dish.Recipe {
		if !validUnit(ingredient.BaseUnit) || seen[ingredient.ProductID] {
			return errors.New("invalid recipe unit or duplicate ingredient")
		}
		seen[ingredient.ProductID] = true
	}
	return nil
}

func validateEvaluation(evaluation contract.OrderEvaluation, requested []wirePosition) error {
	if len(requested) == 0 || len(evaluation.RequestedPositions) != len(requested) {
		return errors.New("evaluation request shape mismatch")
	}
	expected := make(map[int]contract.Position, len(requested))
	for i, p := range requested {
		value, err := positionFromWire(p)
		if err != nil {
			return err
		}
		if value != evaluation.RequestedPositions[i] || expected[value.LineNo].DishID != "" {
			return errors.New("evaluation requested positions mismatch")
		}
		expected[value.LineNo] = value
	}
	accepted := make(map[int]int64)
	for _, p := range evaluation.AcceptedPositions {
		original, ok := expected[p.LineNo]
		if !ok || p.DishID != original.DishID || p.Portions > original.Portions || accepted[p.LineNo] != 0 {
			return errors.New("evaluation accepted positions mismatch")
		}
		accepted[p.LineNo] = p.Portions
	}
	changes := make(map[int]bool)
	for _, change := range evaluation.Changes {
		original, ok := expected[change.LineNo]
		if !ok || change.DishID != original.DishID || change.RequestedPortions != original.Portions || change.AcceptedPortions != accepted[change.LineNo] || change.AcceptedPortions >= original.Portions || len(change.Deficits) == 0 || changes[change.LineNo] {
			return errors.New("evaluation change mismatch")
		}
		for _, deficit := range change.Deficits {
			if deficit.AvailableQuantity >= deficit.RequiredQuantity {
				return errors.New("invalid evaluation deficit")
			}
		}
		changes[change.LineNo] = true
	}
	for line, original := range expected {
		if (accepted[line] < original.Portions) != changes[line] {
			return errors.New("evaluation change missing")
		}
	}
	return nil
}
