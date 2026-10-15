package bootstrap

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/JustIkra/umir-serving/contract"
)

type WarehousePorts struct {
	Orders          contract.OrdersWarehouse
	Attempts        contract.OrderAttemptReader
	Catalog         contract.CatalogWarehouse
	Menu            contract.MenuWarehouse
	Recommendations contract.RecommendationWarehouse
	Epoch           string
}

type WarehouseClientFactory func(context.Context, string) (WarehousePorts, error)

type RegistryOptions struct {
	Epoch            string
	Now              func() time.Time
	NewID            func() string
	WarehouseFactory WarehouseClientFactory
}

type restaurantRuntime struct {
	registration   contract.RestaurantRegistration
	runtime        *Runtime
	warehouseEpoch string
}

type Registry struct {
	mu          sync.RWMutex
	options     RegistryOptions
	restaurants map[string]restaurantRuntime
	closed      map[string]restaurantRuntime
	revision    string
	ready       bool
	registering chan struct{}
}

func NewRegistry(options RegistryOptions) (*Registry, error) {
	if !contract.ValidEntityID(options.Epoch) || options.Now == nil || options.NewID == nil || options.WarehouseFactory == nil {
		return nil, contract.ErrInvalidArgument
	}
	return &Registry{options: options, restaurants: make(map[string]restaurantRuntime), closed: make(map[string]restaurantRuntime), registering: make(chan struct{}, 1)}, nil
}

func (r *Registry) Register(ctx context.Context, registration contract.RestaurantRegistration) error {
	if !contract.ValidEntityID(registration.OrgID) || !contract.ValidEntityID(registration.TenantID) || !contract.ValidEntityID(registration.OperationID) || !contract.ValidEntityID(registration.ExpectedEpoch) || !contract.ValidVersion(registration.LifecycleVersion) {
		return contract.ErrInvalidArgument
	}
	if registration.ExpectedEpoch != r.options.Epoch {
		return &contract.OperationError{Code: "RuntimeReset", Outcome: "indeterminate"}
	}
	select {
	case r.registering <- struct{}{}:
		defer func() { <-r.registering }()
	case <-ctx.Done():
		return ctx.Err()
	}
	r.mu.Lock()
	if err := ctx.Err(); err != nil {
		r.mu.Unlock()
		return err
	}
	exists, err := r.registrationStateLocked(registration)
	r.mu.Unlock()
	if exists || err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ports, err := r.options.WarehouseFactory(ctx, registration.TenantID)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !contract.ValidEntityID(ports.Epoch) || isNil(ports.Orders) || isNil(ports.Catalog) || isNil(ports.Menu) {
		return contract.ErrInvalidArgument
	}
	proxy := &requestWarehouse{fallback: ports}
	var attempts contract.OrderAttemptReader
	if ports.Attempts != nil {
		attempts = proxy
	}
	runtime, err := NewInMemory(Options{Now: r.options.Now, NewID: r.options.NewID, OrdersWarehouse: proxy, OrderAttemptReader: attempts, CatalogWarehouse: proxy, MenuWarehouse: proxy, RecommendationsWarehouse: proxy})
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	exists, err = r.registrationStateLocked(registration)
	if exists || err != nil {
		return err
	}
	runtime.registration = registration
	r.restaurants[registration.TenantID] = restaurantRuntime{registration: registration, runtime: runtime, warehouseEpoch: ports.Epoch}
	return nil
}

