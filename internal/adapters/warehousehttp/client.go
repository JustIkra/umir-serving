package warehousehttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/JustIkra/umir-serving/contract"
	"github.com/JustIkra/umir-serving/internal/adapters/httpjson"
)

const maxResponseBytes = 16 << 20

type Client struct {
	scoped  *ScopedOptions
	baseURL string
	http    *http.Client
	timeout time.Duration
}

var _ contract.OrdersWarehouse = (*Client)(nil)
var _ contract.CatalogWarehouse = (*Client)(nil)
var _ contract.MenuWarehouse = (*Client)(nil)
var _ contract.OrderAttemptReader = (*Client)(nil)

func NewHTTPClient() *http.Client {
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext, ForceAttemptHTTP2: true, MaxIdleConns: 64, MaxIdleConnsPerHost: 64, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 5 * time.Second, ExpectContinueTimeout: time.Second}
	return &http.Client{Transport: transport}
}

func NewClient(baseURL string, client *http.Client, timeout time.Duration) (*Client, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || timeout <= 0 {
		return nil, contract.ErrInvalidArgument
	}
	if client == nil {
		client = NewHTTPClient()
	}
	clientCopy := *client
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	base := strings.TrimRight(parsed.String(), "/")
	return &Client{baseURL: base, http: &clientCopy, timeout: timeout}, nil
}

func (c *Client) GetProducts(ctx context.Context) (contract.Products, error) {
	var response struct {
		EvaluatedAt  string        `json:"evaluatedAt"`
		StateVersion string        `json:"stateVersion"`
		Items        []wireProduct `json:"items"`
	}
	if err := c.get(ctx, "/products", nil, &response); err != nil {
		return contract.Products{}, err
	}
	evaluatedAt, err := c.parseTimestamp(response.EvaluatedAt)
	if err != nil {
		return contract.Products{}, protocolError(err)
	}
	version, err := parseInt64(response.StateVersion, false)
	if err != nil || response.Items == nil {
		return contract.Products{}, protocolError(errors.New("invalid products payload"))
	}
	result := contract.Products{EvaluatedAt: evaluatedAt, StateVersion: version, Items: make([]contract.Product, len(response.Items))}
	for index, item := range response.Items {
		result.Items[index], err = productFromWire(item)
		if err != nil {
			return contract.Products{}, protocolError(err)
		}
	}
	return result, nil
}

func (c *Client) GetDishCatalog(ctx context.Context) (contract.DishCatalog, error) {
	var response struct {
		EvaluatedAt  string     `json:"evaluatedAt"`
		StateVersion string     `json:"stateVersion"`
		Items        []wireDish `json:"items"`
	}
	if err := c.get(ctx, "/dish-catalog", nil, &response); err != nil {
		return contract.DishCatalog{}, err
	}
	evaluatedAt, err := c.parseTimestamp(response.EvaluatedAt)
	if err != nil {
		return contract.DishCatalog{}, protocolError(err)
	}
	version, err := parseInt64(response.StateVersion, false)
	if err != nil || response.Items == nil {
		return contract.DishCatalog{}, protocolError(errors.New("invalid dish catalog payload"))
	}
	result := contract.DishCatalog{EvaluatedAt: evaluatedAt, StateVersion: version, Items: make([]contract.Dish, len(response.Items))}
	for index, item := range response.Items {
		result.Items[index], err = dishFromWire(item)
		if err != nil {
			return contract.DishCatalog{}, protocolError(err)
		}
	}
	return result, nil
}

