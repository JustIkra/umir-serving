package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/JustIkra/umir-serving/contract"
	"github.com/JustIkra/umir-serving/internal/adapters/httpjson"
)

const defaultMaxRequestBytes = 1 << 20
const errorFlushBudget = 250 * time.Millisecond

type Runtime interface {
	GetProducts(context.Context) (contract.Products, error)
	GetDishCatalog(context.Context) (contract.DishCatalog, error)
	GetDish(context.Context, string) (contract.DishDetails, error)
	RegisterProduct(context.Context, contract.RegisterProductRequest) (contract.Product, error)
	DefineDish(context.Context, contract.DefineDishRequest) (contract.Dish, error)
	GetMenu(context.Context) (contract.Menu, error)
	GetStopList(context.Context) (contract.StopList, error)
	GetRecommendations(context.Context) (contract.Recommendations, error)
	PrepareOrder(context.Context, contract.PrepareOrderRequest) (contract.OrderProposal, error)
	ConfirmOrder(context.Context, contract.ConfirmOrderRequest) (contract.ConfirmResult, error)
	ResolveConfirmation(context.Context, contract.ConfirmOrderRequest) (contract.ConfirmResult, error)
}

type Options struct {
	RequestTimeout  time.Duration
	MaxRequestBytes int64
	// Logger receives http.response_failed records. Nil discards them.
	Logger *slog.Logger
}

type Handler struct {
	scopedErrors   bool
	scopedResponse *scopedResponse
	runtime        Runtime
	options        Options
	mux            *http.ServeMux
}

func NewHandler(runtime Runtime, options Options) (*Handler, error) {
	if options.MaxRequestBytes == 0 {
		options.MaxRequestBytes = defaultMaxRequestBytes
	}
	if isNil(runtime) || options.RequestTimeout <= 0 || options.MaxRequestBytes < 0 {
		return nil, contract.ErrInvalidArgument
	}
	handler := &Handler{runtime: runtime, options: options, mux: http.NewServeMux()}
	handler.routes()
	return handler, nil
}

func isNil(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	}
	return false
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	deadline := time.Now().Add(h.options.RequestTimeout)
	if existing, ok := r.Context().Deadline(); ok && existing.Before(deadline) {
		deadline = existing
	}
	controller := http.NewResponseController(w)
	if err := controller.SetWriteDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
		h.responseFailed(slog.LevelWarn, "set_write_deadline", err)
	}
	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	defer cancel()
	h.mux.ServeHTTP(w, r.WithContext(ctx))
}

func (h *Handler) routes() {
	h.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if !h.requireNoQuery(w, r) {
			return
		}
		h.writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": "umir-serving"})
	})
	h.mux.HandleFunc("GET /products", h.getProducts)
	h.mux.HandleFunc("POST /products", h.registerProduct)
	h.mux.HandleFunc("GET /dish-catalog", h.getDishCatalog)
	h.mux.HandleFunc("GET /dish", h.getDish)
	h.mux.HandleFunc("POST /dishes", h.defineDish)
	h.mux.HandleFunc("GET /menu", h.getMenu)
	h.mux.HandleFunc("GET /stop-list", h.getStopList)
	h.mux.HandleFunc("GET /recommendations", h.getRecommendations)
	h.mux.HandleFunc("POST /order-proposals", h.prepareOrder)
	h.mux.HandleFunc("POST /order-confirmations", h.confirmOrder)
	h.mux.HandleFunc("POST /order-confirmations/resolve", h.resolveConfirmation)
}

func (h *Handler) getProducts(w http.ResponseWriter, r *http.Request) {
	if !h.requireNoQuery(w, r) {
		return
	}
	value, err := h.runtime.GetProducts(r.Context())
	if err != nil {
		h.writeError(w, err, false)
		return
	}
	response := productsWire{EvaluatedAt: formatTime(value.EvaluatedAt), StateVersion: strconv.FormatInt(value.StateVersion, 10), Items: make([]productWire, len(value.Items))}
	for index, item := range value.Items {
		response.Items[index] = productToWire(item)
	}
	h.writeJSON(w, http.StatusOK, response)
}

