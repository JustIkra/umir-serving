package application

import (
	"context"
	"errors"
	"sort"

	"github.com/JustIkra/umir-serving/contract"
)

type RecommendationsService struct {
	menu contract.MenuWarehouse
}

func NewRecommendationsService(menu contract.MenuWarehouse) *RecommendationsService {
	return &RecommendationsService{menu: menu}
}

func (s *RecommendationsService) GetRecommendations(ctx context.Context) (contract.Recommendations, error) {
	evaluation, err := s.menu.EvaluateMenu(ctx)
	if err != nil {
		return contract.Recommendations{}, err
	}
	result := contract.Recommendations{
		EvaluatedAt:  evaluation.EvaluatedAt,
		StateVersion: evaluation.StateVersion,
		Items:        make([]contract.Recommendation, 0, len(evaluation.Dishes)),
	}
	for _, dish := range evaluation.Dishes {
		if !dish.IsAvailable {
			continue
		}
		if len(dish.Allocations) == 0 {
			return contract.Recommendations{}, errors.New("available dish has no allocations")
		}
		priorityExpiresAt := dish.Allocations[0].ExpiresAt
		for _, allocation := range dish.Allocations[1:] {
			if allocation.ExpiresAt.Before(priorityExpiresAt) {
				priorityExpiresAt = allocation.ExpiresAt
			}
		}
		reasons := make([]contract.RecommendationReason, 0, len(dish.Allocations))
		for _, allocation := range dish.Allocations {
			if !allocation.ExpiresAt.Equal(priorityExpiresAt) {
				continue
			}
			reasons = append(reasons, contract.RecommendationReason{
				ProductID:          allocation.ProductID,
				ProductName:        allocation.ProductName,
				BatchID:            allocation.BatchID,
				QuantityPerPortion: allocation.Quantity,
				BaseUnit:           allocation.BaseUnit,
				ExpiresAt:          allocation.ExpiresAt,
			})
		}
		sort.Slice(reasons, func(i, j int) bool {
			if reasons[i].ProductID != reasons[j].ProductID {
				return reasons[i].ProductID < reasons[j].ProductID
			}
			return reasons[i].BatchID < reasons[j].BatchID
		})
		result.Items = append(result.Items, contract.Recommendation{
			DishID:            dish.DishID,
			DishName:          dish.DishName,
			PriorityExpiresAt: priorityExpiresAt,
			Reasons:           reasons,
		})
	}
	sort.Slice(result.Items, func(i, j int) bool {
		if !result.Items[i].PriorityExpiresAt.Equal(result.Items[j].PriorityExpiresAt) {
			return result.Items[i].PriorityExpiresAt.Before(result.Items[j].PriorityExpiresAt)
		}
		return result.Items[i].DishID < result.Items[j].DishID
	})
	return result, nil
}