func (c *Client) GetDish(ctx context.Context, dishID string) (contract.DishDetails, error) {
	query := url.Values{"dishID": []string{dishID}}
	var response struct {
		EvaluatedAt  string   `json:"evaluatedAt"`
		StateVersion string   `json:"stateVersion"`
		Dish         wireDish `json:"dish"`
	}
	if dishID == "" {
		return contract.DishDetails{}, contract.ErrInvalidArgument
	}
	if err := c.get(ctx, "/dish", query, &response); err != nil {
		return contract.DishDetails{}, err
	}
	evaluatedAt, err := c.parseTimestamp(response.EvaluatedAt)
	if err != nil {
		return contract.DishDetails{}, protocolError(err)
	}
	version, err := parseInt64(response.StateVersion, false)
	if err != nil {
		return contract.DishDetails{}, protocolError(err)
	}
	dish, err := dishFromWire(response.Dish)
	if err != nil {
		return contract.DishDetails{}, protocolError(err)
	}
	if dish.DishID != dishID {
		return contract.DishDetails{}, protocolError(errors.New("dish details identity does not match query"))
	}
	return contract.DishDetails{EvaluatedAt: evaluatedAt, StateVersion: version, Dish: dish}, nil
}

func (c *Client) RegisterProduct(ctx context.Context, request contract.RegisterProductRequest) (contract.Product, error) {
	payload := struct {
		CommandID string `json:"commandID"`
		ProductID string `json:"productID"`
		Name      string `json:"name"`
		BaseUnit  string `json:"baseUnit"`
	}{request.CommandID, request.ProductID, request.Name, request.BaseUnit}
	var response wireProduct
	if err := c.post(ctx, "/products", payload, &response); err != nil {
		return contract.Product{}, err
	}
	value, err := productFromWire(response)
	if err != nil {
		return contract.Product{}, protocolError(err)
	}
	if value.ProductID != request.ProductID {
		return contract.Product{}, protocolError(errors.New("registered product identity does not match request"))
	}
	return value, nil
}

func (c *Client) DefineDish(ctx context.Context, request contract.DefineDishRequest) (contract.Dish, error) {
	recipe := make([]wireIngredient, len(request.Recipe))
	for index, value := range request.Recipe {
		recipe[index] = wireIngredient{ProductID: value.ProductID, Quantity: strconv.FormatInt(value.Quantity, 10), BaseUnit: value.BaseUnit}
	}
	payload := struct {
		CommandID   string           `json:"commandID"`
		DishID      string           `json:"dishID"`
		Name        string           `json:"name"`
		Ingredients []wireIngredient `json:"ingredients"`
	}{request.CommandID, request.DishID, request.Name, recipe}
	var response wireDish
	if err := c.post(ctx, "/dishes", payload, &response); err != nil {
		return contract.Dish{}, err
	}
	value, err := dishFromWire(response)
	if err != nil {
		return contract.Dish{}, protocolError(err)
	}
	if value.DishID != request.DishID {
		return contract.Dish{}, protocolError(errors.New("defined dish identity does not match request"))
	}
	return value, nil
}

func (c *Client) EvaluateMenu(ctx context.Context) (contract.MenuEvaluation, error) {
	var response wireMenuEvaluation
	if err := c.get(ctx, "/menu-evaluation", nil, &response); err != nil {
		return contract.MenuEvaluation{}, err
	}
	return c.menuFromWire(response)
}

func (c *Client) menuFromWire(response wireMenuEvaluation) (contract.MenuEvaluation, error) {
	evaluatedAt, err := c.parseTimestamp(response.EvaluatedAt)
	if err != nil {
		return contract.MenuEvaluation{}, protocolError(err)
	}
	version, err := parseInt64(response.StateVersion, false)
	if err != nil || response.Dishes == nil {
		return contract.MenuEvaluation{}, protocolError(errors.New("invalid menu evaluation"))
	}
	result := contract.MenuEvaluation{EvaluatedAt: evaluatedAt, StateVersion: version, Dishes: make([]contract.DishEvaluation, len(response.Dishes))}
	for index, dish := range response.Dishes {
		if dish.DishID == "" || dish.DishName == "" || dish.IsAvailable == nil || dish.Allocations == nil {
			return contract.MenuEvaluation{}, protocolError(errors.New("invalid dish evaluation"))
		}
		deficits, parseErr := deficitsFromWire(dish.Deficits)
		if parseErr != nil {
			return contract.MenuEvaluation{}, protocolError(parseErr)
		}
		item := contract.DishEvaluation{DishID: dish.DishID, DishName: dish.DishName, IsAvailable: *dish.IsAvailable, Deficits: deficits, Allocations: make([]contract.MenuAllocation, len(dish.Allocations))}
		for allocationIndex, allocation := range dish.Allocations {
			quantity, parseErr := parseInt64(allocation.Quantity, true)
			if parseErr != nil {
				return contract.MenuEvaluation{}, protocolError(parseErr)
			}
			expiresAt, parseErr := c.parseTimestamp(allocation.ExpiresAt)
			if parseErr != nil || allocation.ProductID == "" || allocation.BatchID == "" {
				return contract.MenuEvaluation{}, protocolError(errors.New("invalid menu allocation"))
			}
			item.Allocations[allocationIndex] = contract.MenuAllocation{ProductID: allocation.ProductID, ProductName: allocation.ProductName, BatchID: allocation.BatchID, Quantity: quantity, BaseUnit: allocation.BaseUnit, ExpiresAt: expiresAt}
		}
		result.Dishes[index] = item
	}
	return result, nil
}