func (h *Handler) registerProduct(w http.ResponseWriter, r *http.Request) {
	var input registerProductInput
	if !h.decodeRequest(w, r, &input) {
		return
	}
	value, err := h.runtime.RegisterProduct(r.Context(), contract.RegisterProductRequest{CommandID: input.CommandID, ProductID: input.ProductID, Name: input.Name, BaseUnit: input.BaseUnit})
	if err != nil {
		h.writeError(w, err, true)
		return
	}
	h.writeJSON(w, http.StatusOK, productToWire(value))
}

func (h *Handler) getDishCatalog(w http.ResponseWriter, r *http.Request) {
	if !h.requireNoQuery(w, r) {
		return
	}
	value, err := h.runtime.GetDishCatalog(r.Context())
	if err != nil {
		h.writeError(w, err, false)
		return
	}
	response := dishCatalogWire{EvaluatedAt: formatTime(value.EvaluatedAt), StateVersion: strconv.FormatInt(value.StateVersion, 10), Items: make([]dishWire, len(value.Items))}
	for index, item := range value.Items {
		response.Items[index] = dishToWire(item)
	}
	h.writeJSON(w, http.StatusOK, response)
}

func (h *Handler) getDish(w http.ResponseWriter, r *http.Request) {
	dishID, err := parseDishQuery(r.URL.RawQuery, r.URL.ForceQuery)
	if err != nil {
		h.writeError(w, contract.ErrInvalidArgument, false)
		return
	}
	value, err := h.runtime.GetDish(r.Context(), dishID)
	if err != nil {
		h.writeError(w, err, false)
		return
	}
	h.writeJSON(w, http.StatusOK, dishDetailsWire{EvaluatedAt: formatTime(value.EvaluatedAt), StateVersion: strconv.FormatInt(value.StateVersion, 10), Dish: dishToWire(value.Dish)})
}

func (h *Handler) defineDish(w http.ResponseWriter, r *http.Request) {
	var input defineDishInput
	if !h.decodeRequest(w, r, &input) {
		return
	}
	if input.Ingredients == nil {
		h.writeError(w, contract.ErrInvalidArgument, true)
		return
	}
	recipe := make([]contract.Ingredient, len(input.Ingredients))
	for index, ingredient := range input.Ingredients {
		quantity, err := parsePositiveInt64(ingredient.Quantity)
		if err != nil {
			h.writeError(w, contract.ErrInvalidArgument, true)
			return
		}
		baseUnit := ""
		if ingredient.BaseUnit != nil {
			switch *ingredient.BaseUnit {
			case "g", "ml", "pcs":
				baseUnit = *ingredient.BaseUnit
			default:
				h.writeError(w, contract.ErrInvalidArgument, true)
				return
			}
		}
		recipe[index] = contract.Ingredient{ProductID: ingredient.ProductID, Quantity: quantity, BaseUnit: baseUnit}
	}
	value, err := h.runtime.DefineDish(r.Context(), contract.DefineDishRequest{CommandID: input.CommandID, DishID: input.DishID, Name: input.Name, Recipe: recipe})
	if err != nil {
		h.writeError(w, err, true)
		return
	}
	h.writeJSON(w, http.StatusOK, dishToWire(value))
}

func (h *Handler) getMenu(w http.ResponseWriter, r *http.Request) {
	if !h.requireNoQuery(w, r) {
		return
	}
	value, err := h.runtime.GetMenu(r.Context())
	if err != nil {
		h.writeError(w, err, false)
		return
	}
	response := menuWire{EvaluatedAt: formatTime(value.EvaluatedAt), StateVersion: strconv.FormatInt(value.StateVersion, 10), Dishes: make([]menuDishWire, len(value.Dishes))}
	for index, item := range value.Dishes {
		response.Dishes[index] = menuDishWire{DishID: item.DishID, Name: item.Name}
	}
	h.writeJSON(w, http.StatusOK, response)
}

