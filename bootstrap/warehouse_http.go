package bootstrap

import (
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
}

func NewScopedWarehouseHTTPPorts(options ScopedWarehouseHTTPOptions) (WarehousePorts, error) {
	client, err := warehousehttp.NewScopedClient(options.WarehouseInternalURL, options.HTTPClient, options.WarehouseTimeout, warehousehttp.ScopedOptions{TenantID: options.TenantID, Epoch: options.Epoch, ServiceKey: options.ServiceKey, Verifier: options.Verifier})
	if err != nil {
		return WarehousePorts{}, err
	}
	return WarehousePorts{Orders: client, Attempts: client, Catalog: client, Menu: client, Epoch: options.Epoch}, nil
}
