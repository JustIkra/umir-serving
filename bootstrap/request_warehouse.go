package bootstrap

import (
	"context"

	"github.com/JustIkra/umir-serving/contract"
)

type warehousePortsKey struct{}
type requestWarehouse struct{ fallback WarehousePorts }

func (w *requestWarehouse) ports(ctx context.Context) WarehousePorts {
	if p, ok := ctx.Value(warehousePortsKey{}).(WarehousePorts); ok {
		return p
	}
	return w.fallback
}

type requestRuntime struct {
	*Runtime
	ports WarehousePorts
}

func (r *requestRuntime) context(ctx context.Context) context.Context {
	return context.WithValue(ctx, warehousePortsKey{}, r.ports)
}
func (r *requestRuntime) PrepareOrder(ctx context.Context, request contract.PrepareOrderRequest) (contract.OrderProposal, error) {
	return r.Runtime.PrepareOrder(r.context(ctx), request)
}
func (r *requestRuntime) ConfirmOrder(ctx context.Context, request contract.ConfirmOrderRequest) (contract.ConfirmResult, error) {
	return r.Runtime.ConfirmOrder(r.context(ctx), request)
}
func (r *requestRuntime) ResolveConfirmation(ctx context.Context, request contract.ConfirmOrderRequest) (contract.ConfirmResult, error) {
	return r.Runtime.ResolveConfirmation(r.context(ctx), request)
}
func (r *requestRuntime) GetProducts(ctx context.Context) (contract.Products, error) {
	return r.Runtime.GetProducts(r.context(ctx))
}
func (r *requestRuntime) GetDishCatalog(ctx context.Context) (contract.DishCatalog, error) {
	return r.Runtime.GetDishCatalog(r.context(ctx))
}
func (r *requestRuntime) GetDish(ctx context.Context, dishID string) (contract.DishDetails, error) {
	return r.Runtime.GetDish(r.context(ctx), dishID)
}
func (r *requestRuntime) RegisterProduct(ctx context.Context, request contract.RegisterProductRequest) (contract.Product, error) {
	return r.Runtime.RegisterProduct(r.context(ctx), request)
}
func (r *requestRuntime) DefineDish(ctx context.Context, request contract.DefineDishRequest) (contract.Dish, error) {
	return r.Runtime.DefineDish(r.context(ctx), request)
}
func (r *requestRuntime) GetMenu(ctx context.Context) (contract.Menu, error) {
	return r.Runtime.GetMenu(r.context(ctx))
}
func (r *requestRuntime) GetStopList(ctx context.Context) (contract.StopList, error) {
	return r.Runtime.GetStopList(r.context(ctx))
}
func (r *requestRuntime) GetRecommendations(ctx context.Context) (contract.Recommendations, error) {
	return r.Runtime.GetRecommendations(r.context(ctx))
}
func (w *requestWarehouse) EvaluateOrder(ctx context.Context, request contract.EvaluateOrderRequest) (contract.OrderEvaluation, error) {
	return w.ports(ctx).Orders.EvaluateOrder(ctx, request)
}
func (w *requestWarehouse) CommitAcceptedOrder(ctx context.Context, request contract.CommitOrderRequest) (contract.CommitResult, error) {
	return w.ports(ctx).Orders.CommitAcceptedOrder(ctx, request)
}
func (w *requestWarehouse) FindRecordedCommand(ctx context.Context, commandID string) (contract.CommandRecord, bool, error) {
	return w.ports(ctx).Orders.FindRecordedCommand(ctx, commandID)
}
func (w *requestWarehouse) FindOrderByProposal(ctx context.Context, proposalID string) (contract.Order, bool, error) {
	return w.ports(ctx).Orders.FindOrderByProposal(ctx, proposalID)
}
func (w *requestWarehouse) GetProducts(ctx context.Context) (contract.Products, error) {
	return w.ports(ctx).Catalog.GetProducts(ctx)
}
func (w *requestWarehouse) GetDishCatalog(ctx context.Context) (contract.DishCatalog, error) {
	return w.ports(ctx).Catalog.GetDishCatalog(ctx)
}
func (w *requestWarehouse) GetDish(ctx context.Context, dishID string) (contract.DishDetails, error) {
	return w.ports(ctx).Catalog.GetDish(ctx, dishID)
}
func (w *requestWarehouse) RegisterProduct(ctx context.Context, request contract.RegisterProductRequest) (contract.Product, error) {
	return w.ports(ctx).Catalog.RegisterProduct(ctx, request)
}
func (w *requestWarehouse) DefineDish(ctx context.Context, request contract.DefineDishRequest) (contract.Dish, error) {
	return w.ports(ctx).Catalog.DefineDish(ctx, request)
}
func (w *requestWarehouse) EvaluateMenu(ctx context.Context) (contract.MenuEvaluation, error) {
	return w.ports(ctx).Menu.EvaluateMenu(ctx)
}
func (w *requestWarehouse) RecommendationInputs(ctx context.Context) (contract.RecommendationInputs, error) {
	ports := w.ports(ctx)
	if isNil(ports.Recommendations) {
		return contract.RecommendationInputs{}, contract.ErrInventoryUnavailable
	}
	return ports.Recommendations.RecommendationInputs(ctx)
}
func (w *requestWarehouse) FindOrderAttempt(ctx context.Context, commandID, proposalID string) (contract.OrderAttempt, error) {
	ports := w.ports(ctx)
	if ports.Attempts == nil {
		return contract.OrderAttempt{}, contract.ErrInventoryUnavailable
	}
	return ports.Attempts.FindOrderAttempt(ctx, commandID, proposalID)
}
