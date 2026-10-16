package bootstrap

import (
	"context"
	"errors"
	"log/slog"
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
	Logger           *slog.Logger
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
	log         *slog.Logger
	// skipLogged remembers which registering entries not yet installed here
	// were already logged, so a poll loop writes each one once.
	skipLogged map[string]string

	mismatchMu   sync.Mutex
	mismatchSeen map[string]time.Time
}

func NewRegistry(options RegistryOptions) (*Registry, error) {
	if !contract.ValidEntityID(options.Epoch) || options.Now == nil || options.NewID == nil || options.WarehouseFactory == nil {
		return nil, contract.ErrInvalidArgument
	}
	options.Logger = nopLogger(options.Logger)
	return &Registry{options: options, log: options.Logger, restaurants: make(map[string]restaurantRuntime), closed: make(map[string]restaurantRuntime), registering: make(chan struct{}, 1), skipLogged: make(map[string]string), mismatchSeen: make(map[string]time.Time)}, nil
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
	runtime, err := NewInMemory(Options{Now: r.options.Now, NewID: r.options.NewID, OrdersWarehouse: proxy, OrderAttemptReader: attempts, CatalogWarehouse: proxy, MenuWarehouse: proxy, RecommendationsWarehouse: proxy, Logger: r.log})
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
	b := &logBatch{registry: r}
	r.mu.RLock()
	defer func() {
		r.mu.RUnlock()
		b.flush()
	}()
	if closed, ok := r.closed[scope.TenantID]; ok {
		if closed.registration.OrgID != scope.OrgID {
			b.mismatch("resolve", closed.registration, scope.OrgID, scope.LifecycleVersion)
			return nil, contract.RuntimeInfo{}, &contract.OperationError{Code: "Forbidden", Outcome: "unavailable"}
		}
		return nil, contract.RuntimeInfo{}, &contract.OperationError{Code: "ScopeDeleted", Outcome: "unavailable"}
	}
	entry, ok := r.restaurants[scope.TenantID]
	if !ok {
		return nil, contract.RuntimeInfo{}, &contract.OperationError{Code: "RuntimeNotReady", Outcome: "unavailable"}
	}
	if entry.registration.OrgID != scope.OrgID || entry.registration.LifecycleVersion != scope.LifecycleVersion {
		b.mismatch("resolve", entry.registration, scope.OrgID, scope.LifecycleVersion)
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
	b := &logBatch{registry: r}
	err := r.reconcile(ctx, snapshot, b)
	b.flush()
	return err
}

// reconcile never writes a log record itself: records go to b and are written
// after the registry mutex is released.
func (r *Registry) reconcile(ctx context.Context, snapshot contract.RegistrySnapshot, b *logBatch) error {
	if err := ctx.Err(); err != nil {
		if errors.Is(err, context.Canceled) {
			return err // the process is stopping
		}
		return b.failed("context_done", snapshot, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ready = false
	if !snapshot.Complete || !contract.ValidVersion(snapshot.Revision) || len(snapshot.Entries) > 10000 || len(snapshot.Tombstones) > 10000 {
		return b.failed("snapshot_invalid", snapshot, &contract.OperationError{Code: "RuntimeNotReady", Outcome: "unavailable"})
	}
	if r.revision != "" {
		old, err := strconv.ParseInt(r.revision, 10, 64)
		if err != nil {
			return b.failed("revision_invalid", snapshot, err)
		}
		next, err := strconv.ParseInt(snapshot.Revision, 10, 64)
		if err != nil {
			return b.failed("revision_invalid", snapshot, err)
		}
		if next < old {
			return b.failed("revision_rollback", snapshot, &contract.OperationError{Code: "RuntimeNotReady", Outcome: "unavailable"}, slog.String("local_revision", boundedLog(r.revision)))
		}
	}
	entryIDs := make(map[string]bool)
	for _, entry := range snapshot.Entries {
		if entryIDs[entry.TenantID] {
			return b.failed("invalid_entry", snapshot, contract.ErrInvalidArgument)
		}
		entryIDs[entry.TenantID] = true
		if entry.Lifecycle != "registering" && entry.Lifecycle != "ready" && entry.Lifecycle != "deleting" && entry.Lifecycle != "deleted" {
			return b.failed("invalid_entry", snapshot, contract.ErrInvalidArgument)
		}
	}
	seen := make(map[string]contract.RegistryRestaurant)
	tombstones := make(map[string]bool)
	for _, entry := range snapshot.Tombstones {
		if tombstones[entry.TenantID] {
			return b.failed("invalid_entry", snapshot, contract.ErrInvalidArgument)
		}
		tombstones[entry.TenantID] = true
	}
	skipped := make(map[string]bool)
	for _, entry := range append(append([]contract.RegistryRestaurant(nil), snapshot.Entries...), snapshot.Tombstones...) {
		if !contract.ValidEntityID(entry.OrgID) || !contract.ValidEntityID(entry.TenantID) || !contract.ValidVersion(entry.LifecycleVersion) {
			return b.failed("invalid_entry", snapshot, contract.ErrInvalidArgument)
		}
		if previous, ok := seen[entry.TenantID]; ok {
			if previous.OrgID != entry.OrgID || previous.LifecycleVersion != entry.LifecycleVersion || (previous.Lifecycle != "deleting" && previous.Lifecycle != "deleted") {
				return b.failed("invalid_entry", snapshot, contract.ErrInvalidArgument)
			}
			continue
		}
		seen[entry.TenantID] = entry
		local, ok := r.restaurants[entry.TenantID]
		if _, closedLocally := r.closed[entry.TenantID]; entry.Lifecycle == "registering" && !ok && !closedLocally {
			// Auth lists the restaurant before its registration reaches this
			// process. It is not served until installed (Resolve answers
			// RuntimeNotReady), and it must not close the whole process.
			// A restaurant closed locally is different: it stays an error.
			skipped[entry.TenantID] = true
			b.skipped(snapshot, entry)
			continue
		}
		notReady := &contract.OperationError{Code: "RuntimeNotReady", Outcome: "unavailable"}
		if entry.Lifecycle == "ready" || entry.Lifecycle == "registering" {
			switch {
			case !ok && entry.Lifecycle == "registering":
				return b.failed("registering_closed_locally", snapshot, notReady, tenantAttrs(entry)...)
			case !ok:
				return b.failed("ready_not_installed", snapshot, notReady, tenantAttrs(entry)...)
			case local.registration.OrgID != entry.OrgID || local.registration.LifecycleVersion != entry.LifecycleVersion:
				b.mismatch("reconcile", local.registration, entry.OrgID, entry.LifecycleVersion)
				return b.failed("tenant_mismatch", snapshot, notReady, tenantAttrs(entry)...)
			}
		} else {
			closed, ok := r.closed[entry.TenantID]
			switch {
			case !ok:
				return b.failed("tombstone_not_applied", snapshot, notReady, tenantAttrs(entry)...)
			case closed.registration.OrgID != entry.OrgID || closed.registration.LifecycleVersion != entry.LifecycleVersion:
				b.mismatch("reconcile", closed.registration, entry.OrgID, entry.LifecycleVersion)
				return b.failed("tenant_mismatch", snapshot, notReady, tenantAttrs(entry)...)
			}
		}
	}
	for tenant := range r.restaurants {
		if _, ok := seen[tenant]; !ok {
			return b.failed("local_missing_from_snapshot", snapshot, &contract.OperationError{Code: "RuntimeNotReady", Outcome: "unavailable"}, slog.String("tenant_id", tenant))
		}
	}
	for tenant := range r.skipLogged {
		if !skipped[tenant] {
			delete(r.skipLogged, tenant)
		}
	}
	r.revision = snapshot.Revision
	r.ready = true
	return nil
}

func tenantAttrs(entry contract.RegistryRestaurant) []slog.Attr {
	return []slog.Attr{slog.String("tenant_id", entry.TenantID), slog.String("org_id", entry.OrgID), slog.String("lifecycle_version", boundedLog(entry.LifecycleVersion))}
}

type logEntry struct {
	level slog.Level
	event string
	attrs []slog.Attr
}

// logBatch collects records made while a registry mutex is held; flush writes
// them after the caller has released it.
type logBatch struct {
	registry *Registry
	entries  []logEntry
}

func (b *logBatch) add(level slog.Level, event string, attrs ...slog.Attr) {
	b.entries = append(b.entries, logEntry{level: level, event: event, attrs: append([]slog.Attr{slog.String("event", event)}, attrs...)})
}

func (b *logBatch) flush() {
	for _, entry := range b.entries {
		b.registry.log.LogAttrs(context.Background(), entry.level, entry.event, entry.attrs...)
	}
	b.entries = nil
}

// failed records why a reconciliation did not make the registry ready and
// returns err unchanged. Reasons are fixed words; the record carries the error
// code, never its text.
func (b *logBatch) failed(reason string, snapshot contract.RegistrySnapshot, err error, extra ...slog.Attr) error {
	b.add(slog.LevelError, EventReconcileFailed, append([]slog.Attr{slog.String("reason", reason), slog.String("error_code", errorCode(err)), slog.String("revision", boundedLog(snapshot.Revision))}, extra...)...)
	return err
}

// skipped notes, once per entry, that Auth lists a restaurant as registering
// before its registration reached this process. Call it under the registry mutex.
func (b *logBatch) skipped(snapshot contract.RegistrySnapshot, entry contract.RegistryRestaurant) {
	r := b.registry
	key := entry.OrgID + "/" + entry.LifecycleVersion
	if r.skipLogged[entry.TenantID] == key {
		return
	}
	r.skipLogged[entry.TenantID] = key
	b.add(slog.LevelWarn, EventReconcileFailed, append([]slog.Attr{slog.String("reason", "registering_not_installed"), slog.String("revision", boundedLog(snapshot.Revision))}, tenantAttrs(entry)...)...)
}

// mismatch records an organization or lifecycle version that differs from the
// registration this process holds for the restaurant. The same mismatch is
// written at most once per mismatchLogInterval, and the memory of what was
// written is bounded.
func (b *logBatch) mismatch(where string, registered contract.RestaurantRegistration, orgID, lifecycleVersion string) {
	r := b.registry
	reason := "lifecycle_mismatch"
	if registered.OrgID != orgID {
		reason = "org_mismatch"
	}
	if !r.mismatchDue(where + "/" + registered.TenantID + "/" + boundedLog(orgID) + "/" + boundedLog(lifecycleVersion)) {
		return
	}
	b.add(slog.LevelWarn, EventRegistryTenantMismatch,
		slog.String("where", where), slog.String("reason", reason),
		slog.String("tenant_id", registered.TenantID),
		slog.String("registered_org_id", registered.OrgID), slog.String("presented_org_id", boundedLog(orgID)),
		slog.String("registered_lifecycle_version", registered.LifecycleVersion), slog.String("presented_lifecycle_version", boundedLog(lifecycleVersion)))
}

const (
	mismatchLogInterval = time.Minute
	mismatchLogCapacity = 1024
)

func (r *Registry) mismatchDue(key string) bool {
	now := r.options.Now()
	r.mismatchMu.Lock()
	defer r.mismatchMu.Unlock()
	if last, ok := r.mismatchSeen[key]; ok && now.Sub(last) < mismatchLogInterval {
		return false
	}
	if _, ok := r.mismatchSeen[key]; !ok && len(r.mismatchSeen) >= mismatchLogCapacity {
		for old, last := range r.mismatchSeen {
			if now.Sub(last) >= mismatchLogInterval {
				delete(r.mismatchSeen, old)
			}
		}
		if len(r.mismatchSeen) >= mismatchLogCapacity {
			clear(r.mismatchSeen)
		}
	}
	r.mismatchSeen[key] = now
	return true
}

// errorCode is the bounded code of err for log records.
func errorCode(err error) string {
	var operation *contract.OperationError
	switch {
	case errors.As(err, &operation):
		return boundedLog(operation.Code)
	case errors.Is(err, contract.ErrInvalidArgument):
		return "InvalidArgument"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "ContextDone"
	}
	return "Error"
}

func boundedLog(value string) string {
	if len(value) > 64 {
		return value[:64]
	}
	return value
}
