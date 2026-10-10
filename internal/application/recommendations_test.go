package application_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/JustIkra/umir-serving/contract"
	"github.com/JustIkra/umir-serving/internal/application"
)

func TestRecommendationsActualAllocations(t *testing.T) {
	priority := time.Date(2026, 10, 11, 9, 0, 0, 0, time.FixedZone("MSK", 3*60*60))
	warehouse := &menuPortFake{evaluation: contract.MenuEvaluation{Dishes: []contract.DishEvaluation{
		{DishID: "dish", DishName: "Dish", IsAvailable: true, Allocations: []contract.MenuAllocation{
			{ProductID: "rice", ProductName: "Rice", BatchID: "batch-early", Quantity: 250, BaseUnit: "g", ExpiresAt: priority},
			{ProductID: "rice", ProductName: "Rice", BatchID: "batch-late", Quantity: 750, BaseUnit: "g", ExpiresAt: priority.Add(24 * time.Hour)},
		}},
		{DishID: "unavailable", IsAvailable: false, Allocations: []contract.MenuAllocation{{ProductID: "oil", BatchID: "ignored", Quantity: 900, ExpiresAt: priority.Add(-time.Hour)}}},
	}}}
	service := application.NewRecommendationsService(warehouse)

	result, err := service.GetRecommendations(context.Background())
	if err != nil {
		t.Fatalf("GetRecommendations() error = %v", err)
	}
	if warehouse.calls != 1 {
		t.Fatalf("EvaluateMenu() calls = %d, want 1", warehouse.calls)
	}
	if result.EvaluatedAt != warehouse.evaluation.EvaluatedAt || result.StateVersion != warehouse.evaluation.StateVersion {
		t.Fatalf("GetRecommendations() metadata = %v/%d, want %v/%d", result.EvaluatedAt, result.StateVersion, warehouse.evaluation.EvaluatedAt, warehouse.evaluation.StateVersion)
	}
	if len(result.Items) != 1 || result.Items[0].DishID != "dish" || result.Items[0].DishName != "Dish" || result.Items[0].PriorityExpiresAt != priority || !reflect.DeepEqual(result.Items[0].Reasons, []contract.RecommendationReason{{ProductID: "rice", ProductName: "Rice", BatchID: "batch-early", QuantityPerPortion: 250, BaseUnit: "g", ExpiresAt: priority}}) {
		t.Fatalf("GetRecommendations() = %+v", result.Items)
	}
}

func TestRecommendationTiedReasons(t *testing.T) {
	priority := time.Date(2026, 10, 11, 9, 0, 0, 0, time.FixedZone("MSK", 3*60*60))
	warehouse := &menuPortFake{evaluation: contract.MenuEvaluation{Dishes: []contract.DishEvaluation{{
		DishID: "dish", DishName: "Dish", IsAvailable: true, Allocations: []contract.MenuAllocation{
			{ProductID: "z-product", ProductName: "Z", BatchID: "z-batch", Quantity: 7, BaseUnit: "ml", ExpiresAt: priority},
			{ProductID: "a-product", ProductName: "A", BatchID: "b-batch", Quantity: 11, BaseUnit: "g", ExpiresAt: priority},
			{ProductID: "a-product", ProductName: "A", BatchID: "a-batch", Quantity: 13, BaseUnit: "g", ExpiresAt: priority},
			{ProductID: "earlier-product", ProductName: "Earlier", BatchID: "earlier", Quantity: 17, BaseUnit: "g", ExpiresAt: priority.Add(time.Hour)},
		},
	}}}}
	service := application.NewRecommendationsService(warehouse)

	result, err := service.GetRecommendations(context.Background())
	if err != nil {
		t.Fatalf("GetRecommendations() error = %v", err)
	}
	want := []contract.RecommendationReason{
		{ProductID: "a-product", ProductName: "A", BatchID: "a-batch", QuantityPerPortion: 13, BaseUnit: "g", ExpiresAt: priority},
		{ProductID: "a-product", ProductName: "A", BatchID: "b-batch", QuantityPerPortion: 11, BaseUnit: "g", ExpiresAt: priority},
		{ProductID: "z-product", ProductName: "Z", BatchID: "z-batch", QuantityPerPortion: 7, BaseUnit: "ml", ExpiresAt: priority},
	}
	if len(result.Items) != 1 || result.Items[0].PriorityExpiresAt != priority || !reflect.DeepEqual(result.Items[0].Reasons, want) {
		t.Fatalf("GetRecommendations() allocations = %+v, want %+v", result.Items, want)
	}
}

func TestRecommendationDishTie(t *testing.T) {
	priority := time.Date(2026, 10, 11, 9, 0, 0, 0, time.FixedZone("MSK", 3*60*60))
	warehouse := &menuPortFake{evaluation: contract.MenuEvaluation{Dishes: []contract.DishEvaluation{
		{DishID: "dish-z", DishName: "Z", IsAvailable: true, Allocations: []contract.MenuAllocation{{ProductID: "rice", BatchID: "batch-z", Quantity: 100, ExpiresAt: priority}}},
		{DishID: "dish-a", DishName: "A", IsAvailable: true, Allocations: []contract.MenuAllocation{{ProductID: "rice", BatchID: "batch-a", Quantity: 100, ExpiresAt: priority}}},
	}}}
	service := application.NewRecommendationsService(warehouse)

	result, err := service.GetRecommendations(context.Background())
	if err != nil {
		t.Fatalf("GetRecommendations() error = %v", err)
	}
	if len(result.Items) != 2 || result.Items[0].DishID != "dish-a" || result.Items[1].DishID != "dish-z" {
		t.Fatalf("GetRecommendations() dish order = %+v", result.Items)
	}
}