func (h *Handler) getStopList(w http.ResponseWriter, r *http.Request) {
	if !h.requireNoQuery(w, r) {
		return
	}
	value, err := h.runtime.GetStopList(r.Context())
	if err != nil {
		h.writeError(w, err, false)
		return
	}
	response := stopListWire{EvaluatedAt: formatTime(value.EvaluatedAt), StateVersion: strconv.FormatInt(value.StateVersion, 10), Dishes: make([]stopDishWire, len(value.Dishes))}
	for index, item := range value.Dishes {
		response.Dishes[index] = stopDishWire{DishID: item.DishID, Name: item.Name, Deficits: deficitsToWire(item.Deficits)}
	}
	h.writeJSON(w, http.StatusOK, response)
}

func (h *Handler) getRecommendations(w http.ResponseWriter, r *http.Request) {
	if !h.requireNoQuery(w, r) {
		return
	}
	value, err := h.runtime.GetRecommendations(r.Context())
	if err != nil {
		h.writeError(w, err, false)
		return
	}
	response := recommendationsWire{EvaluatedAt: formatTime(value.EvaluatedAt), StateVersion: strconv.FormatInt(value.StateVersion, 10), PolicyVersion: value.PolicyVersion, HorizonHours: value.HorizonHours, Items: make([]recommendationWire, len(value.Items))}
	if value.PolicyVersion == "FIN1" {
		response.Window = &planningWindowWire{formatTime(value.Window.From), formatTime(value.Window.To), value.Window.Days}
		response.Coverage = &historyCoverageWire{value.Coverage.Epoch, formatTime(value.Coverage.CoverageFrom), value.Coverage.Complete, formatTime(value.Coverage.RequestedFrom), formatTime(value.Coverage.AvailableFrom)}
	}
	for index, item := range value.Items {
		reasons := make([]recommendationReasonWire, len(item.Reasons))
		for reasonIndex, reason := range item.Reasons {
			reasons[reasonIndex] = recommendationReasonWire{ProductID: reason.ProductID, ProductName: reason.ProductName, BatchID: reason.BatchID, QuantityPerPortion: strconv.FormatInt(reason.QuantityPerPortion, 10), BaseUnit: reason.BaseUnit, ExpiresAt: formatTime(reason.ExpiresAt)}
			if value.PolicyVersion == "FIN1" {
				reasons[reasonIndex].RiskQuantity = strconv.FormatInt(reason.RiskQuantity, 10)
				reasons[reasonIndex].TargetPortions = strconv.FormatInt(reason.TargetPortions, 10)
				reasons[reasonIndex].RemainingSeconds = strconv.FormatInt(reason.RemainingSeconds, 10)
			}
		}
		response.Items[index] = recommendationWire{DishID: item.DishID, DishName: item.DishName, Score: item.Score, DemandBasis: item.DemandBasis, PriorityExpiresAt: formatTime(item.PriorityExpiresAt), Reasons: reasons}
	}
	limit := 0
	if value.PolicyVersion == "FIN1" {
		limit = 16 << 20
	}
	h.writeJSONLimited(w, http.StatusOK, response, limit)
}

func (h *Handler) prepareOrder(w http.ResponseWriter, r *http.Request) {
	var input prepareOrderInput
	if !h.decodeRequest(w, r, &input) {
		return
	}
	if input.Positions == nil {
		h.writeError(w, contract.ErrInvalidArgument, true)
		return
	}
	positions := make([]contract.Position, len(input.Positions))
	for index, item := range input.Positions {
		portions, err := parsePositiveInt64(item.Portions)
		if err != nil || item.DishID == "" {
			h.writeError(w, contract.ErrInvalidArgument, true)
			return
		}
		positions[index] = contract.Position{DishID: item.DishID, Portions: portions}
	}
	value, err := h.runtime.PrepareOrder(r.Context(), contract.PrepareOrderRequest{Positions: positions})
	if err != nil {
		h.writeError(w, err, true)
		return
	}
	h.writeJSON(w, http.StatusOK, proposalToWire(value))
}

