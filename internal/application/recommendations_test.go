package application_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/JustIkra/umir-serving/contract"
	"github.com/JustIkra/umir-serving/internal/application"
)

type finRecommendationPort struct {
	input contract.RecommendationInputs
	err   error
	calls int
}

func (p *finRecommendationPort) RecommendationInputs(context.Context) (contract.RecommendationInputs, error) {
	p.calls++
	return p.input, p.err
}

func finRecommendationFixture() contract.RecommendationInputs {
	at := time.Date(2026, 10, 15, 12, 0, 0, 0, time.FixedZone("MSK", 10800))
	from := at.Add(-168 * time.Hour)
	input := contract.RecommendationInputs{EvaluatedAt: at, StateVersion: 42, HorizonHours: 72, Window: contract.PlanningWindow{From: from, To: at, Days: 7}, Coverage: contract.HistoryCoverage{Epoch: "44444444-4444-4444-8444-444444444444", CoverageFrom: at.Add(-time.Hour), RequestedFrom: from, AvailableFrom: at.Add(-time.Hour)}, RiskBatches: []contract.RiskBatch{}, Demand: []contract.DishDemand{}}
	input.Menu = contract.MenuEvaluation{EvaluatedAt: at, StateVersion: 42, Dishes: []contract.DishEvaluation{}}
	add := func(id, product string, risk, recipe, fresh int64, expiry time.Time) {
		allocations := []contract.MenuAllocation{{ProductID: product, ProductName: product, BatchID: product + "-risk", Quantity: min(risk, recipe), BaseUnit: "g", ExpiresAt: expiry}}
		if fresh > 0 {
			allocations = append(allocations, contract.MenuAllocation{ProductID: product, ProductName: product, BatchID: product + "-fresh", Quantity: fresh, BaseUnit: "g", ExpiresAt: at.Add(180 * 24 * time.Hour)})
		}
		input.Menu.Dishes = append(input.Menu.Dishes, contract.DishEvaluation{DishID: id, DishName: id, IsAvailable: true, Deficits: []contract.Deficit{}, Allocations: allocations})
		if !expiry.After(at.Add(72 * time.Hour)) {
			input.RiskBatches = append(input.RiskBatches, contract.RiskBatch{ProductID: product, ProductName: product, BatchID: product + "-risk", RemainingQuantity: risk, BaseUnit: "g", ExpiresAt: expiry, RemainingSeconds: int64(expiry.Sub(at) / time.Second)})
		}
	}
	add("z-fish", "fish", 3000000, 300000, 0, at.Add(6*time.Hour))
	add("a-parsley", "parsley", 1000, 5000, 4000, at.Add(6*time.Hour))
	add("rice", "rice", 100000, 1000, 0, at.Add(180*24*time.Hour))
	input.Menu.Dishes = append(input.Menu.Dishes, contract.DishEvaluation{DishID: "unavailable", DishName: "Unavailable", Deficits: []contract.Deficit{{ProductID: "fish", RequiredQuantity: math.MaxInt64, AvailableQuantity: 3000000}}, Allocations: []contract.MenuAllocation{}})
	return input
}

func TestFINRecommendationsFishOutranksParsleyAndExcludesRice(t *testing.T) {
	port := &finRecommendationPort{input: finRecommendationFixture()}
	value, err := application.NewFINRecommendationsService(port).GetRecommendations(context.Background())
	if err != nil || len(value.Items) != 2 || value.Items[0].DishID != "z-fish" || value.Items[1].DishID != "a-parsley" || value.Items[0].Score != "0.666667" || value.Items[1].Score != "0.133333" {
		t.Fatalf("FIN1 ranking %+v %v", value.Items, err)
	}
	fish, parsley := value.Items[0], value.Items[1]
	if port.calls != 1 || value.PolicyVersion != "FIN1" || value.HorizonHours != 72 || value.Coverage.Complete || fish.DemandBasis != "fallback" || len(fish.Reasons) != 1 || fish.Reasons[0].RiskQuantity != 3000000 || fish.Reasons[0].QuantityPerPortion != 300000 || fish.Reasons[0].TargetPortions != 10 || fish.Reasons[0].RemainingSeconds != 21600 || parsley.Reasons[0].QuantityPerPortion != 5000 || parsley.Reasons[0].TargetPortions != 1 {
		t.Fatalf("explanation does not use whole recipe: %+v", value)
	}
	value.Items[0].Reasons[0].RiskQuantity = 1
	if port.input.RiskBatches[0].RemainingQuantity != 3000000 {
		t.Fatal("result aliases warehouse input")
	}
}

