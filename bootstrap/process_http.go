package bootstrap

import (
	"context"
	"time"

	"github.com/JustIkra/umir-serving/contract"
	"github.com/JustIkra/umir-serving/internal/adapters/authhttp"
	"github.com/JustIkra/umir-serving/internal/adapters/warehousehttp"
)

type ProcessHTTPOptions struct {
	HTTPOptions
	AuthInternalURL      string
	AuthTimeout          time.Duration
	ServingAuthKey       string
	AuthRuntimeKey       string
	GatewayServingKey    string
	ServingWarehouseKey  string
	TZDataVersion        string
	Epoch                string
	RegistryPollInterval time.Duration
}

func NewProcessHTTP(options ProcessHTTPOptions) (*HTTPHandler, error) {
	if options.Now == nil || options.NewID == nil || options.TZDataVersion == "" || !contract.ValidEntityID(options.Epoch) || options.RegistryPollInterval <= 0 || options.RegistryPollInterval > 30*time.Second || options.AuthRuntimeKey == "" || options.GatewayServingKey == "" || options.ServingWarehouseKey == "" || options.WarehouseTimeout <= 0 || options.WarehouseTimeout > 5*time.Second {
		return nil, contract.ErrInvalidArgument
	}
	keys := []string{options.ServingAuthKey, options.AuthRuntimeKey, options.GatewayServingKey, options.ServingWarehouseKey}
	for i, k := range keys {
		if k == "" {
			return nil, contract.ErrInvalidArgument
		}
		for _, previous := range keys[:i] {
			if k == previous {
				return nil, contract.ErrInvalidArgument
			}
		}
	}
	client := options.HTTPClient
	if client == nil {
		client = warehousehttp.NewHTTPClient()
	}
	auth, err := authhttp.New(options.AuthInternalURL, options.ServingAuthKey, client, options.AuthTimeout, options.Now)
	if err != nil {
		return nil, err
	}
	verifier, err := NewJWTVerifier(JWTOptions{Now: options.Now, Keys: auth.SigningKey})
	if err != nil {
		return nil, err
	}
	factory := func(ctx context.Context, tenant string) (WarehousePorts, error) {
		epoch, err := auth.WarehouseEpoch(ctx, tenant, options.Epoch)
		if err != nil {
			return WarehousePorts{}, err
		}
		return NewScopedWarehouseHTTPPorts(ScopedWarehouseHTTPOptions{WarehouseInternalURL: options.WarehouseInternalURL, WarehouseTimeout: options.WarehouseTimeout, HTTPClient: client, TenantID: tenant, Epoch: epoch, ServiceKey: options.ServingWarehouseKey, Verifier: verifier})
	}
	if _, err = warehousehttp.NewClient(options.WarehouseInternalURL, client, options.WarehouseTimeout); err != nil {
		return nil, err
	}
	registry, err := NewRegistry(RegistryOptions{Epoch: options.Epoch, Now: options.Now, NewID: options.NewID, WarehouseFactory: factory})
	if err != nil {
		return nil, err
	}
	handler, err := NewScopedHTTP(ScopedHTTPOptions{Registry: registry, Verifier: verifier, GatewayKey: options.GatewayServingKey, AuthKey: options.AuthRuntimeKey, TZDataVersion: options.TZDataVersion, RequestTimeout: options.RequestTimeout, MaxRequestBytes: options.MaxRequestBytes, Now: options.Now})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer auth.Close()
		delay := time.Second
		for {
			attempt, stop := context.WithTimeout(ctx, 5*time.Second)
			snapshot, err := auth.Registry(attempt)
			if err == nil {
				err = auth.CheckSigningKeys(attempt)
			}
			if err == nil {
				err = registry.Reconcile(attempt, snapshot.Projection())
			}
			if err != nil {
				registry.mu.Lock()
				registry.ready = false
				registry.mu.Unlock()
			}
			stop()
			wait := options.RegistryPollInterval
			if err != nil {
				wait = delay
				if wait > options.RegistryPollInterval {
					wait = options.RegistryPollInterval
				}
				delay *= 2
				if delay > 30*time.Second {
					delay = 30 * time.Second
				}
			} else {
				delay = time.Second
			}
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
	return &HTTPHandler{handler: handler, client: client, stop: cancel, done: done}, nil
}
