package bootstrap

import (
	"context"
	"reflect"

	"github.com/JustIkra/umir-serving/contract"
	"github.com/JustIkra/umir-serving/internal/adapters/memory"
	"github.com/JustIkra/umir-serving/internal/application"
)

type Runtime struct {
	orders          *application.OrdersService
	menu            *application.MenuService
	recommendations *application.RecommendationsService
}

func NewInMemory(options Options) (*Runtime, error) {
	if options.Now == nil || options.NewID == nil || isNil(options.OrdersWarehouse) || isNil(options.CatalogWarehouse) || isNil(options.MenuWarehouse) {
		return nil, contract.ErrInvalidArgument
	}

	store := memory.NewProposalStore()
	orders, err := application.NewOrdersService(store, options.OrdersWarehouse, options.Now, options.NewID)
	if err != nil {
		return nil, err
	}
	menu := application.NewMenuService(options.CatalogWarehouse, options.MenuWarehouse)
	recommendations := application.NewRecommendationsService(options.MenuWarehouse)
	return &Runtime{orders: orders, menu: menu, recommendations: recommendations}, nil
}

func (r *Runtime) PrepareOrder(ctx context.Context, request contract.PrepareOrderRequest) (contract.OrderProposal, error) {
	return r.orders.PrepareOrder(ctx, request)
}

func (r *Runtime) ConfirmOrder(ctx context.Context, request contract.ConfirmOrderRequest) (contract.ConfirmResult, error) {
	return r.orders.ConfirmOrder(ctx, request)
}

func (r *Runtime) GetProducts(ctx context.Context) (contract.Products, error) {
	return r.menu.GetProducts(ctx)
}

func (r *Runtime) GetDishCatalog(ctx context.Context) (contract.DishCatalog, error) {
	return r.menu.GetDishCatalog(ctx)
}

func (r *Runtime) GetDish(ctx context.Context, dishID string) (contract.DishDetails, error) {
	return r.menu.GetDish(ctx, dishID)
}

func (r *Runtime) RegisterProduct(ctx context.Context, request contract.RegisterProductRequest) (contract.Product, error) {
	return r.menu.RegisterProduct(ctx, request)
}

func (r *Runtime) DefineDish(ctx context.Context, request contract.DefineDishRequest) (contract.Dish, error) {
	return r.menu.DefineDish(ctx, request)
}

func (r *Runtime) GetMenu(ctx context.Context) (contract.Menu, error) {
	return r.menu.GetMenu(ctx)
}

func (r *Runtime) GetStopList(ctx context.Context) (contract.StopList, error) {
	return r.menu.GetStopList(ctx)
}

func (r *Runtime) GetRecommendations(ctx context.Context) (contract.Recommendations, error) {
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