func (c *Client) EvaluateOrder(ctx context.Context, request contract.EvaluateOrderRequest) (contract.OrderEvaluation, error) {
	positions := make([]wirePosition, len(request.Positions))
	for index, position := range request.Positions {
		positions[index] = wirePositionFromContract(position)
	}
	var response wireOrderEvaluation
	if err := c.post(ctx, "/order-evaluations", struct {
		Positions []wirePosition `json:"positions"`
	}{positions}, &response); err != nil {
		return contract.OrderEvaluation{}, err
	}
	evaluation, err := evaluationFromWire(response, c.parseTimestamp)
	if err != nil {
		return contract.OrderEvaluation{}, protocolError(err)
	}
	return evaluation, nil
}

func (c *Client) CommitAcceptedOrder(ctx context.Context, request contract.CommitOrderRequest) (contract.CommitResult, error) {
	positions := make([]wireAcceptedPosition, len(request.AcceptedPositions))
	for index, position := range request.AcceptedPositions {
		positions[index] = wireAcceptedPositionFromContract(position)
	}
	payload := struct {
		CommandID         string                 `json:"commandID"`
		ProposalID        string                 `json:"proposalID"`
		AcceptedPositions []wireAcceptedPosition `json:"acceptedPositions"`
	}{request.CommandID, request.ProposalID, positions}
	var response wireCommitResult
	err := c.request(ctx, http.MethodPost, "/order-commits", nil, payload, &response, http.StatusOK, http.StatusAccepted)
	if err != nil {
		return contract.CommitResult{}, err
	}
	result, err := commitResultFromWire(response, c.parseTimestamp)
	if err != nil {
		return contract.CommitResult{}, protocolError(err)
	}
	if result.Kind == contract.OrderCommitted && (result.Order == nil || result.Order.ProposalID != request.ProposalID || !samePositions(result.Order.Positions, request.AcceptedPositions)) {
		return contract.CommitResult{}, protocolError(errors.New("committed order does not match command input"))
	}
	return result, nil
}

func (c *Client) FindRecordedCommand(ctx context.Context, commandID string) (contract.CommandRecord, bool, error) {
	if commandID == "" {
		return contract.CommandRecord{}, false, contract.ErrInvalidArgument
	}
	var response struct {
		Found  *bool              `json:"found"`
		Record *wireCommandRecord `json:"record,omitempty"`
	}
	if err := c.get(ctx, "/commands", url.Values{"commandID": []string{commandID}}, &response); err != nil {
		return contract.CommandRecord{}, false, err
	}
	if response.Found == nil {
		return contract.CommandRecord{}, false, protocolError(errors.New("found discriminator is required"))
	}
	if !*response.Found {
		if response.Record != nil {
			return contract.CommandRecord{}, false, protocolError(errors.New("record present when found is false"))
		}
		return contract.CommandRecord{}, false, nil
	}
	if response.Record == nil {
		return contract.CommandRecord{}, false, protocolError(errors.New("record missing when found is true"))
	}
	result, err := commandResultFromWire(response.Record.Result, c.parseTimestamp)
	if err != nil {
		return contract.CommandRecord{}, false, protocolError(err)
	}
	record := contract.CommandRecord{CommandID: response.Record.CommandID, CommandType: response.Record.CommandType, CommandFingerprint: response.Record.CommandFingerprint, Result: result}
	if record.CommandID == "" || record.CommandType == "" || record.CommandFingerprint == "" {
		return contract.CommandRecord{}, false, protocolError(errors.New("invalid command record"))
	}
	return record, true, nil
}