func (h *Handler) confirmOrder(w http.ResponseWriter, r *http.Request)        { h.confirm(w, r, false) }
func (h *Handler) resolveConfirmation(w http.ResponseWriter, r *http.Request) { h.confirm(w, r, true) }

func (h *Handler) confirm(w http.ResponseWriter, r *http.Request, resolve bool) {
	var input confirmInput
	decode := h.decodeRequest
	if resolve {
		decode = h.decodeResolveRequest
	}
	if !decode(w, r, &input) {
		return
	}
	if input.CommandID == "" || input.ProposalID == "" {
		if resolve {
			h.writeErrorWithInvalidArgumentOutcome(w, contract.ErrInvalidArgument, true, "unknown")
		} else {
			h.writeError(w, contract.ErrInvalidArgument, true)
		}
		return
	}
	request := contract.ConfirmOrderRequest{CommandID: input.CommandID, ProposalID: input.ProposalID}
	var value contract.ConfirmResult
	var err error
	if resolve {
		value, err = h.runtime.ResolveConfirmation(r.Context(), request)
	} else {
		value, err = h.runtime.ConfirmOrder(r.Context(), request)
	}
	if err != nil {
		if resolve {
			h.writeErrorWithInvalidArgumentOutcome(w, err, true, "unknown")
		} else {
			h.writeError(w, err, true)
		}
		return
	}
	response, err := confirmToWire(value)
	if err != nil {
		if resolve {
			h.writeErrorWithInvalidArgumentOutcome(w, err, true, "unknown")
		} else {
			h.writeError(w, err, true)
		}
		return
	}
	status := http.StatusOK
	if value.Kind == contract.ConfirmationPending && !h.scopedErrors {
		status = http.StatusAccepted
	}
	h.writeJSON(w, status, response)
}

func (h *Handler) requireNoQuery(w http.ResponseWriter, r *http.Request) bool {
	return h.requireNoQueryWithOutcome(w, r, "unavailable")
}

func (h *Handler) requireNoQueryWithOutcome(w http.ResponseWriter, r *http.Request, invalidArgumentOutcome string) bool {
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		h.writeErrorWithInvalidArgumentOutcome(w, contract.ErrInvalidArgument, invalidArgumentOutcome == "not_committed", invalidArgumentOutcome)
		return false
	}
	return true
}

func parseDishQuery(raw string, force bool) (string, error) {
	if force || raw == "" || strings.Contains(raw, "&") {
		return "", contract.ErrInvalidArgument
	}
	key, value, found := strings.Cut(raw, "=")
	if !found || key != "dishID" {
		return "", contract.ErrInvalidArgument
	}
	query, err := url.ParseQuery(raw)
	if err != nil || len(query) != 1 || len(query["dishID"]) != 1 {
		return "", contract.ErrInvalidArgument
	}
	dishID, err := url.QueryUnescape(value)
	if err != nil || dishID == "" || !utf8.ValidString(dishID) {
		return "", contract.ErrInvalidArgument
	}
	return dishID, nil
}

func (h *Handler) decodeRequest(w http.ResponseWriter, r *http.Request, target any) bool {
	return h.decodeRequestWithInvalidArgumentOutcome(w, r, target, "not_committed")
}

func (h *Handler) decodeResolveRequest(w http.ResponseWriter, r *http.Request, target any) bool {
	return h.decodeRequestWithInvalidArgumentOutcome(w, r, target, "unknown")
}