func (r *Registry) registrationStateLocked(registration contract.RestaurantRegistration) (bool, error) {
	if _, ok := r.closed[registration.TenantID]; ok {
		return false, &contract.OperationError{Code: "ScopeDeleted", Outcome: "not_committed"}
	}
	if existing, ok := r.restaurants[registration.TenantID]; ok {
		if existing.registration == registration {
			return true, nil
		}
		identity := registration
		identity.TimeZone, identity.SettingsVersion = existing.registration.TimeZone, existing.registration.SettingsVersion
		if identity == existing.registration && contract.ValidVersion(registration.SettingsVersion) && contract.ValidVersion(existing.registration.SettingsVersion) {
			current, currentErr := strconv.ParseInt(existing.registration.SettingsVersion, 10, 64)
			next, nextErr := strconv.ParseInt(registration.SettingsVersion, 10, 64)
			_, zoneErr := time.LoadLocation(registration.TimeZone)
			if currentErr == nil && nextErr == nil && next > current && zoneErr == nil && registration.TimeZone != "" && registration.TimeZone != "Local" {
				existing.registration = registration
				r.restaurants[registration.TenantID] = existing
				return true, nil
			}
		}
		return false, &contract.OperationError{Code: "CommandConflict", Outcome: "not_committed"}
	}
	return false, nil
}

func (r *Registry) Resolve(ctx context.Context, scope contract.RequestScope) (*Runtime, contract.RuntimeInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, contract.RuntimeInfo{}, err
	}
	if !contract.ValidEntityID(scope.TenantID) || !contract.ValidEntityID(scope.OrgID) || !contract.ValidVersion(scope.LifecycleVersion) {
		return nil, contract.RuntimeInfo{}, &contract.OperationError{Code: "Forbidden", Outcome: "unavailable"}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if closed, ok := r.closed[scope.TenantID]; ok {
		if closed.registration.OrgID != scope.OrgID {
			return nil, contract.RuntimeInfo{}, &contract.OperationError{Code: "Forbidden", Outcome: "unavailable"}
		}
		return nil, contract.RuntimeInfo{}, &contract.OperationError{Code: "ScopeDeleted", Outcome: "unavailable"}
	}
	entry, ok := r.restaurants[scope.TenantID]
	if !ok {
		return nil, contract.RuntimeInfo{}, &contract.OperationError{Code: "RuntimeNotReady", Outcome: "unavailable"}
	}
	if entry.registration.OrgID != scope.OrgID || entry.registration.LifecycleVersion != scope.LifecycleVersion {
		return nil, contract.RuntimeInfo{}, &contract.OperationError{Code: "Forbidden", Outcome: "unavailable"}
	}
	return entry.runtime, contract.RuntimeInfo{Epoch: r.options.Epoch, WarehouseEpoch: entry.warehouseEpoch}, nil
}

func (r *Runtime) requireScope(ctx context.Context, permission string) error {
	if r.registration.TenantID == "" {
		return nil
	}
	scope, ok := contract.RequestScopeFromContext(ctx)
	if !ok || scope.OrgID != r.registration.OrgID || scope.TenantID != r.registration.TenantID || scope.LifecycleVersion != r.registration.LifecycleVersion || !scope.Allows(permission) {
		return &contract.OperationError{Code: "Forbidden", Outcome: "unavailable"}
	}
	return nil
}