func (c *Client) FindOrderByProposal(ctx context.Context, proposalID string) (contract.Order, bool, error) {
	if c.scoped != nil {
		return contract.Order{}, false, contract.ErrInvalidArgument
	}
	return c.findOrder(ctx, "proposalID", proposalID)
}
func (c *Client) FindOrderByID(ctx context.Context, orderID string) (contract.Order, bool, error) {
	return c.findOrder(ctx, "orderID", orderID)
}
func (c *Client) findOrder(ctx context.Context, key, id string) (contract.Order, bool, error) {
	if id == "" {
		return contract.Order{}, false, contract.ErrInvalidArgument
	}
	var response struct {
		Found *bool      `json:"found"`
		Order *wireOrder `json:"order,omitempty"`
	}
	if err := c.get(ctx, "/orders", url.Values{key: []string{id}}, &response); err != nil {
		return contract.Order{}, false, err
	}
	if response.Found == nil {
		return contract.Order{}, false, protocolError(errors.New("found discriminator is required"))
	}
	if !*response.Found {
		if response.Order != nil {
			return contract.Order{}, false, protocolError(errors.New("order present when found is false"))
		}
		return contract.Order{}, false, nil
	}
	if response.Order == nil {
		return contract.Order{}, false, protocolError(errors.New("order missing when found is true"))
	}
	order, err := orderFromWire(*response.Order, c.parseTimestamp)
	if err != nil {
		return contract.Order{}, false, protocolError(err)
	}
	if key == "orderID" && order.OrderID != id {
		return contract.Order{}, false, protocolError(errors.New("order identity differs from query"))
	}
	return order, true, nil
}

func (c *Client) FindOrderAttempt(ctx context.Context, commandID, proposalID string) (contract.OrderAttempt, error) {
	if commandID == "" || proposalID == "" {
		return contract.OrderAttempt{}, contract.ErrInvalidArgument
	}
	query := url.Values{"commandID": []string{commandID}, "proposalID": []string{proposalID}}
	if c.scoped != nil {
		query.Del("proposalID")
	}
	var response wireOrderAttempt
	if err := c.get(ctx, "/order-attempts", query, &response); err != nil {
		return contract.OrderAttempt{}, err
	}
	result := contract.OrderAttempt{State: response.State}
	switch response.State {
	case contract.OrderAttemptUnseen, contract.OrderAttemptPending:
		if response.Result != nil || response.Error != nil {
			return contract.OrderAttempt{}, protocolError(errors.New("inactive attempt payload"))
		}
	case contract.OrderAttemptCompleted:
		if response.Result == nil || response.Error != nil {
			return contract.OrderAttempt{}, protocolError(errors.New("invalid completed attempt payload"))
		}
		parsed, err := commitResultFromWire(*response.Result, c.parseTimestamp)
		if err != nil || parsed.Kind == contract.CommitPending {
			return contract.OrderAttempt{}, protocolError(errors.New("invalid completed result"))
		}
		result.Result = &parsed
	case contract.OrderAttemptRejected:
		if response.Error == nil || response.Result != nil || (response.Error.Code == "" || c.scoped == nil && !knownAttemptErrorCode(response.Error.Code)) || response.Error.Message == "" || response.Error.Outcome != "not_committed" {
			return contract.OrderAttempt{}, protocolError(errors.New("invalid rejected attempt payload"))
		}
		result.Error = &contract.AttemptError{Code: response.Error.Code, Message: response.Error.Message, Outcome: response.Error.Outcome}
	default:
		return contract.OrderAttempt{}, protocolError(fmt.Errorf("unknown attempt state %q", response.State))
	}
	return result, nil
}

