package httpapi

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/JustIkra/umir-serving/contract"
)

type ScopedRuntimeResolver interface {
	Resolve(context.Context, contract.RequestScope) (Runtime, contract.RuntimeInfo, error)
}

type ScopedOptions struct {
	Options
	GatewayKey    string
	AuthKey       string
	TZDataVersion string
	Now           func() time.Time
}

type ScopedHandler struct {
	resolver ScopedRuntimeResolver
	verifier TokenVerifier
	options  ScopedOptions
}

func NewScopedHandler(resolver ScopedRuntimeResolver, verifier TokenVerifier, options ScopedOptions) (*ScopedHandler, error) {
	if isNil(resolver) || isNil(verifier) || options.GatewayKey == "" || options.Now == nil || options.RequestTimeout <= 0 || options.MaxRequestBytes <= 0 || options.MaxRequestBytes > defaultMaxRequestBytes {
		return nil, contract.ErrInvalidArgument
	}
	if options.AuthKey != "" && (options.AuthKey == options.GatewayKey || options.TZDataVersion == "") {
		return nil, contract.ErrInvalidArgument
	}
	return &ScopedHandler{resolver: resolver, verifier: verifier, options: options}, nil
}

type scopedRoute struct {
	method, path, operation, permission string
	epoch                               bool
}

var scopedRoutes = []scopedRoute{
	{"GET", "products", "servingProducts", "menu.read", false},
	{"GET", "dish-catalog", "servingDishes", "menu.read", false},
	{"GET", "dish", "servingDish", "menu.read", false},
	{"GET", "menu", "menu", "menu.read", false},
	{"GET", "stop-list", "stopList", "menu.read", false},
	{"GET", "recommendations", "recommendations", "recommendations.read", false},
	{"POST", "order-proposals", "prepareOrder", "order.prepare", true},
	{"POST", "order-confirmations", "confirmOrder", "order.confirm", true},
	{"GET", "order-confirmations/resolve", "resolveOrder", "order.read", true},
}

func hasBrowserIdentityHeaders(r *http.Request) bool {
	for name := range r.Header {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "x-umir-") && lower != "x-umir-service-key" && lower != "x-umir-runtime-epoch" {
			return true
		}
		switch lower {
		case "x-user-id", "x-tenant-id", "x-org-id", "x-role", "x-roles", "x-permissions", "x-caller-id", "x-forwarded-user", "cookie":
			return true
		}
	}
	return false
}

func singleHeader(r *http.Request, name string) string {
	values := r.Header.Values(name)
	if len(values) != 1 {
		return ""
	}
	return values[0]
}