func (r *Registry) Close(ctx context.Context, request contract.RestaurantRegistration) error {
	if !contract.ValidEntityID(request.OrgID) || !contract.ValidEntityID(request.TenantID) || !contract.ValidEntityID(request.OperationID) || !contract.ValidVersion(request.LifecycleVersion) || !contract.ValidEntityID(request.ExpectedEpoch) {
		return contract.ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if request.ExpectedEpoch != r.options.Epoch {
		return &contract.OperationError{Code: "RuntimeReset", Outcome: "indeterminate"}
	}
	r.mu.Lock()
	if err := ctx.Err(); err != nil {
		r.mu.Unlock()
		return err
	}
	entry, closed := r.closed[request.TenantID]
	if closed {
		if entry.registration != request {
			r.mu.Unlock()
			return &contract.OperationError{Code: "CommandConflict", Outcome: "not_committed"}
		}
	} else {
		entry = r.restaurants[request.TenantID]
		if entry.runtime != nil {
			current, parseErr := strconv.ParseInt(entry.registration.LifecycleVersion, 10, 64)
			if parseErr != nil {
				r.mu.Unlock()
				return contract.ErrInvalidArgument
			}
			next, parseErr := strconv.ParseInt(request.LifecycleVersion, 10, 64)
			if parseErr != nil {
				r.mu.Unlock()
				return contract.ErrInvalidArgument
			}
			if entry.registration.OrgID != request.OrgID || next < current {
				r.mu.Unlock()
				return &contract.OperationError{Code: "Forbidden", Outcome: "not_committed"}
			}
			entry.runtime.fence()
		}
		entry.registration = request
		r.closed[request.TenantID] = entry
		delete(r.restaurants, request.TenantID)
	}
	r.mu.Unlock()
	if entry.runtime != nil {
		return entry.runtime.awaitClose(ctx)
	}
	return nil
}

func (r *Registry) Status() contract.RuntimeStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return contract.RuntimeStatus{Epoch: r.options.Epoch, Ready: r.ready}
}
func (r *Registry) Reconcile(ctx context.Context, snapshot contract.RegistrySnapshot) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ready = false
	if !snapshot.Complete || !contract.ValidVersion(snapshot.Revision) || len(snapshot.Entries) > 10000 || len(snapshot.Tombstones) > 10000 {
		return &contract.OperationError{Code: "RuntimeNotReady", Outcome: "unavailable"}
	}
	if r.revision != "" {
		old, err := strconv.ParseInt(r.revision, 10, 64)
		if err != nil {
			return err
		}
		next, err := strconv.ParseInt(snapshot.Revision, 10, 64)
		if err != nil {
			return err
		}
		if next < old {
			return &contract.OperationError{Code: "RuntimeNotReady", Outcome: "unavailable"}
		}
	}
	entryIDs := make(map[string]bool)
	for _, entry := range snapshot.Entries {
		if entryIDs[entry.TenantID] {
			return contract.ErrInvalidArgument
		}
		entryIDs[entry.TenantID] = true
		if entry.Lifecycle != "registering" && entry.Lifecycle != "ready" && entry.Lifecycle != "deleting" && entry.Lifecycle != "deleted" {
			return contract.ErrInvalidArgument
		}
	}
	seen := make(map[string]contract.RegistryRestaurant)
	tombstones := make(map[string]bool)
	for _, entry := range snapshot.Tombstones {
		if tombstones[entry.TenantID] {
			return contract.ErrInvalidArgument
		}
		tombstones[entry.TenantID] = true
	}
	for _, entry := range append(append([]contract.RegistryRestaurant(nil), snapshot.Entries...), snapshot.Tombstones...) {
		if !contract.ValidEntityID(entry.OrgID) || !contract.ValidEntityID(entry.TenantID) || !contract.ValidVersion(entry.LifecycleVersion) {
			return contract.ErrInvalidArgument
		}
		if previous, ok := seen[entry.TenantID]; ok {
			if previous.OrgID != entry.OrgID || previous.LifecycleVersion != entry.LifecycleVersion || (previous.Lifecycle != "deleting" && previous.Lifecycle != "deleted") {
				return contract.ErrInvalidArgument
			}
			continue
		}
		seen[entry.TenantID] = entry
		local, ok := r.restaurants[entry.TenantID]
		if entry.Lifecycle == "registering" && !ok {
			// Auth lists the restaurant before its registration reaches this
			// process. It is not served until installed (Resolve answers
			// RuntimeNotReady), and it must not close the whole process.
			continue
		}
		if entry.Lifecycle == "ready" || entry.Lifecycle == "registering" {
			if !ok || local.registration.OrgID != entry.OrgID || local.registration.LifecycleVersion != entry.LifecycleVersion {
				return &contract.OperationError{Code: "RuntimeNotReady", Outcome: "unavailable"}
			}
		} else {
			closed, ok := r.closed[entry.TenantID]
			if !ok || closed.registration.OrgID != entry.OrgID || closed.registration.LifecycleVersion != entry.LifecycleVersion {
				return &contract.OperationError{Code: "RuntimeNotReady", Outcome: "unavailable"}
			}
		}
	}
	for tenant := range r.restaurants {
		if _, ok := seen[tenant]; !ok {
			return &contract.OperationError{Code: "RuntimeNotReady", Outcome: "unavailable"}
		}
	}
	r.revision = snapshot.Revision
	r.ready = true
	return nil
}