func knownAttemptErrorCode(code string) bool {
	switch code {
	case "InvalidArgument", "InventoryUnavailable", "CommandIdentityConflict", "ConcurrentModification", "ProposalNotFound", "ProposalSuperseded", "DishNotFound", "ProductNotFound", "ProductAlreadyExists", "DishAlreadyExists", "DeliveryAlreadyReceived", "InsufficientInventory":
		return true
	default:
		return false
	}
}

func (c *Client) get(ctx context.Context, path string, query url.Values, output any) error {
	return c.request(ctx, http.MethodGet, path, query, nil, output, http.StatusOK)
}

func (c *Client) post(ctx context.Context, path string, payload, output any) error {
	return c.request(ctx, http.MethodPost, path, nil, payload, output, http.StatusOK)
}

func (c *Client) request(ctx context.Context, method, path string, query url.Values, payload, output any, statuses ...int) (resultErr error) {
	requestContext, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if c.scoped != nil {
		defer func() {
			var operation *contract.OperationError
			if method == http.MethodGet && errors.As(resultErr, &operation) && operation.Code == "ProtocolError" {
				copy := *operation
				copy.Outcome = "unavailable"
				resultErr = &copy
			}
		}()
	}
	endpoint := c.baseURL + "/internal/v1" + path
	var scope contract.RequestScope
	if c.scoped != nil {
		var err error
		scope, err = c.authorize(requestContext, method, path)
		if err != nil {
			return err
		}
		endpoint = c.baseURL + "/internal/warehouse/v2/r/" + c.scoped.TenantID + path
	}
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return protocolError(err)
		}
		if c.scoped != nil && len(encoded) > 1<<20 {
			return &contract.OperationError{Code: "InvalidArgument", Message: "warehouse request exceeds 1 MiB", Outcome: "not_committed", Cause: contract.ErrInvalidArgument}
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(requestContext, method, endpoint, body)
	if err != nil {
		return protocolError(err)
	}
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if c.scoped != nil {
		request.Header.Set("Authorization", "Bearer "+scope.Token())
		request.Header.Set("X-Umir-Service-Key", c.scoped.ServiceKey)
		if method == http.MethodPost || path == "/commands" || path == "/orders" || path == "/order-attempts" {
			epoch := c.scoped.Epoch
			if binding, ok := contract.WarehouseBindingFromContext(ctx); ok {
				epoch = binding.Context.Epoch
			}
			request.Header.Set("X-Umir-Runtime-Epoch", epoch)
		}
	}
	response, err := c.http.Do(request)
	if err != nil {
		return exchangeError(method, err)
	}
	if c.scoped != nil {
		preparation, err := c.preparation(scope, response.Header)
		if err != nil {
			return exchangeError(method, errors.Join(err, response.Body.Close()))
		}
		if preparation != nil {
			defer func() {
				contract.RecordWarehousePreparation(ctx, *preparation)
				var operation *contract.OperationError
				if errors.As(resultErr, &operation) {
					copy := *operation
					copy.Preparation = preparation
					resultErr = &copy
				}
			}()
		}
		if response.StatusCode == http.StatusAccepted {
			return exchangeError(method, errors.Join(errors.New("v2 does not return legacy pending success"), response.Body.Close()))
		}
	}
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	closeErr := response.Body.Close()
	if err != nil {
		return exchangeError(method, errors.Join(err, closeErr))
	}
	if closeErr != nil {
		return exchangeError(method, closeErr)
	}
	if len(responseBody) > maxResponseBytes {
		return exchangeError(method, errors.New("warehouse response exceeds 16 MiB"))
	}
	if !containsStatus(statuses, response.StatusCode) {
		if c.scoped != nil {
			return decodeScopedError(method, path, response, responseBody)
		}
		return decodeRemoteError(request.Method, path, response, responseBody)
	}
	if response.StatusCode == http.StatusAccepted {
		if method != http.MethodPost || path != "/order-commits" || output == nil {
			return protocolError(errors.New("unexpected 202 response"))
		}
	}
	if err := requireJSON(response.Header.Get("Content-Type")); err != nil {
		return exchangeError(method, err)
	}
	if c.scoped != nil {
		return c.decodeScoped(ctx, scope, path, query, payload, responseBody, output)
	}
	if err := decodeStrict(responseBody, output); err != nil {
		return exchangeError(method, err)
	}
	if method == http.MethodPost && path == "/order-commits" {
		commit, ok := output.(*wireCommitResult)
		if !ok {
			return protocolError(errors.New("invalid commit decoder"))
		}
		if response.StatusCode == http.StatusAccepted && commit.Kind != contract.CommitPending {
			return protocolError(errors.New("202 requires Pending result"))
		}
		if response.StatusCode == http.StatusOK && commit.Kind == contract.CommitPending {
			return protocolError(errors.New("pending requires HTTP 202 status"))
		}
	}
	return nil
}