func (h *Handler) decodeRequestWithInvalidArgumentOutcome(w http.ResponseWriter, r *http.Request, target any, invalidArgumentOutcome string) bool {
	if !h.requireNoQueryWithOutcome(w, r, invalidArgumentOutcome) {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		h.writeErrorWithInvalidArgumentOutcome(w, contract.ErrInvalidArgument, invalidArgumentOutcome == "not_committed", invalidArgumentOutcome)
		return false
	}
	controller := http.NewResponseController(w)
	deadline, ok := r.Context().Deadline()
	if ok {
		if err := controller.SetReadDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
			h.writeErrorWithInvalidArgumentOutcome(w, contract.ErrInvalidArgument, invalidArgumentOutcome == "not_committed", invalidArgumentOutcome)
			return false
		}
	}
	body, readErr := io.ReadAll(http.MaxBytesReader(w, r.Body, h.options.MaxRequestBytes))
	if err := r.Body.Close(); readErr == nil && err != nil {
		readErr = err
	}
	if ok {
		if err := controller.SetReadDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
			h.responseFailed(slog.LevelWarn, "clear_read_deadline", err)
		}
	}
	if readErr != nil || int64(len(body)) > h.options.MaxRequestBytes {
		h.writeErrorWithInvalidArgumentOutcome(w, contract.ErrInvalidArgument, invalidArgumentOutcome == "not_committed", invalidArgumentOutcome)
		return false
	}
	if err := httpjson.DecodeStrict(body, target); err != nil {
		h.writeErrorWithInvalidArgumentOutcome(w, contract.ErrInvalidArgument, invalidArgumentOutcome == "not_committed", invalidArgumentOutcome)
		return false
	}
	return true
}

func (h *Handler) writeError(w http.ResponseWriter, err error, write bool) {
	invalidArgumentOutcome := "unavailable"
	if write {
		invalidArgumentOutcome = "not_committed"
	}
	h.writeErrorWithInvalidArgumentOutcome(w, err, write, invalidArgumentOutcome)
}

func (h *Handler) writeErrorWithInvalidArgumentOutcome(w http.ResponseWriter, err error, write bool, invalidArgumentOutcome string) {
	controller := http.NewResponseController(w)
	if deadlineErr := controller.SetWriteDeadline(time.Now().Add(errorFlushBudget)); deadlineErr != nil && !errors.Is(deadlineErr, http.ErrNotSupported) {
		h.responseFailed(slog.LevelWarn, "set_error_write_deadline", deadlineErr)
	}
	value := errorWire{Code: "TechnicalError", Message: "service request failed", Outcome: "unavailable"}
	status := http.StatusInternalServerError
	var operation *contract.OperationError
	if errors.As(err, &operation) {
		if h.scopedErrors {
			retryable := operation.Retryable
			value.Retryable = &retryable
		}
		value.Code = operation.Code
		if value.Code == "" {
			value.Code = "TechnicalError"
		}
		if value.Code == "CommandIdentityConflict" { // a legacy Warehouse code; the public enum has CommandConflict
			value.Code = "CommandConflict"
		}
		value.Outcome = operation.Outcome
		if operation.Message != "" {
			value.Message = operation.Message
		}
		if value.Code == "WarehouseUnavailable" || value.Code == "ProtocolError" {
			value.Code = "UpstreamUnavailable"
			value.Message = "upstream unavailable or protocol failure"
			value.Outcome = outcomeForError(write)
		}
	} else {
		switch {
		case errors.Is(err, contract.ErrInvalidArgument):
			value.Code = "InvalidArgument"
			value.Message = "request contains invalid fields"
			value.Outcome = invalidArgumentOutcome
		case errors.Is(err, contract.ErrCommandIdentityConflict):
			value.Code = "CommandConflict"
			value.Message = "command identity conflicts with an existing attempt"
			value.Outcome = "not_committed"
		case errors.Is(err, contract.ErrProposalNotFound):
			value.Code = "ProposalNotFound"
			value.Message = "proposal outcome is unknown"
			value.Outcome = "unknown"
		case errors.Is(err, contract.ErrProposalSuperseded):
			value.Code = "ProposalSuperseded"
			value.Message = "proposal is superseded"
			value.Outcome = "not_committed"
		case errors.Is(err, contract.ErrProductNotFound):
			value.Code = "ProductNotFound"
			value.Message = "product was not found"
			value.Outcome = "not_committed"
		case errors.Is(err, contract.ErrDishNotFound):
			value.Code = "DishNotFound"
			value.Message = "dish was not found"
			value.Outcome = "not_committed"
		case errors.Is(err, contract.ErrInventoryUnavailable):
			value.Code = "InventoryUnavailable"
			value.Message = "warehouse is unavailable"
			value.Outcome = outcomeForError(write)
		case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
			value.Code = "UpstreamTimeout"
			value.Message = "request deadline exceeded"
			value.Outcome = outcomeForError(write)
		default:
			value.Outcome = outcomeForError(write)
		}
	}
	switch value.Code {
	case "InvalidArgument", "InvalidRequest":
		status = http.StatusBadRequest
	case "Unauthenticated", "InvalidToken":
		status = http.StatusUnauthorized
	case "Forbidden":
		status = http.StatusForbidden
	case "AuthUnavailable", "RuntimeNotReady", "ServiceUnavailable":
		status = http.StatusServiceUnavailable
	case "RuntimeReset", "CommandConflict", "SettingsChanged", "RefreshRequired", "ProductConflict", "DishConflict":
		status = http.StatusConflict
	case "ProductNotFound", "DishNotFound", "ProposalNotFound", "NotFound":
		status = http.StatusNotFound
	case "ProposalSuperseded", "ProductAlreadyExists", "DishAlreadyExists":
		status = http.StatusConflict
	case "InventoryUnavailable", "CalculationUnavailable", "TimeZoneDataUnavailable":
		status = http.StatusServiceUnavailable
	case "UpstreamUnavailable":
		status = http.StatusBadGateway
	case "UpstreamTimeout":
		status = http.StatusGatewayTimeout
	case "ScopeDeleted":
		status = http.StatusGone
	case "RateLimited", "LimitExceeded":
		status = http.StatusTooManyRequests
	case "ConfirmationPending":
		status = http.StatusAccepted
	}
	h.writeJSON(w, status, value)
}

