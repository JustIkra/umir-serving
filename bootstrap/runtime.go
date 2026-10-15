package bootstrap

import (
	"context"
	"reflect"
	"sync"

	"github.com/JustIkra/umir-serving/contract"
	"github.com/JustIkra/umir-serving/internal/adapters/memory"
	"github.com/JustIkra/umir-serving/internal/application"
)

type Runtime struct {
	registration    contract.RestaurantRegistration
	gate            sync.Mutex
	closed          bool
	active          int
	drained         chan struct{}
	stop            context.Context
	cancel          context.CancelFunc
	orders          *application.OrdersService
	menu            *application.MenuService
	recommendations *application.RecommendationsService
}

func NewInMemory(options Options) (*Runtime, error) {
	if options.Now == nil || options.NewID == nil || isNil(options.OrdersWarehouse) || isNil(options.CatalogWarehouse) || isNil(options.MenuWarehouse) {
		return nil, contract.ErrInvalidArgument
	}

	store := memory.NewProposalStore()
	orders, err := application.NewOrdersService(store, options.OrdersWarehouse, options.Now, options.NewID, options.OrderAttemptReader)
	if err != nil {
		return nil, err
	}
	orders.WithLogger(options.Logger)
	menu := application.NewMenuService(options.CatalogWarehouse, options.MenuWarehouse)
	recommendations := application.NewRecommendationsService(options.MenuWarehouse)
	if !isNil(options.RecommendationsWarehouse) {
		recommendations = application.NewFINRecommendationsService(options.RecommendationsWarehouse)
	}
	stop, cancel := context.WithCancel(context.Background())
	return &Runtime{orders: orders, menu: menu, recommendations: recommendations, stop: stop, cancel: cancel}, nil
}

func (r *Runtime) PrepareOrder(ctx context.Context, request contract.PrepareOrderRequest) (contract.OrderProposal, error) {
	ctx, release, err := r.admit(ctx)
	if err != nil {
		return contract.OrderProposal{}, err
	}
	defer release()
	if err := r.requireScope(ctx, "order.prepare"); err != nil {
		return contract.OrderProposal{}, err
	}
	return r.orders.PrepareOrder(ctx, request)
}

func (r *Runtime) ConfirmOrder(ctx context.Context, request contract.ConfirmOrderRequest) (contract.ConfirmResult, error) {
	ctx, release, err := r.admit(ctx)
	if err != nil {
		return contract.ConfirmResult{}, err
	}
	defer release()
	if err := r.requireScope(ctx, "order.confirm"); err != nil {
		return contract.ConfirmResult{}, err
	}
	return r.orders.ConfirmOrder(ctx, request)
}

func (r *Runtime) ResolveConfirmation(ctx context.Context, request contract.ConfirmOrderRequest) (contract.ConfirmResult, error) {
	ctx, release, err := r.admit(ctx)
	if err != nil {
		return contract.ConfirmResult{}, err
	}
	defer release()
	if err := r.requireScope(ctx, "order.read"); err != nil {
		return contract.ConfirmResult{}, err
	}
	return r.orders.ResolveConfirmation(ctx, request)
}

func (r *Runtime) GetProducts(ctx context.Context) (contract.Products, error) {
	ctx, release, err := r.admit(ctx)
	if err != nil {
		return contract.Products{}, err
	}
	defer release()
	if err := r.requireScope(ctx, "menu.read"); err != nil {
		return contract.Products{}, err
	}
	return r.menu.GetProducts(ctx)
}

func (r *Runtime) GetDishCatalog(ctx context.Context) (contract.DishCatalog, error) {
	ctx, release, err := r.admit(ctx)
	if err != nil {
		return contract.DishCatalog{}, err
	}
	defer release()
	if err := r.requireScope(ctx, "menu.read"); err != nil {
		return contract.DishCatalog{}, err
	}
	return r.menu.GetDishCatalog(ctx)
}

func (r *Runtime) GetDish(ctx context.Context, dishID string) (contract.DishDetails, error) {
	ctx, release, err := r.admit(ctx)
	if err != nil {
		return contract.DishDetails{}, err
	}
	defer release()
	if err := r.requireScope(ctx, "menu.read"); err != nil {
		return contract.DishDetails{}, err
	}
	return r.menu.GetDish(ctx, dishID)
}

func (r *Runtime) RegisterProduct(ctx context.Context, request contract.RegisterProductRequest) (contract.Product, error) {
	ctx, release, err := r.admit(ctx)
	if err != nil {
		return contract.Product{}, err
	}
	defer release()
	if err := r.requireScope(ctx, "catalog.write"); err != nil {
		return contract.Product{}, err
	}
	return r.menu.RegisterProduct(ctx, request)
}

func (r *Runtime) DefineDish(ctx context.Context, request contract.DefineDishRequest) (contract.Dish, error) {
	ctx, release, err := r.admit(ctx)
	if err != nil {
		return contract.Dish{}, err
	}
	defer release()
	if err := r.requireScope(ctx, "catalog.write"); err != nil {
		return contract.Dish{}, err
	}
	return r.menu.DefineDish(ctx, request)
}

func (r *Runtime) GetMenu(ctx context.Context) (contract.Menu, error) {
	ctx, release, err := r.admit(ctx)
	if err != nil {
		return contract.Menu{}, err
	}
	defer release()
	if err := r.requireScope(ctx, "menu.read"); err != nil {
		return contract.Menu{}, err
	}
	return r.menu.GetMenu(ctx)
}

func (r *Runtime) GetStopList(ctx context.Context) (contract.StopList, error) {
	ctx, release, err := r.admit(ctx)
	if err != nil {
		return contract.StopList{}, err
	}
	defer release()
	if err := r.requireScope(ctx, "menu.read"); err != nil {
		return contract.StopList{}, err
	}
	return r.menu.GetStopList(ctx)
}

func (r *Runtime) GetRecommendations(ctx context.Context) (contract.Recommendations, error) {
	ctx, release, err := r.admit(ctx)
	if err != nil {
		return contract.Recommendations{}, err
	}
	defer release()
	if err := r.requireScope(ctx, "recommendations.read"); err != nil {
		return contract.Recommendations{}, err
	}
	return r.recommendations.GetRecommendations(ctx)
}

func isNil(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func (r *Runtime) admit(ctx context.Context) (context.Context, func(), error) {
	r.gate.Lock()
	defer r.gate.Unlock()
	if r.closed {
		return nil, nil, &contract.OperationError{Code: "ScopeDeleted", Outcome: "unavailable"}
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	r.active++
	admitted, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(r.stop, cancel)
	return admitted, func() {
		stop()
		cancel()
		r.gate.Lock()
		defer r.gate.Unlock()
		r.active--
		if r.closed && r.active == 0 {
			close(r.drained)
		}
	}, nil
}
func (r *Runtime) fence() {
	r.gate.Lock()
	defer r.gate.Unlock()
	if r.closed {
		return
	}
	r.closed = true
	r.drained = make(chan struct{})
	r.cancel()
	if r.active == 0 {
		close(r.drained)
	}
}
func (r *Runtime) awaitClose(ctx context.Context) error {
	select {
	case <-r.drained:
		r.gate.Lock()
		r.orders = nil
		r.menu = nil
		r.recommendations = nil
		r.gate.Unlock()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