func containsStatus(statuses []int, status int) bool {
	for _, allowed := range statuses {
		if allowed == status {
			return true
		}
	}
	return false
}

func requireJSON(header string) error {
	mediaType, _, err := mime.ParseMediaType(header)
	if err != nil || mediaType != "application/json" {
		return fmt.Errorf("unexpected content type %q", header)
	}
	return nil
}

func decodeStrict(data []byte, value any) error {
	return httpjson.DecodeStrict(data, value)
}

func decodeRemoteError(method, path string, response *http.Response, body []byte) error {
	if err := requireJSON(response.Header.Get("Content-Type")); err != nil {
		return exchangeError(method, err)
	}
	var value struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Outcome string `json:"outcome"`
	}
	if err := decodeStrict(body, &value); err != nil || value.Code == "" || value.Message == "" || (value.Outcome != "unknown" && value.Outcome != "unavailable" && value.Outcome != "not_committed") {
		return exchangeError(method, errors.New("invalid warehouse error response"))
	}
	var cause error
	switch value.Code {
	case "InvalidArgument":
		if response.StatusCode != http.StatusBadRequest || (value.Outcome != "not_committed" && value.Outcome != "unavailable") {
			return exchangeError(method, errors.New("invalid InvalidArgument outcome"))
		}
		cause = contract.ErrInvalidArgument
	case "InventoryUnavailable":
		validOutcome := false
		switch {
		case method == http.MethodGet, method == http.MethodPost && path == "/order-evaluations":
			validOutcome = value.Outcome == "unavailable"
		case method == http.MethodPost && path == "/order-commits":
			validOutcome = value.Outcome == "not_committed" || value.Outcome == "unknown"
		case method == http.MethodPost:
			validOutcome = value.Outcome == "unknown"
		}
		if response.StatusCode != http.StatusServiceUnavailable || !validOutcome {
			return exchangeError(method, errors.New("invalid InventoryUnavailable outcome"))
		}
		cause = contract.ErrInventoryUnavailable
	case "CommandIdentityConflict":
		if response.StatusCode != http.StatusConflict || value.Outcome != "not_committed" {
			return exchangeError(method, errors.New("invalid CommandIdentityConflict outcome"))
		}
		cause = contract.ErrCommandIdentityConflict
	case "ConcurrentModification":
		if response.StatusCode != http.StatusConflict || (value.Outcome != "not_committed" && value.Outcome != "unavailable") {
			return exchangeError(method, errors.New("invalid ConcurrentModification outcome"))
		}
		cause = contract.ErrConcurrentModification
	case "ProposalNotFound":
		if response.StatusCode != http.StatusNotFound || (value.Outcome != "unknown" && value.Outcome != "unavailable") {
			return exchangeError(method, errors.New("invalid ProposalNotFound outcome"))
		}
		cause = contract.ErrProposalNotFound
	case "ProposalSuperseded":
		if response.StatusCode != http.StatusConflict || value.Outcome != "not_committed" {
			return exchangeError(method, errors.New("invalid ProposalSuperseded outcome"))
		}
		cause = contract.ErrProposalSuperseded
	case "ConfirmationPending":
		if response.StatusCode != http.StatusAccepted || value.Outcome != "unknown" {
			return exchangeError(method, errors.New("invalid ConfirmationPending outcome"))
		}
		cause = contract.ErrConfirmationPending
	case "DishNotFound":
		if response.StatusCode != http.StatusNotFound || (value.Outcome != "not_committed" && value.Outcome != "unavailable") {
			return exchangeError(method, errors.New("invalid DishNotFound outcome"))
		}
		cause = contract.ErrDishNotFound
	case "ProductNotFound":
		if response.StatusCode != http.StatusNotFound || (value.Outcome != "not_committed" && value.Outcome != "unavailable") {
			return exchangeError(method, errors.New("invalid ProductNotFound outcome"))
		}
		cause = contract.ErrProductNotFound
	case "ProductAlreadyExists", "DishAlreadyExists", "DeliveryAlreadyReceived", "InsufficientInventory":
		if response.StatusCode != http.StatusConflict || value.Outcome != "not_committed" {
			return exchangeError(method, errors.New("invalid conflict error response"))
		}
	case "InternalError":
		if response.StatusCode != http.StatusInternalServerError || value.Outcome != "unknown" {
			return exchangeError(method, errors.New("invalid InternalError response"))
		}
		return &contract.OperationError{Code: value.Code, Message: value.Message, Outcome: value.Outcome}
	default:
		return exchangeError(method, errors.New("unknown warehouse error code"))
	}
	return &contract.OperationError{Code: value.Code, Message: value.Message, Outcome: value.Outcome, Cause: cause}
}

