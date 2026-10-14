package contract

import (
	"context"
	"regexp"
	"slices"
	"strconv"
	"time"
)

var entityIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func ValidEntityID(value string) bool { return entityIDPattern.MatchString(value) }
func ValidVersion(value string) bool {
	parsed, err := strconv.ParseInt(value, 10, 64)
	return err == nil && parsed > 0 && strconv.FormatInt(parsed, 10) == value
}

type RequestScope struct {
	Kind             string
	Subject          string
	RuntimeEpoch     string
	OrgID            string
	TenantID         string
	UserID           string
	Operation        string
	Permissions      []string
	TimeZone         string
	SettingsVersion  string
	LifecycleVersion string
	token            string
}

func (s RequestScope) Allows(permission string) bool {
	return slices.Contains(s.Permissions, permission)
}
func (s RequestScope) WithToken(token string) RequestScope { s.token = token; return s }
func (s RequestScope) Token() string                       { return s.token }

type requestScopeKey struct{}

func WithRequestScope(ctx context.Context, scope RequestScope) context.Context {
	scope.Permissions = slices.Clone(scope.Permissions)
	return context.WithValue(ctx, requestScopeKey{}, scope)
}
func RequestScopeFromContext(ctx context.Context) (RequestScope, bool) {
	scope, ok := ctx.Value(requestScopeKey{}).(RequestScope)
	scope.Permissions = slices.Clone(scope.Permissions)
	return scope, ok
}

type RestaurantRegistration struct {
	TimeZone         string
	SettingsVersion  string
	OrgID            string
	TenantID         string
	OperationID      string
	ExpectedEpoch    string
	LifecycleVersion string
}

type RuntimeInfo struct {
	Epoch          string
	WarehouseEpoch string
}

type WarehouseContext struct {
	OrgID           string
	TenantID        string
	Epoch           string
	TimeZone        string
	SettingsVersion string
	EvaluatedAt     time.Time
	StateVersion    int64
}

type WarehouseObserver interface{ Record(WarehouseContext) }
type warehouseObserverKey struct{}

func WithWarehouseObserver(ctx context.Context, observer WarehouseObserver) context.Context {
	return context.WithValue(ctx, warehouseObserverKey{}, observer)
}
func RecordWarehouseContext(ctx context.Context, observation WarehouseContext) {
	if observer, ok := ctx.Value(warehouseObserverKey{}).(WarehouseObserver); ok {
		observer.Record(observation)
	}
}

type WarehousePreparation struct {
	OrgID            string
	TenantID         string
	Epoch            string
	CommittedVersion int64
}

type WarehousePreparationObserver interface{ RecordPreparation(WarehousePreparation) }

func RecordWarehousePreparation(ctx context.Context, preparation WarehousePreparation) {
	if observer, ok := ctx.Value(warehouseObserverKey{}).(WarehousePreparationObserver); ok {
		observer.RecordPreparation(preparation)
	}
}

type warehouseBindingKey struct{}
type WarehouseBinding struct {
	Context  WarehouseContext
	Retained bool
}

func WithWarehouseBinding(ctx context.Context, value WarehouseContext, retained bool) context.Context {
	return context.WithValue(ctx, warehouseBindingKey{}, WarehouseBinding{Context: value, Retained: retained})
}
func WarehouseBindingFromContext(ctx context.Context) (WarehouseBinding, bool) {
	v, ok := ctx.Value(warehouseBindingKey{}).(WarehouseBinding)
	return v, ok
}

type RuntimeReadiness struct {
	RestaurantID     string
	Service          string
	Epoch            string
	State            string
	LifecycleVersion string
}
type RuntimeStatus struct {
	Epoch string
	Ready bool
}
type RegistryRestaurant struct {
	OrgID            string
	TenantID         string
	LifecycleVersion string
	Lifecycle        string
}
type RegistrySnapshot struct {
	Revision   string
	Complete   bool
	Entries    []RegistryRestaurant
	Tombstones []RegistryRestaurant
}