func outcomeForError(write bool) string {
	if write {
		return "unknown"
	}
	return "unavailable"
}

func (h *Handler) writeJSON(w http.ResponseWriter, status int, value any) {
	h.writeJSONLimited(w, status, value, 0)
}

func (h *Handler) writeJSONLimited(w http.ResponseWriter, status int, value any, limit int) {
	if h.scopedResponse != nil {
		h.scopedResponse.preparationHeaders(w)
	}
	data, err := json.Marshal(value)
	if err != nil {
		h.responseFailed(slog.LevelError, "encode_response", err)
		return
	}
	if h.scopedErrors && status >= 400 {
		var fields map[string]json.RawMessage
		if err = json.Unmarshal(data, &fields); err != nil {
			h.responseFailed(slog.LevelError, "encode_error", err)
			return
		}
		if _, present := fields["retryable"]; !present {
			fields["retryable"] = json.RawMessage("false")
		}
		data, err = json.Marshal(fields)
		if err != nil {
			h.responseFailed(slog.LevelError, "encode_error", err)
			return
		}
	} else if h.scopedResponse != nil {
		data, err = h.scopedResponse.envelope(data)
		if err != nil {
			response := h.scopedResponse
			h.scopedResponse = nil
			outcome := "unavailable"
			if response.write {
				outcome = "unknown"
			}
			h.writeError(w, &contract.OperationError{Code: "RuntimeNotReady", Message: "verified warehouse context is unavailable", Outcome: outcome}, response.write)
			return
		}
	}
	if limit > 0 && len(data)+1 > limit {
		h.writeError(w, &contract.OperationError{Code: "CalculationUnavailable", Message: "Recommendation result exceeds capacity.", Outcome: "unavailable"}, false)
		return
	}
	data = append(data, '\n')
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(data); err != nil {
		h.responseFailed(slog.LevelWarn, "write_response", err)
	}
}
