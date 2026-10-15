package bootstrap

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/JustIkra/umir-serving/internal/adapters/warehousehttp"
)

type ScopedWarehouseHTTPOptions struct {
	WarehouseInternalURL string
	WarehouseTimeout     time.Duration
	HTTPClient           *http.Client
	TenantID             string
	Epoch                string
	ServiceKey           string
	Verifier             TokenVerifier
	Logger               *slog.Logger
}

func NewScopedWarehouseHTTPPorts(options ScopedWarehouseHTTPOptions) (WarehousePorts, error) {
	client, err := warehousehttp.NewScopedClient(options.WarehouseInternalURL, options.HTTPClient, options.WarehouseTimeout, warehousehttp.ScopedOptions{TenantID: options.TenantID, Epoch: options.Epoch, ServiceKey: options.ServiceKey, Verifier: options.Verifier, Logger: options.Logger})
	if err != nil {
		return WarehousePorts{}, err
	}
	return WarehousePorts{Orders: client, Attempts: client, Catalog: client, Menu: client, Recommendations: client, Epoch: options.Epoch}, nil
}
