package application

import (
	"context"
	"math"
	"math/big"
	"sort"
	"time"

	"github.com/JustIkra/umir-serving/contract"
)

type RecommendationsService struct {
	inputs     contract.RecommendationWarehouse
	legacyMenu contract.MenuWarehouse
}

func NewRecommendationsService(menu contract.MenuWarehouse) *RecommendationsService {
	return &RecommendationsService{legacyMenu: menu}
}

func NewFINRecommendationsService(inputs contract.RecommendationWarehouse) *RecommendationsService {
	return &RecommendationsService{inputs: inputs}
}

type productRisk struct {
	quantity int64
	nearest  contract.RiskBatch
}

type scoredRecommendation struct {
	item  contract.Recommendation
	score *big.Rat
}

func recommendationFailure() error {
	return &contract.OperationError{Code: "CalculationUnavailable", Message: "Recommendation inputs are inconsistent.", Outcome: "unavailable"}
}

func (s *RecommendationsService) GetRecommendations(ctx context.Context) (contract.Recommendations, error) {
	if err := ctx.Err(); err != nil {
		return contract.Recommendations{}, err
	}
	if s.inputs == nil {
		if s.legacyMenu == nil {
			return contract.Recommendations{}, contract.ErrInventoryUnavailable
		}
		return legacyRecommendations(ctx, s.legacyMenu)
	}
	input, err := s.inputs.RecommendationInputs(ctx)
	if err != nil {
		return contract.Recommendations{}, err
	}
	if input.Validate() != nil {
		return contract.Recommendations{}, recommendationFailure()
	}
	risks := make(map[string]productRisk)
	for _, b := range input.RiskBatches {
		v := risks[b.ProductID]
		if v.quantity > math.MaxInt64-b.RemainingQuantity {
			return contract.Recommendations{}, recommendationFailure()
		}
		v.quantity += b.RemainingQuantity
		if v.nearest.BatchID == "" || b.ExpiresAt.Before(v.nearest.ExpiresAt) || (b.ExpiresAt.Equal(v.nearest.ExpiresAt) && b.BatchID < v.nearest.BatchID) {
			v.nearest = b
		}
		risks[b.ProductID] = v
	}
	demand := make(map[string]int64, len(input.Demand))
	for _, d := range input.Demand {
		demand[d.DishID] = d.Portions
	}
	rows := make([]scoredRecommendation, 0, len(input.Menu.Dishes))
	for _, dish := range input.Menu.Dishes {
		if err := ctx.Err(); err != nil {
			return contract.Recommendations{}, err
		}
		if !dish.IsAvailable {
			continue
		}
		recipe := map[string]int64{}
		for _, a := range dish.Allocations {
			if recipe[a.ProductID] > math.MaxInt64-a.Quantity {
				return contract.Recommendations{}, recommendationFailure()
			}
			recipe[a.ProductID] += a.Quantity
		}
		item := contract.Recommendation{DishID: dish.DishID, DishName: dish.DishName, DemandBasis: "fallback", Reasons: []contract.RecommendationReason{}}
		rate := big.NewRat(2, 1)
		if input.Coverage.Complete && demand[dish.DishID] > 0 {
			rate = big.NewRat(demand[dish.DishID], 168)
			item.DemandBasis = "history"
		}
		score := new(big.Rat)
		for id, perPortion := range recipe {
			risk, ok := risks[id]
			if !ok {
				continue
			}
			b := risk.nearest
			delta := b.ExpiresAt.Sub(input.EvaluatedAt)
			urgency := big.NewRat(int64(12*time.Hour), int64(12*time.Hour+delta))
			amount := capOne(big.NewRat(risk.quantity, perPortion))
			portions := new(big.Rat).Mul(rate, big.NewRat(int64(delta), int64(time.Hour)))
			effect := capOne(new(big.Rat).Mul(portions, big.NewRat(perPortion, risk.quantity)))
			contribution := new(big.Rat).Mul(urgency, amount)
			contribution.Mul(contribution, effect)
			score.Add(score, contribution)
			target := risk.quantity / perPortion
			if risk.quantity%perPortion != 0 {
				target++
			}
			item.Reasons = append(item.Reasons, contract.RecommendationReason{ProductID: id, ProductName: b.ProductName, BatchID: b.BatchID, QuantityPerPortion: perPortion, RiskQuantity: risk.quantity, TargetPortions: target, RemainingSeconds: b.RemainingSeconds, BaseUnit: b.BaseUnit, ExpiresAt: b.ExpiresAt})
			if item.PriorityExpiresAt.IsZero() || b.ExpiresAt.Before(item.PriorityExpiresAt) {
				item.PriorityExpiresAt = b.ExpiresAt
			}
		}
		if len(item.Reasons) == 0 {
			continue
		}
		if len(item.Reasons) > 500 {
			return contract.Recommendations{}, recommendationFailure()
		}
		sort.Slice(item.Reasons, func(i, j int) bool { return item.Reasons[i].ProductID < item.Reasons[j].ProductID })
		item.Score = score.FloatString(6)
		rows = append(rows, scoredRecommendation{item, score})
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if comparison := a.score.Cmp(b.score); comparison != 0 {
			return comparison > 0
		}
		if !a.item.PriorityExpiresAt.Equal(b.item.PriorityExpiresAt) {
			return a.item.PriorityExpiresAt.Before(b.item.PriorityExpiresAt)
		}
		return a.item.DishID < b.item.DishID
	})
	result := contract.Recommendations{EvaluatedAt: input.EvaluatedAt, StateVersion: input.StateVersion, PolicyVersion: "FIN1", HorizonHours: 72, Window: input.Window, Coverage: input.Coverage, Items: make([]contract.Recommendation, 0, min(10, len(rows)))}
	for _, row := range rows[:min(10, len(rows))] {
		result.Items = append(result.Items, row.item)
	}
	return result, nil
}

func capOne(value *big.Rat) *big.Rat {
	if value.Cmp(big.NewRat(1, 1)) > 0 {
		return big.NewRat(1, 1)
	}
	return value
}