func samePositions(left, right []contract.AcceptedPosition) bool {
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

func protocolError(err error) error {
	return &contract.OperationError{Code: "ProtocolError", Message: "warehouse returned an invalid response", Outcome: "unknown", Cause: err}
}
func exchangeError(method string, err error) error {
	code, message, outcome := "WarehouseUnavailable", "warehouse outcome is unknown", "unknown"
	var networkError net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &networkError) && networkError.Timeout()) {
		code, message = "UpstreamTimeout", "warehouse request timed out"
	}
	if method == http.MethodGet {
		outcome = "unavailable"
	}
	return &contract.OperationError{Code: code, Message: message, Outcome: outcome, Cause: err}
}

func commitResultFromWire(value wireCommitResult, parseTime func(string) (time.Time, error)) (contract.CommitResult, error) {
	result := contract.CommitResult{Kind: value.Kind}
	switch value.Kind {
	case contract.OrderCommitted:
		if value.Order == nil || value.Evaluation != nil {
			return contract.CommitResult{}, errors.New("OrderCommitted requires only order")
		}
		order, err := orderFromWire(*value.Order, parseTime)
		if err != nil {
			return contract.CommitResult{}, err
		}
		result.Order = &order
	case contract.RecheckRequired:
		if value.Evaluation == nil || value.Order != nil {
			return contract.CommitResult{}, errors.New("RecheckRequired requires only evaluation")
		}
		evaluation, err := evaluationFromWire(*value.Evaluation, parseTime)
		if err != nil {
			return contract.CommitResult{}, err
		}
		result.OrderEvaluation = &evaluation
	case contract.CommitPending:
		if value.Order != nil || value.Evaluation != nil {
			return contract.CommitResult{}, errors.New("pending has an inactive payload")
		}
	default:
		return contract.CommitResult{}, fmt.Errorf("unknown commit result kind %q", value.Kind)
	}
	return result, nil
}

