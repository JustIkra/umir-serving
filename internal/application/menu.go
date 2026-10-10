package application

import (
	"context"
	"sort"

	"github.com/JustIkra/umir-serving/contract"
)

type MenuService struct {
	catalog contract.CatalogWarehouse
	menu    contract.MenuWarehouse
}

func NewMenuService(catalog contract.CatalogWarehouse, menu contract.MenuWarehouse) *MenuService {
	return &MenuService{catalog: catalog, menu: menu}
}

func (s *MenuService) GetProducts(ctx context.Context) (contract.Products, error) {
	products, err := s.catalog.GetProducts(ctx)
	if err != nil {
		return nil, err
	}
	result := append(make(contract.Products, 0, len(products)), products...)
	sort.Slice(result, func(i, j int) bool { return result[i].ProductID < result[j].ProductID })
	return result, nil
}

func (s *MenuService) GetDishCatalog(ctx context.Context) (contract.DishCatalog, error) {
	dishes, err := s.catalog.GetDishCatalog(ctx)
	if err != nil {
		return nil, err
	}
	result := make(contract.DishCatalog, len(dishes))
	for i, dish := range dishes {
		result[i] = cloneContractDish(dish)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].DishID < result[j].DishID })
	return result, nil
}

func (s *MenuService) GetDish(ctx context.Context, dishID string) (contract.Dish, error) {
	dish, err := s.catalog.GetDish(ctx, dishID)
	if err != nil {
		return contract.Dish{}, err
	}
	return cloneContractDish(dish), nil
}

func (s *MenuService) RegisterProduct(ctx context.Context, request contract.RegisterProductRequest) (contract.Product, error) {
	return s.catalog.RegisterProduct(ctx, request)
}

func (s *MenuService) DefineDish(ctx context.Context, request contract.DefineDishRequest) (contract.Dish, error) {
	request.Recipe = cloneContractIngredients(request.Recipe)
	dish, err := s.catalog.DefineDish(ctx, request)
	if err != nil {
		return contract.Dish{}, err
	}
	return cloneContractDish(dish), nil
}

func (s *MenuService) GetMenu(ctx context.Context) (contract.Menu, error) {
	evaluation, err := s.menu.EvaluateMenu(ctx)
	if err != nil {
		return contract.Menu{}, err
	}
	result := contract.Menu{
		EvaluatedAt:  evaluation.EvaluatedAt,
		StateVersion: evaluation.StateVersion,
		Dishes:       make([]contract.MenuDish, 0, len(evaluation.Dishes)),
	}
	for _, dish := range evaluation.Dishes {
		if dish.IsAvailable {
			result.Dishes = append(result.Dishes, contract.MenuDish{DishID: dish.DishID, Name: dish.DishName})
		}
	}
	sort.Slice(result.Dishes, func(i, j int) bool { return result.Dishes[i].DishID < result.Dishes[j].DishID })
	return result, nil
}

func (s *MenuService) GetStopList(ctx context.Context) (contract.StopList, error) {
	evaluation, err := s.menu.EvaluateMenu(ctx)
	if err != nil {
		return contract.StopList{}, err
	}
	result := contract.StopList{
		EvaluatedAt:  evaluation.EvaluatedAt,
		StateVersion: evaluation.StateVersion,
		Dishes:       make([]contract.StopListDish, 0, len(evaluation.Dishes)),
	}
	for _, dish := range evaluation.Dishes {
		if dish.IsAvailable {
			continue
		}
		deficits := append(make([]contract.Deficit, 0, len(dish.Deficits)), dish.Deficits...)
		result.Dishes = append(result.Dishes, contract.StopListDish{DishID: dish.DishID, Name: dish.DishName, Deficits: deficits})
	}
	sort.Slice(result.Dishes, func(i, j int) bool { return result.Dishes[i].DishID < result.Dishes[j].DishID })
	return result, nil
}

func cloneContractIngredients(ingredients []contract.Ingredient) []contract.Ingredient {
	return append(make([]contract.Ingredient, 0, len(ingredients)), ingredients...)
}

func cloneContractDish(dish contract.Dish) contract.Dish {
	dish.Recipe = cloneContractIngredients(dish.Recipe)
	return dish
}