func (h *ScopedHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	deadline := time.Now().Add(h.options.RequestTimeout)
	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	defer cancel()
	r = r.WithContext(ctx)
	helper := &Handler{options: h.options.Options, scopedErrors: true}
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
		if helper.requireNoQuery(w, r) {
			helper.writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": "umir-serving"})
		}
		return
	}
	fail := func(err error) {
		outcome := "unavailable"
		if r.Method != http.MethodGet {
			outcome = "not_committed"
		}
		var operation *contract.OperationError
		if errors.As(err, &operation) {
			if operation.Outcome == "indeterminate" || operation.Outcome == "unknown" {
				outcome = operation.Outcome
			}
			value := *operation
			value.Outcome = outcome
			err = &value
		}
		helper.writeError(w, err, r.Method != http.MethodGet)
	}
	if hasBrowserIdentityHeaders(r) {
		fail(contract.ErrInvalidArgument)
		return
	}
	if h.lifecycle(w, r, helper) {
		return
	}
	key := singleHeader(r, "X-Umir-Service-Key")
	if key == "" || subtle.ConstantTimeCompare([]byte(key), []byte(h.options.GatewayKey)) != 1 {
		fail(authFailure("Unauthenticated", nil))
		return
	}
	prefix := "/internal/serving/v2/r/"
	rest, ok := strings.CutPrefix(r.URL.Path, prefix)
	if !ok {
		fail(contract.ErrInvalidArgument)
		return
	}
	tenant, path, ok := strings.Cut(rest, "/")
	if !ok || !contract.ValidEntityID(tenant) {
		fail(contract.ErrInvalidArgument)
		return
	}
	var route scopedRoute
	for _, candidate := range scopedRoutes {
		if path == candidate.path && r.Method == candidate.method {
			route = candidate
			break
		}
	}
	if route.path == "" {
		fail(contract.ErrInvalidArgument)
		return
	}
	bearer := singleHeader(r, "Authorization")
	token, ok := strings.CutPrefix(bearer, "Bearer ")
	if !ok || token == "" || strings.ContainsAny(token, " \t\r\n") {
		fail(authFailure("InvalidToken", nil))
		return
	}
	scope, err := h.verifier.Verify(ctx, token)
	if err != nil {
		fail(err)
		return
	}
	if scope.TenantID != tenant || scope.Operation != route.operation || !scope.Allows(route.permission) {
		fail(authFailure("Forbidden", nil))
		return
	}
	ctx = contract.WithRequestScope(ctx, scope)
	var epoch string
	var resolveInput confirmInput
	if route.operation == "resolveOrder" {
		query, parseErr := parseResolveQuery(r)
		if parseErr != nil {
			fail(parseErr)
			return
		}
		epoch = query.epoch
		resolveInput = query.input
	} else if route.epoch {
		epoch = singleHeader(r, "X-Umir-Runtime-Epoch")
		if !contract.ValidEntityID(epoch) {
			fail(contract.ErrInvalidArgument)
			return
		}
	}
	scope.RuntimeEpoch = epoch
	ctx = contract.WithRequestScope(ctx, scope)
	runtime, info, err := h.resolver.Resolve(ctx, scope)
	if err != nil {
		fail(err)
		return
	}
	if route.epoch && epoch != info.Epoch {
		fail(&contract.OperationError{Code: "RuntimeReset", Outcome: "indeterminate"})
		return
	}
	if isNil(runtime) || !contract.ValidEntityID(info.Epoch) || !contract.ValidEntityID(info.WarehouseEpoch) {
		fail(&contract.OperationError{Code: "RuntimeNotReady"})
		return
	}
	legacy, err := NewHandler(runtime, h.options.Options)
	if err != nil {
		fail(err)
		return
	}
	legacy.scopedErrors = true
	observation := &warehouseObservation{}
	legacy.scopedResponse = &scopedResponse{scope: scope, info: info, observation: observation, write: r.Method != http.MethodGet}
	ctx = contract.WithWarehouseBinding(ctx, contract.WarehouseContext{OrgID: scope.OrgID, TenantID: scope.TenantID, Epoch: info.WarehouseEpoch, TimeZone: scope.TimeZone, SettingsVersion: scope.SettingsVersion}, false)
	request := r.Clone(contract.WithWarehouseObserver(ctx, observation))
	request.URL.Path = "/" + route.path
	request.URL.RawPath = ""
	if route.operation == "resolveOrder" {
		body, marshalErr := json.Marshal(resolveInput)
		if marshalErr != nil {
			fail(contract.ErrInvalidArgument)
			return
		}
		request.Method = http.MethodPost
		request.URL.RawQuery = ""
		request.URL.ForceQuery = false
		request.Body = io.NopCloser(bytes.NewReader(body))
		request.ContentLength = int64(len(body))
		request.Header.Set("Content-Type", "application/json")
	}
	legacy.ServeHTTP(w, request)
}

type resolveQuery struct {
	epoch string
	input confirmInput
}