func orderFromWire(value wireOrder, parseTime func(string) (time.Time, error)) (contract.Order, error) {
	placedAt, err := parseTime(value.PlacedAt)
	if err != nil || value.OrderID == "" || value.ProposalID == "" || value.AcceptedPositions == nil || value.Allocations == nil {
		return contract.Order{}, errors.New("invalid order payload")
	}
	order := contract.Order{OrderID: value.OrderID, ProposalID: value.ProposalID, PlacedAt: placedAt, Positions: make([]contract.AcceptedPosition, len(value.AcceptedPositions)), Allocations: make([]contract.Allocation, len(value.Allocations))}
	for index, position := range value.AcceptedPositions {
		order.Positions[index], err = acceptedPositionFromWire(position)
		if err != nil {
			return contract.Order{}, err
		}
	}
	for index, allocation := range value.Allocations {
		quantity, parseErr := parseInt64(allocation.Quantity, true)
		if parseErr != nil || allocation.ProductID == "" || allocation.BatchID == "" {
			return contract.Order{}, errors.New("invalid allocation")
		}
		order.Allocations[index] = contract.Allocation{ProductID: allocation.ProductID, BatchID: allocation.BatchID, Quantity: quantity}
	}
	return order, nil
}

func commandResultFromWire(value wireCommandResult, parseTime func(string) (time.Time, error)) (contract.CommandResult, error) {
	result := contract.CommandResult{Kind: value.Kind}
	active := 0
	switch value.Kind {
	case contract.ProductRegistered:
		if value.Product != nil {
			active++
			product, err := productFromWire(*value.Product)
			if err != nil {
				return result, err
			}
			result.Product = &product
		}
	case contract.DishDefined:
		if value.Dish != nil {
			active++
			if value.Dish.DishID == "" || value.Dish.Name == "" || value.Dish.Ingredients == nil {
				return result, errors.New("invalid recorded dish")
			}
			dish := contract.Dish{DishID: value.Dish.DishID, Name: value.Dish.Name, Recipe: make([]contract.Ingredient, len(value.Dish.Ingredients))}
			for index, ingredient := range value.Dish.Ingredients {
				quantity, err := parseInt64(ingredient.Quantity, true)
				if err != nil || ingredient.ProductID == "" {
					return result, errors.New("invalid recorded dish ingredient")
				}
				dish.Recipe[index] = contract.Ingredient{ProductID: ingredient.ProductID, Quantity: quantity}
			}
			result.Dish = &dish
		}
	case contract.DeliveryReceived:
		if value.Delivery != nil {
			active++
			if value.Delivery.DeliveryID == "" || value.Delivery.BatchIDs == nil {
				return result, errors.New("invalid delivery receipt")
			}
			receipt := contract.DeliveryReceipt{DeliveryID: value.Delivery.DeliveryID, BatchIDs: append([]string(nil), value.Delivery.BatchIDs...)}
			result.DeliveryReceipt = &receipt
		}
	case contract.OrderPlaced:
		if value.Order != nil {
			active++
			order, err := orderFromWire(*value.Order, parseTime)
			if err != nil {
				return result, err
			}
			result.Order = &order
		}
	case contract.BatchExpired:
		if value.Expiry != nil {
			active++
			quantity, err := parseInt64(value.Expiry.WrittenOffQuantity, true)
			if err != nil {
				return result, err
			}
			expiresAt, err := parseTime(value.Expiry.ExpiresAt)
			if err != nil {
				return result, err
			}
			processedAt, err := parseTime(value.Expiry.ProcessedAt)
			if err != nil {
				return result, err
			}
			expiry := contract.ExpiryResult{BatchID: value.Expiry.BatchID, ProductID: value.Expiry.ProductID, WrittenOffQuantity: quantity, Reason: value.Expiry.Reason, ExpiresAt: expiresAt, ProcessedAt: processedAt}
			result.ExpiryResult = &expiry
		}
	default:
		return result, fmt.Errorf("unknown recorded command result kind %q", value.Kind)
	}
	if active != 1 {
		return result, errors.New("recorded command must contain one active payload")
	}
	if (value.Kind != contract.ProductRegistered && value.Product != nil) || (value.Kind != contract.DishDefined && value.Dish != nil) || (value.Kind != contract.DeliveryReceived && value.Delivery != nil) || (value.Kind != contract.OrderPlaced && value.Order != nil) || (value.Kind != contract.BatchExpired && value.Expiry != nil) {
		return result, errors.New("recorded command contains inactive payload")
	}
	return result, nil
}
