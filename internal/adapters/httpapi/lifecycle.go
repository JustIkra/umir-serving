package httpapi

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"
	"time"

	"github.com/JustIkra/umir-serving/contract"
)

type lifecycleRegistry interface {
	Register(context.Context, contract.RestaurantRegistration) (contract.RuntimeReadiness, error)
	Close(context.Context, contract.RestaurantRegistration) (contract.RuntimeReadiness, error)
	Status() contract.RuntimeStatus
}

func (h *ScopedHandler) lifecycle(w http.ResponseWriter, r *http.Request, helper *Handler) bool {
	statusRoute := r.URL.Path == "/internal/serving/v2/runtime"
	registryRoute := strings.HasPrefix(r.URL.Path, "/internal/serving/v2/r/") && strings.HasSuffix(r.URL.Path, "/registry")
	if !statusRoute && !registryRoute {
		return false
	}
	fail := func(err error) { helper.writeError(w, err, r.Method != http.MethodGet) }
	registry, ok := h.resolver.(lifecycleRegistry)
	if !ok || h.options.AuthKey == "" {
		fail(&contract.OperationError{Code: "RuntimeNotReady", Outcome: "unavailable"})
		return true
	}
	key := singleHeader(r, "X-Umir-Service-Key")
	isAuth := subtle.ConstantTimeCompare([]byte(key), []byte(h.options.AuthKey)) == 1
	isGateway := subtle.ConstantTimeCompare([]byte(key), []byte(h.options.GatewayKey)) == 1
	if !isAuth && (!statusRoute || !isGateway) {
		fail(authFailure("Unauthenticated", nil))
		return true
	}
	if !helper.requireNoQuery(w, r) {
		return true
	}
	if statusRoute {
		if r.Method != http.MethodGet {
			fail(contract.ErrInvalidArgument)
			return true
		}
		status := registry.Status()
		helper.writeJSON(w, 200, map[string]any{"service": "serving", "epoch": status.Epoch, "ready": status.Ready, "tzdataVersion": h.options.TZDataVersion})
		return true
	}
	tenant := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/internal/serving/v2/r/"), "/registry")
	if !contract.ValidEntityID(tenant) || (r.Method != http.MethodPut && r.Method != http.MethodDelete) {
		fail(contract.ErrInvalidArgument)
		return true
	}
	token, ok := strings.CutPrefix(singleHeader(r, "Authorization"), "Bearer ")
	if !ok || token == "" || strings.ContainsAny(token, " \t\r\n") {
		fail(authFailure("InvalidToken", nil))
		return true
	}
	scope, err := h.verifier.Verify(r.Context(), token)
	if err != nil {
		fail(err)
		return true
	}
	operation := "runtime.register"
	if r.Method == http.MethodDelete {
		operation = "runtime.close"
	}
	if scope.Kind != "service" || scope.Subject != "service:auth" || scope.TenantID != tenant || scope.Operation != operation || !scope.Allows(operation) {
		fail(authFailure("Forbidden", nil))
		return true
	}
	var input struct {
		OperationID      string        `json:"operationID"`
		ExpectedEpoch    string        `json:"expectedEpoch"`
		LifecycleVersion string        `json:"lifecycleVersion"`
		Settings         *settingsWire `json:"settings,omitempty"`
	}
	if !helper.decodeRequest(w, r, &input) {
		return true
	}
	if input.ExpectedEpoch != scope.RuntimeEpoch || input.LifecycleVersion != scope.LifecycleVersion || !contract.ValidEntityID(input.OperationID) {
		fail(contract.ErrInvalidArgument)
		return true
	}
	request := contract.RestaurantRegistration{OrgID: scope.OrgID, TenantID: tenant, OperationID: input.OperationID, ExpectedEpoch: input.ExpectedEpoch, LifecycleVersion: input.LifecycleVersion}
	if operation == "runtime.register" {
		if input.Settings == nil || input.Settings.TimeZone == "" || input.Settings.TimeZone == "Local" || !contract.ValidVersion(input.Settings.SettingsVersion) {
			fail(contract.ErrInvalidArgument)
			return true
		}
		if _, err = time.LoadLocation(input.Settings.TimeZone); err != nil {
			fail(contract.ErrInvalidArgument)
			return true
		}
		request.TimeZone = input.Settings.TimeZone
		request.SettingsVersion = input.Settings.SettingsVersion
	} else if input.Settings != nil {
		fail(contract.ErrInvalidArgument)
		return true
	}
	var result contract.RuntimeReadiness
	if operation == "runtime.register" {
		result, err = registry.Register(r.Context(), request)
	} else {
		result, err = registry.Close(r.Context(), request)
	}
	if err != nil {
		fail(err)
		return true
	}
	helper.writeJSON(w, 200, map[string]string{"restaurantID": result.RestaurantID, "service": result.Service, "epoch": result.Epoch, "state": result.State, "lifecycleVersion": result.LifecycleVersion})
	return true
}
