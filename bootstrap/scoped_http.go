package bootstrap

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/JustIkra/umir-serving/contract"
	"github.com/JustIkra/umir-serving/internal/adapters/httpapi"
)

type JWTOptions = httpapi.JWTOptions
type SigningKeySource = httpapi.SigningKeySource
type TokenVerifier = httpapi.TokenVerifier

func NewJWTVerifier(options JWTOptions) (*httpapi.JWTVerifier, error) {
	return httpapi.NewJWTVerifier(options)
}

type ScopedHTTPOptions struct {
	Registry        *Registry
	Verifier        TokenVerifier
	GatewayKey      string
	AuthKey         string
	TZDataVersion   string
	RequestTimeout  time.Duration
	MaxRequestBytes int64
	Now             func() time.Time
	// Logger receives HTTP response failures. Nil uses the registry logger.
	Logger *slog.Logger
}

type scopedRegistryResolver struct{ registry *Registry }

func (r scopedRegistryResolver) Resolve(ctx context.Context, scope contract.RequestScope) (httpapi.Runtime, contract.RuntimeInfo, error) {
	runtime, info, err := r.registry.Resolve(ctx, scope)
	if err != nil {
		return nil, contract.RuntimeInfo{}, err
	}
	if scope.RuntimeEpoch != "" && scope.RuntimeEpoch != info.Epoch {
		return nil, contract.RuntimeInfo{}, &contract.OperationError{Code: "RuntimeReset", Outcome: "indeterminate"}
	}
	ports, err := r.registry.options.WarehouseFactory(ctx, scope.TenantID)
	if err != nil {
		return nil, contract.RuntimeInfo{}, peerDiscoveryError(scope, err)
	}
	if !contract.ValidEntityID(ports.Epoch) || isNil(ports.Orders) || isNil(ports.Catalog) || isNil(ports.Menu) {
		return nil, contract.RuntimeInfo{}, peerDiscoveryError(scope, contract.ErrInventoryUnavailable)
	}
	info.WarehouseEpoch = ports.Epoch
	return &requestRuntime{Runtime: runtime, ports: ports}, info, nil
}

func peerDiscoveryError(scope contract.RequestScope, err error) error {
	if scope.Operation != "confirmOrder" && scope.Operation != "resolveOrder" {
		return err
	}
	var operation *contract.OperationError
	if errors.As(err, &operation) {
		value := *operation
		if value.Outcome != "indeterminate" {
			value.Outcome = "unknown"
		}
		return &value
	}
	return &contract.OperationError{Code: "InventoryUnavailable", Message: "warehouse attempt outcome is unknown", Outcome: "unknown", Cause: err}
}

func NewScopedHTTP(options ScopedHTTPOptions) (http.Handler, error) {
	if options.Registry == nil {
		return nil, contract.ErrInvalidArgument
	}
	logger := options.Logger
	if logger == nil {
		logger = options.Registry.log
	}
	return httpapi.NewScopedHandler(scopedRegistryResolver{registry: options.Registry}, options.Verifier, httpapi.ScopedOptions{Options: httpapi.Options{RequestTimeout: options.RequestTimeout, MaxRequestBytes: options.MaxRequestBytes, Logger: logger}, GatewayKey: options.GatewayKey, AuthKey: options.AuthKey, TZDataVersion: options.TZDataVersion, Now: options.Now})
}

func (r scopedRegistryResolver) Register(ctx context.Context, request contract.RestaurantRegistration) (contract.RuntimeReadiness, error) {
	if err := r.registry.Register(ctx, request); err != nil {
		return contract.RuntimeReadiness{}, err
	}
	return contract.RuntimeReadiness{RestaurantID: request.TenantID, Service: "serving", Epoch: request.ExpectedEpoch, State: "ready", LifecycleVersion: request.LifecycleVersion}, nil
}
func (r scopedRegistryResolver) Close(ctx context.Context, request contract.RestaurantRegistration) (contract.RuntimeReadiness, error) {
	if err := r.registry.Close(ctx, request); err != nil {
		return contract.RuntimeReadiness{}, err
	}
	return contract.RuntimeReadiness{RestaurantID: request.TenantID, Service: "serving", Epoch: request.ExpectedEpoch, State: "closed", LifecycleVersion: request.LifecycleVersion}, nil
}
func (r scopedRegistryResolver) Status() contract.RuntimeStatus { return r.registry.Status() }
