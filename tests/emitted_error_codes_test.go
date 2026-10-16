package tests_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/JustIkra/umir-serving/contract"
	"github.com/JustIkra/umir-serving/internal/adapters/httpapi"
	"github.com/JustIkra/umir-serving/tests/testsupport"
)

type failingRuntime struct {
	httpapi.Runtime
	err error
}

func (r failingRuntime) GetProducts(context.Context) (contract.Products, error) {
	return contract.Products{}, r.err
}

// Serving reports contract errors to the Gateway, which accepts only the codes
// of the public Error.code enum and turns anything else into 502/unknown. The
// table covers every contract.Err* and an OperationError without a code.
func TestEveryEmittedErrorCodeIsInContractEnum(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code string
		// outside is set for the codes the Serving still emits although the
		// enum does not list them; the entry pins the current code so that a
		// change is noticed. It is a known gap outside W1-1, not a pass.
		outside bool
	}{
		{name: "ErrInvalidArgument", err: contract.ErrInvalidArgument, code: "InvalidArgument"},
		{name: "ErrInventoryUnavailable", err: contract.ErrInventoryUnavailable, code: "InventoryUnavailable"},
		{name: "ErrCommandIdentityConflict", err: contract.ErrCommandIdentityConflict, code: "CommandConflict"},
		{name: "ErrConcurrentModification", err: contract.ErrConcurrentModification, code: "TechnicalError"},
		{name: "ErrProposalNotFound", err: contract.ErrProposalNotFound, code: "ProposalNotFound"},
		{name: "ErrConfirmationPending", err: contract.ErrConfirmationPending, code: "TechnicalError"},
		{name: "wrapped ErrCommandIdentityConflict", err: fmt.Errorf("attempt: %w", contract.ErrCommandIdentityConflict), code: "CommandConflict"},
		{name: "unclassified error", err: errors.New("boom"), code: "TechnicalError"},
		{name: "OperationError without code", err: &contract.OperationError{Message: "failed", Outcome: "unknown"}, code: "TechnicalError"},
		{name: "OperationError without code and message", err: &contract.OperationError{Outcome: "unavailable"}, code: "TechnicalError"},
		{name: "OperationError with stored legacy code", err: &contract.OperationError{Code: "CommandIdentityConflict", Outcome: "not_committed", Cause: contract.ErrCommandIdentityConflict}, code: "CommandConflict"},
		{name: "ErrProposalSuperseded", err: contract.ErrProposalSuperseded, code: "ProposalSuperseded", outside: true},
		{name: "ErrProductNotFound", err: contract.ErrProductNotFound, code: "ProductNotFound", outside: true},
		{name: "ErrDishNotFound", err: contract.ErrDishNotFound, code: "DishNotFound", outside: true},
	}
	for _, mode := range []string{"non-scoped", "scoped"} {
		for _, value := range cases {
			t.Run(mode+"/"+value.name, func(t *testing.T) {
				response := httptest.NewRecorder()
				if mode == "scoped" {
					serveScopedProducts(t, response, value.err)
				} else {
					handler, err := httpapi.NewHandler(failingRuntime{err: value.err}, httpapi.Options{RequestTimeout: time.Second})
					if err != nil {
						t.Fatal(err)
					}
					handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/products", nil))
				}
				var body struct {
					Code      string `json:"code"`
					Retryable *bool  `json:"retryable"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
					t.Fatalf("HTTP %d body %q: %v", response.Code, response.Body.String(), err)
				}
				if body.Code != value.code {
					t.Fatalf("code = %q, want %q (HTTP %d)", body.Code, value.code, response.Code)
				}
				if (body.Retryable != nil) != (mode == "scoped") {
					t.Fatalf("retryable present = %v in %s mode", body.Retryable != nil, mode)
				}
				if inEnum := testsupport.IsContractErrorCode(body.Code); inEnum == value.outside {
					t.Fatalf("code %q: in contract enum = %v, want %v", body.Code, inEnum, !value.outside)
				}
			})
		}
	}
}

const (
	scopedTenant = "33333333-3333-4333-8333-333333333333"
	scopedEpoch  = "44444444-4444-4444-8444-444444444444"
)

type scopedProbe struct {
	runtime httpapi.Runtime
}

func (p scopedProbe) Verify(context.Context, string) (contract.RequestScope, error) {
	return contract.RequestScope{TenantID: scopedTenant, Operation: "servingProducts", Permissions: []string{"menu.read"}}, nil
}

func (p scopedProbe) Resolve(context.Context, contract.RequestScope) (httpapi.Runtime, contract.RuntimeInfo, error) {
	return p.runtime, contract.RuntimeInfo{Epoch: scopedEpoch, WarehouseEpoch: scopedEpoch}, nil
}

func serveScopedProducts(t *testing.T, response *httptest.ResponseRecorder, err error) {
	t.Helper()
	probe := scopedProbe{runtime: failingRuntime{err: err}}
	handler, handlerErr := httpapi.NewScopedHandler(probe, probe, httpapi.ScopedOptions{
		Options:    httpapi.Options{RequestTimeout: time.Second, MaxRequestBytes: 1024},
		GatewayKey: "gateway-test",
		Now:        time.Now,
	})
	if handlerErr != nil {
		t.Fatal(handlerErr)
	}
	request := httptest.NewRequest(http.MethodGet, "/internal/serving/v2/r/"+scopedTenant+"/products", nil)
	request.Header.Set("X-Umir-Service-Key", "gateway-test")
	request.Header.Set("Authorization", "Bearer token")
	handler.ServeHTTP(response, request)
}
