package bootstrap

import (
	"context"
	"net/http"
	"time"

	"github.com/JustIkra/umir-serving/contract"
	"github.com/JustIkra/umir-serving/internal/adapters/httpapi"
	"github.com/JustIkra/umir-serving/internal/adapters/warehousehttp"
)

type HTTPOptions struct {
	WarehouseInternalURL string
	WarehouseTimeout     time.Duration
	RequestTimeout       time.Duration
	MaxRequestBytes      int64
	Now                  func() time.Time
	NewID                func() string
	HTTPClient           *http.Client
}

type HTTPHandler struct {
	handler http.Handler
	client  *http.Client
	stop    context.CancelFunc
	done    <-chan struct{}
}

func (h *HTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.handler.ServeHTTP(w, r) }

func (h *HTTPHandler) CloseIdleConnections() {
	if h.stop != nil {
		h.stop()
		<-h.done
	}
	h.client.CloseIdleConnections()
}

func NewHTTP(options HTTPOptions) (*Runtime, *HTTPHandler, error) {
	if options.RequestTimeout <= 0 || options.WarehouseTimeout <= 0 || options.MaxRequestBytes <= 0 || options.Now == nil || options.NewID == nil {
		return nil, nil, contract.ErrInvalidArgument
	}
	httpClient := options.HTTPClient
	if httpClient == nil {
		httpClient = warehousehttp.NewHTTPClient()
	}
	warehouse, err := warehousehttp.NewClient(options.WarehouseInternalURL, httpClient, options.WarehouseTimeout)
	if err != nil {
		return nil, nil, err
	}
	runtime, err := NewInMemory(Options{Now: options.Now, NewID: options.NewID, OrdersWarehouse: warehouse, CatalogWarehouse: warehouse, MenuWarehouse: warehouse, OrderAttemptReader: warehouse})
	if err != nil {
		return nil, nil, err
	}
	handler, err := httpapi.NewHandler(runtime, httpapi.Options{RequestTimeout: options.RequestTimeout, MaxRequestBytes: options.MaxRequestBytes})
	if err != nil {
		return nil, nil, err
	}
	return runtime, &HTTPHandler{handler: handler, client: httpClient}, nil
}