func parseResolveQuery(r *http.Request) (resolveQuery, error) {
	if r.URL.ForceQuery || strings.Contains(r.URL.RawQuery, ";") {
		return resolveQuery{}, contract.ErrInvalidArgument
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(query) != 3 {
		return resolveQuery{}, contract.ErrInvalidArgument
	}
	for _, key := range []string{"commandID", "proposalID", "runtimeEpoch"} {
		if len(query[key]) != 1 || query[key][0] == "" {
			return resolveQuery{}, contract.ErrInvalidArgument
		}
	}
	if !contract.ValidEntityID(query.Get("runtimeEpoch")) {
		return resolveQuery{}, contract.ErrInvalidArgument
	}
	return resolveQuery{epoch: query.Get("runtimeEpoch"), input: confirmInput{CommandID: query.Get("commandID"), ProposalID: query.Get("proposalID")}}, nil
}

type warehouseObservation struct {
	preparation *contract.WarehousePreparation
	mu          sync.Mutex
	value       contract.WarehouseContext
	valid       bool
}

func (o *warehouseObservation) Record(value contract.WarehouseContext) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.value = value
	o.valid = true
}
func (o *warehouseObservation) RecordPreparation(value contract.WarehousePreparation) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.preparation = &value
}
func (o *warehouseObservation) latestPreparation() *contract.WarehousePreparation {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.preparation == nil {
		return nil
	}
	value := *o.preparation
	return &value
}
func (o *warehouseObservation) latest() (contract.WarehouseContext, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.value, o.valid
}

type scopedResponse struct {
	scope       contract.RequestScope
	info        contract.RuntimeInfo
	observation *warehouseObservation
	write       bool
}

func (s *scopedResponse) envelope(data []byte) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	observed, ok := s.observation.latest()
	if !ok || observed.OrgID != s.scope.OrgID || observed.TenantID != s.scope.TenantID || !contract.ValidEntityID(observed.Epoch) || observed.TimeZone == "" || !contract.ValidVersion(observed.SettingsVersion) || observed.EvaluatedAt.IsZero() || observed.StateVersion < 0 {
		return nil, errors.New("verified warehouse context is unavailable")
	}
	evaluatedAt := json.RawMessage(`"` + observed.EvaluatedAt.Format(time.RFC3339Nano) + `"`)
	version := json.RawMessage(`"` + strconv.FormatInt(observed.StateVersion, 10) + `"`)
	if _, ok := fields["proposalID"]; ok {
		value, err := json.Marshal(observed.Epoch)
		if err != nil {
			return nil, err
		}
		fields["warehouseEpoch"] = value
		data, err = json.Marshal(fields)
		if err != nil {
			return nil, err
		}
	}
	return json.Marshal(struct {
		Context servingContext  `json:"context"`
		Result  json.RawMessage `json:"result"`
	}{Context: servingContext{OrgID: s.scope.OrgID, TenantID: s.scope.TenantID, RuntimeEpoch: s.info.Epoch, WarehouseEpoch: observed.Epoch, Settings: settingsWire{TimeZone: observed.TimeZone, SettingsVersion: observed.SettingsVersion}, EvaluatedAt: evaluatedAt, StateVersion: version}, Result: data})
}

type settingsWire struct {
	TimeZone        string `json:"timeZone"`
	SettingsVersion string `json:"settingsVersion"`
}
type servingContext struct {
	OrgID          string          `json:"orgID"`
	TenantID       string          `json:"tenantID"`
	RuntimeEpoch   string          `json:"runtimeEpoch"`
	WarehouseEpoch string          `json:"warehouseEpoch"`
	Settings       settingsWire    `json:"settings"`
	EvaluatedAt    json.RawMessage `json:"evaluatedAt"`
	StateVersion   json.RawMessage `json:"stateVersion"`
}

func (s *scopedResponse) preparationHeaders(w http.ResponseWriter) {
	p := s.observation.latestPreparation()
	if p == nil || p.OrgID != s.scope.OrgID || p.TenantID != s.scope.TenantID || p.Epoch != s.info.WarehouseEpoch || p.CommittedVersion < 0 {
		return
	}
	w.Header().Set("X-Umir-Committed-Version", strconv.FormatInt(p.CommittedVersion, 10))
	w.Header().Set("X-Umir-Invalidation-Required", "true")
	w.Header().Set("X-Umir-Runtime-Epoch", p.Epoch)
}
