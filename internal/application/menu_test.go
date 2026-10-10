package application_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/JustIkra/umir-serving/contract"
	"github.com/JustIkra/umir-serving/internal/application"
)

type menuPortFake struct {
	evaluation contract.MenuEvaluation
	err        error
	calls      int
}

func (w *menuPortFake) EvaluateMenu(context.Context) (contract.MenuEvaluation, error) {
	w.calls++
	return w.evaluation, w.err
}

type catalogPortFake struct {
	products      contract.Products
	catalog       contract.DishCatalog
	dish          contract.Dish
	product       contract.Product
	defined       contract.Dish
	gotDishID     string
	registered    contract.RegisterProductRequest
	defineRequest contract.DefineDishRequest
}

func (w *catalogPortFake) GetProducts(context.Context) (contract.Products, error) {
	return w.products, nil
}

func (w *catalogPortFake) GetDishCatalog(context.Context) (contract.DishCatalog, error) {
	return w.catalog, nil
}

func (w *catalogPortFake) GetDish(_ context.Context, dishID string) (contract.Dish, error) {
	w.gotDishID = dishID
	return w.dish, nil
}

func (w *catalogPortFake) RegisterProduct(_ context.Context, request contract.RegisterProductRequest) (contract.Product, error) {
	w.registered = request
	return w.product, nil
}

func (w *catalogPortFake) DefineDish(_ context.Context, request contract.DefineDishRequest) (contract.Dish, error) {
	w.defineRequest = request
	return w.defined, nil
}

func TestMenuStopListComplement(t *testing.T) {
	evaluatedAt := time.Date(2026, 10, 10, 15, 0, 0, 0, time.FixedZone("MSK", 3*60*60))
	warehouse := &menuPortFake{evaluation: contract.MenuEvaluation{
		EvaluatedAt:  evaluatedAt,
		StateVersion: 19,
		Dishes: []contract.DishEvaluation{
			{DishID: "dish-available", DishName: "Available", IsAvailable: true, Allocations: []contract.Allocation{{ProductID: "rice", Quantity: 100}}},
			{DishID: "dish-stopped", DishName: "Stopped", IsAvailable: false, Deficits: []contract.Deficit{{ProductID: "oil", RequiredQuantity: 50, AvailableQuantity: 0}}},
		},
	}}
	service := application.NewMenuService(&catalogPortFake{}, warehouse)

	menu, err := service.GetMenu(context.Background())
	if err != nil {
		t.Fatalf("GetMenu() error = %v", err)
	}
	stopList, err := service.GetStopList(context.Background())
	if err != nil {
		t.Fatalf("GetStopList() error = %v", err)
	}
	if warehouse.calls != 2 {
		t.Fatalf("EvaluateMenu() calls = %d, want 2", warehouse.calls)
	}
	if menu.EvaluatedAt != evaluatedAt || stopList.EvaluatedAt != evaluatedAt || menu.StateVersion != 19 || stopList.StateVersion != 19 {
		t.Fatalf("evaluation metadata was not preserved: menu=%+v stopList=%+v", menu, stopList)
	}
	if !reflect.DeepEqual(menu.Dishes, []contract.MenuDish{{DishID: "dish-available", Name: "Available"}}) {
		t.Fatalf("GetMenu() dishes = %+v", menu.Dishes)
	}
	if !reflect.DeepEqual(stopList.Dishes, []contract.StopListDish{{DishID: "dish-stopped", Name: "Stopped", Deficits: []contract.Deficit{{ProductID: "oil", RequiredQuantity: 50, AvailableQuantity: 0}}}}) {
		t.Fatalf("GetStopList() dishes = %+v", stopList.Dishes)
	}
}

func TestMenuPreparationUnavailable(t *testing.T) {
	warehouse := &menuPortFake{err: contract.ErrInventoryUnavailable}
	service := application.NewMenuService(&catalogPortFake{}, warehouse)

	if _, err := service.GetMenu(context.Background()); !errors.Is(err, contract.ErrInventoryUnavailable) {
		t.Fatalf("GetMenu() error = %v, want ErrInventoryUnavailable", err)
	}
	if _, err := service.GetStopList(context.Background()); !errors.Is(err, contract.ErrInventoryUnavailable) {
		t.Fatalf("GetStopList() error = %v, want ErrInventoryUnavailable", err)
	}
	recommendations := application.NewRecommendationsService(warehouse)
	if _, err := recommendations.GetRecommendations(context.Background()); !errors.Is(err, contract.ErrInventoryUnavailable) {
		t.Fatalf("GetRecommendations() error = %v, want ErrInventoryUnavailable", err)
	}
}

