package tests_test

import (
	"context"

	"github.com/JustIkra/umir-serving/contract"
)

type ordersWarehouse struct{}

type catalogWarehouse struct{}

type menuWarehouse struct{}

var _ contract.OrdersWarehouse = ordersWarehouse{}
var _ contract.CatalogWarehouse = catalogWarehouse{}
var _ contract.MenuWarehouse = menuWarehouse{}

func (ordersWarehouse) EvaluateOrder(context.Context, contract.EvaluateOrderRequest) (contract.OrderEvaluation, error) {
	return contract.OrderEvaluation{}, nil
}

func (ordersWarehouse) CommitAcceptedOrder(context.Context, contract.CommitOrderRequest) (contract.CommitResult, error) {
	return contract.CommitResult{}, nil
}

func (ordersWarehouse) FindRecordedCommand(context.Context, string) (contract.CommandRecord, bool, error) {
	return contract.CommandRecord{}, false, nil
}

func (ordersWarehouse) FindOrderByProposal(context.Context, string) (contract.Order, bool, error) {
	return contract.Order{}, false, nil
}

func (catalogWarehouse) GetProducts(context.Context) (contract.Products, error) {
	return contract.Products{}, nil
}

func (catalogWarehouse) GetDishCatalog(context.Context) (contract.DishCatalog, error) {
	return contract.DishCatalog{}, nil
}

func (catalogWarehouse) GetDish(context.Context, string) (contract.Dish, error) {
	return contract.Dish{}, nil
}

func (catalogWarehouse) RegisterProduct(context.Context, contract.RegisterProductRequest) (contract.Product, error) {
	return contract.Product{}, nil
}

func (catalogWarehouse) DefineDish(context.Context, contract.DefineDishRequest) (contract.Dish, error) {
	return contract.Dish{}, nil
}

func (menuWarehouse) EvaluateMenu(context.Context) (contract.MenuEvaluation, error) {
	return contract.MenuEvaluation{}, nil
}