func TestFINRecommendationsDemandHistoryAndFallback(t *testing.T) {
	for _, complete := range []bool{false, true} {
		input := finRecommendationFixture()
		input.Demand = []contract.DishDemand{{DishID: "z-fish", Portions: 168}}
		if complete {
			input.Coverage.Complete = true
			input.Coverage.CoverageFrom = input.Window.From
			input.Coverage.AvailableFrom = input.Window.From
		}
		value, err := application.NewFINRecommendationsService(&finRecommendationPort{input: input}).GetRecommendations(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		wantScore, wantBasis := "0.666667", "fallback"
		if complete {
			wantScore, wantBasis = "0.400000", "history"
		}
		if value.Items[0].Score != wantScore || value.Items[0].DemandBasis != wantBasis || value.Items[1].DemandBasis != "fallback" {
			t.Fatalf("complete=%v: %+v", complete, value.Items)
		}
	}
}

func TestFINRecommendationsAggregateRiskAndAllProductReasons(t *testing.T) {
	input := finRecommendationFixture()
	input.RiskBatches = append(input.RiskBatches, contract.RiskBatch{ProductID: "fish", ProductName: "fish", BatchID: "fish-second", RemainingQuantity: 1000000, BaseUnit: "g", ExpiresAt: input.EvaluatedAt.Add(12 * time.Hour), RemainingSeconds: 43200})
	input.Menu.Dishes[0].Allocations = append(input.Menu.Dishes[0].Allocations, contract.MenuAllocation{ProductID: "parsley", ProductName: "parsley", BatchID: "parsley-risk", Quantity: 1000, BaseUnit: "g", ExpiresAt: input.EvaluatedAt.Add(6 * time.Hour)})
	value, err := application.NewFINRecommendationsService(&finRecommendationPort{input: input}).GetRecommendations(context.Background())
	if err != nil || value.Items[0].Score != "1.266667" || len(value.Items[0].Reasons) != 2 || value.Items[0].Reasons[0].ProductID != "fish" || value.Items[0].Reasons[0].RiskQuantity != 4000000 || value.Items[0].Reasons[0].TargetPortions != 14 || value.Items[0].Reasons[1].ProductID != "parsley" {
		t.Fatalf("aggregate score/reasons %+v %v", value, err)
	}
}

func TestFINRecommendationsExactTieUsesExpiryAndSubsecondStaysFinite(t *testing.T) {
	input := finRecommendationFixture()
	input.Menu.Dishes = []contract.DishEvaluation{}
	input.RiskBatches = []contract.RiskBatch{}
	for _, v := range []struct {
		id       string
		delay    time.Duration
		quantity int64
	}{{"z-early", 6 * time.Hour, 1}, {"a-late", 24 * time.Hour, 2}} {
		expiry := input.EvaluatedAt.Add(v.delay)
		allocations := []contract.MenuAllocation{{ProductID: v.id, ProductName: v.id, BatchID: v.id, Quantity: v.quantity, BaseUnit: "g", ExpiresAt: expiry}}
		if v.quantity == 1 {
			allocations = append(allocations, contract.MenuAllocation{ProductID: v.id, ProductName: v.id, BatchID: v.id + "-fresh", Quantity: 1, BaseUnit: "g", ExpiresAt: input.EvaluatedAt.Add(180 * 24 * time.Hour)})
		}
		input.Menu.Dishes = append(input.Menu.Dishes, contract.DishEvaluation{DishID: v.id, DishName: v.id, IsAvailable: true, Deficits: []contract.Deficit{}, Allocations: allocations})
		input.RiskBatches = append(input.RiskBatches, contract.RiskBatch{ProductID: v.id, ProductName: v.id, BatchID: v.id, RemainingQuantity: v.quantity, BaseUnit: "g", ExpiresAt: expiry, RemainingSeconds: int64(v.delay / time.Second)})
	}
	value, err := application.NewFINRecommendationsService(&finRecommendationPort{input: input}).GetRecommendations(context.Background())
	if err != nil || len(value.Items) != 2 || value.Items[0].DishID != "z-early" || value.Items[0].Score != "0.333333" || value.Items[1].Score != "0.333333" {
		t.Fatalf("exact tie expiry: %+v %v", value, err)
	}
	input.Menu.Dishes = input.Menu.Dishes[:1]
	input.RiskBatches = input.RiskBatches[:1]
	input.RiskBatches[0].ExpiresAt = input.EvaluatedAt.Add(time.Nanosecond)
	input.RiskBatches[0].RemainingSeconds = 0
	input.Menu.Dishes[0].Allocations[0].ExpiresAt = input.RiskBatches[0].ExpiresAt
	value, err = application.NewFINRecommendationsService(&finRecommendationPort{input: input}).GetRecommendations(context.Background())
	if err != nil || len(value.Items) != 1 || value.Items[0].Score != "0.000000" || value.Items[0].Reasons[0].RemainingSeconds != 0 {
		t.Fatalf("subsecond finite score: %+v %v", value, err)
	}
}

func TestFINRecommendationsExactSortBeforeDisplayAndTopTen(t *testing.T) {
	input := finRecommendationFixture()
	input.Menu.Dishes = []contract.DishEvaluation{}
	input.RiskBatches = []contract.RiskBatch{}
	for i := range 12 {
		id := fmt.Sprintf("dish-%02d", i)
		quantity := int64(999999999999999)
		if i == 11 {
			quantity++
		}
		allocations := []contract.MenuAllocation{{ProductID: id, ProductName: id, BatchID: id, Quantity: quantity, BaseUnit: "g", ExpiresAt: input.EvaluatedAt.Add(6 * time.Hour)}}
		if quantity < 1000000000000000 {
			allocations = append(allocations, contract.MenuAllocation{ProductID: id, ProductName: id, BatchID: id + "-fresh", Quantity: 1, BaseUnit: "g", ExpiresAt: input.EvaluatedAt.Add(180 * 24 * time.Hour)})
		}
		input.Menu.Dishes = append(input.Menu.Dishes, contract.DishEvaluation{DishID: id, DishName: id, IsAvailable: true, Deficits: []contract.Deficit{}, Allocations: allocations})
		input.RiskBatches = append(input.RiskBatches, contract.RiskBatch{ProductID: id, ProductName: id, BatchID: id, RemainingQuantity: quantity, BaseUnit: "g", ExpiresAt: input.EvaluatedAt.Add(6 * time.Hour), RemainingSeconds: 21600})
	}
	value, err := application.NewFINRecommendationsService(&finRecommendationPort{input: input}).GetRecommendations(context.Background())
	if err != nil || len(value.Items) != 10 || value.Items[0].DishID != "dish-11" || value.Items[1].DishID != "dish-00" || value.Items[0].Score != value.Items[1].Score {
		t.Fatalf("rounded score decided rank/ties: %+v %v", value.Items, err)
	}
}

func TestFINRecommendationsRejectInconsistentCaptureAndInvalidFacts(t *testing.T) {
	for name, mutate := range map[string]func(*contract.RecommendationInputs){
		"menu-version": func(v *contract.RecommendationInputs) { v.Menu.StateVersion++ },
		"fresh-batch-expiry": func(v *contract.RecommendationInputs) {
			allocation := v.Menu.Dishes[1].Allocations[1]
			allocation.ExpiresAt = allocation.ExpiresAt.Add(time.Hour)
			v.Menu.Dishes = append(v.Menu.Dishes, contract.DishEvaluation{DishID: "other", DishName: "Other", IsAvailable: true, Deficits: []contract.Deficit{}, Allocations: []contract.MenuAllocation{allocation}})
		},
		"menu-instant": func(v *contract.RecommendationInputs) { v.Menu.EvaluatedAt = v.Menu.EvaluatedAt.Add(time.Second) },
		"window":       func(v *contract.RecommendationInputs) { v.Window.From = v.Window.From.Add(time.Nanosecond) },
		"coverage":     func(v *contract.RecommendationInputs) { v.Coverage.Complete = true },
		"zero-risk":    func(v *contract.RecommendationInputs) { v.RiskBatches[0].RemainingQuantity = 0 },
		"expired":      func(v *contract.RecommendationInputs) { v.RiskBatches[0].ExpiresAt = v.EvaluatedAt },
		"beyond-horizon": func(v *contract.RecommendationInputs) {
			v.RiskBatches[0].ExpiresAt = v.EvaluatedAt.Add(72*time.Hour + time.Nanosecond)
		},
		"seconds":         func(v *contract.RecommendationInputs) { v.RiskBatches[0].RemainingSeconds++ },
		"mixed-unit":      func(v *contract.RecommendationInputs) { v.RiskBatches[0].BaseUnit = "ml" },
		"duplicate-batch": func(v *contract.RecommendationInputs) { v.RiskBatches = append(v.RiskBatches, v.RiskBatches[0]) },
		"duplicate-demand": func(v *contract.RecommendationInputs) {
			v.Demand = []contract.DishDemand{{DishID: "z-fish", Portions: 1}, {DishID: "z-fish", Portions: 1}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			input := finRecommendationFixture()
			mutate(&input)
			if _, err := application.NewFINRecommendationsService(&finRecommendationPort{input: input}).GetRecommendations(context.Background()); err == nil {
				t.Fatal("inconsistent facts accepted")
			}
		})
	}
	if _, err := application.NewFINRecommendationsService(&finRecommendationPort{err: contract.ErrInventoryUnavailable}).GetRecommendations(context.Background()); !errors.Is(err, contract.ErrInventoryUnavailable) {
		t.Fatalf("upstream failure became empty success: %v", err)
	}
}