func TestMenuCatalogForwardingPreservesValuesAndCopiesSlices(t *testing.T) {
	warehouse := &catalogPortFake{
		products: contract.Products{{ProductID: "z-product", Name: "Z", BaseUnit: "g"}, {ProductID: "a-product", Name: "A", BaseUnit: "g"}},
		catalog: contract.DishCatalog{
			{DishID: "z-dish", Name: "Z", Recipe: []contract.Ingredient{{ProductID: "rice", Quantity: 100, BaseUnit: "g"}}},
			{DishID: "a-dish", Name: "A", Recipe: []contract.Ingredient{{ProductID: "rice", Quantity: 200, BaseUnit: "g"}}},
		},
		dish:    contract.Dish{DishID: "dish", Name: "Dish", Recipe: []contract.Ingredient{{ProductID: "rice", Quantity: 100, BaseUnit: "g"}}},
		product: contract.Product{ProductID: "sugar", Name: "Sugar", BaseUnit: "g"},
		defined: contract.Dish{DishID: "sweet", Name: "Sweet", Recipe: []contract.Ingredient{{ProductID: "sugar", Quantity: 20, BaseUnit: "g"}}},
	}
	service := application.NewMenuService(warehouse, &menuPortFake{})

	products, err := service.GetProducts(context.Background())
	if err != nil || !reflect.DeepEqual(products, contract.Products{{ProductID: "a-product", Name: "A", BaseUnit: "g"}, {ProductID: "z-product", Name: "Z", BaseUnit: "g"}}) {
		t.Fatalf("GetProducts() = %+v, %v", products, err)
	}
	products[0].Name = "mutated"
	if warehouse.products[1].Name != "A" {
		t.Fatal("GetProducts() exposed warehouse product slice")
	}

	catalog, err := service.GetDishCatalog(context.Background())
	if err != nil || !reflect.DeepEqual(catalog, contract.DishCatalog{
		{DishID: "a-dish", Name: "A", Recipe: []contract.Ingredient{{ProductID: "rice", Quantity: 200, BaseUnit: "g"}}},
		{DishID: "z-dish", Name: "Z", Recipe: []contract.Ingredient{{ProductID: "rice", Quantity: 100, BaseUnit: "g"}}},
	}) {
		t.Fatalf("GetDishCatalog() = %+v, %v", catalog, err)
	}
	catalog[0].Recipe[0].Quantity = 1
	if warehouse.catalog[1].Recipe[0].Quantity != 200 {
		t.Fatal("GetDishCatalog() exposed warehouse recipe slice")
	}

	dish, err := service.GetDish(context.Background(), "dish-7")
	if err != nil || dish.DishID != "dish" || warehouse.gotDishID != "dish-7" {
		t.Fatalf("GetDish() = %+v, %v; forwarded ID = %q", dish, err, warehouse.gotDishID)
	}
	dish.Recipe[0].Quantity = 2
	if warehouse.dish.Recipe[0].Quantity != 100 {
		t.Fatal("GetDish() exposed warehouse recipe slice")
	}

	registered, err := service.RegisterProduct(context.Background(), contract.RegisterProductRequest{CommandID: "cmd-product", ProductID: "tea", Name: "Tea", BaseUnit: "g"})
	if err != nil || registered != warehouse.product || warehouse.registered.CommandID != "cmd-product" || warehouse.registered.ProductID != "tea" {
		t.Fatalf("RegisterProduct() = %+v, %v; request = %+v", registered, err, warehouse.registered)
	}

	request := contract.DefineDishRequest{CommandID: "cmd-dish", DishID: "sweet", Name: "Sweet", Recipe: []contract.Ingredient{{ProductID: "sugar", Quantity: 20, BaseUnit: "g"}}}
	defined, err := service.DefineDish(context.Background(), request)
	if err != nil || defined.DishID != "sweet" || !reflect.DeepEqual(warehouse.defineRequest, request) {
		t.Fatalf("DefineDish() = %+v, %v; request = %+v", defined, err, warehouse.defineRequest)
	}
	defined.Recipe[0].Quantity = 3
	if warehouse.defined.Recipe[0].Quantity != 20 {
		t.Fatal("DefineDish() exposed warehouse recipe slice")
	}
	request.Recipe[0].Quantity = 9
	if warehouse.defineRequest.Recipe[0].Quantity != 20 {
		t.Fatal("DefineDish() forwarded caller recipe slice")
	}
}
