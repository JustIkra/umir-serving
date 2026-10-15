package httpapi

import (
	"fmt"
	"strconv"
	"time"

	"github.com/JustIkra/umir-serving/contract"
)

type productWire struct {
	ProductID string `json:"productID"`
	Name      string `json:"name"`
	BaseUnit  string `json:"baseUnit"`
}

type ingredientWire struct {
	ProductID string `json:"productID"`
	Quantity  string `json:"quantity"`
	BaseUnit  string `json:"baseUnit,omitempty"`
}

type dishWire struct {
	DishID      string           `json:"dishID"`
	Name        string           `json:"name"`
	Ingredients []ingredientWire `json:"ingredients"`
}

type positionWire struct {
	LineNo   string `json:"lineNo"`
	DishID   string `json:"dishID"`
	Portions string `json:"portions"`
}

type deficitWire struct {
	ProductID         string `json:"productID"`
	RequiredQuantity  string `json:"requiredQuantity"`
	AvailableQuantity string `json:"availableQuantity"`
}

type changeWire struct {
	LineNo            string        `json:"lineNo"`
	DishID            string        `json:"dishID"`
	RequestedPortions string        `json:"requestedPortions"`
	AcceptedPortions  string        `json:"acceptedPortions"`
	Deficits          []deficitWire `json:"deficits"`
}

type proposalWire struct {
	WarehouseEpoch     string         `json:"warehouseEpoch,omitempty"`
	ProposalID         string         `json:"proposalID"`
	EvaluatedAt        string         `json:"evaluatedAt"`
	StateVersion       string         `json:"stateVersion"`
	RequestedPositions []positionWire `json:"requestedPositions"`
	AcceptedPositions  []positionWire `json:"acceptedPositions"`
	Changes            []changeWire   `json:"changes"`
}

type allocationWire struct {
	ProductID string `json:"productID"`
	BatchID   string `json:"batchID"`
	Quantity  string `json:"quantity"`
}

type orderWire struct {
	OrderID           string           `json:"orderID"`
	ProposalID        string           `json:"proposalID"`
	AcceptedPositions []positionWire   `json:"acceptedPositions"`
	Allocations       []allocationWire `json:"allocations"`
	PlacedAt          string           `json:"placedAt"`
}

type productsWire struct {
	EvaluatedAt  string        `json:"evaluatedAt"`
	StateVersion string        `json:"stateVersion"`
	Items        []productWire `json:"items"`
}

type dishCatalogWire struct {
	EvaluatedAt  string     `json:"evaluatedAt"`
	StateVersion string     `json:"stateVersion"`
	Items        []dishWire `json:"items"`
}

type dishDetailsWire struct {
	EvaluatedAt  string   `json:"evaluatedAt"`
	StateVersion string   `json:"stateVersion"`
	Dish         dishWire `json:"dish"`
}

type menuDishWire struct {
	DishID string `json:"dishID"`
	Name   string `json:"name"`
}

type menuWire struct {
	EvaluatedAt  string         `json:"evaluatedAt"`
	StateVersion string         `json:"stateVersion"`
	Dishes       []menuDishWire `json:"dishes"`
}

type stopDishWire struct {
	DishID   string        `json:"dishID"`
	Name     string        `json:"name"`
	Deficits []deficitWire `json:"deficits"`
}

type stopListWire struct {
	EvaluatedAt  string         `json:"evaluatedAt"`
	StateVersion string         `json:"stateVersion"`
	Dishes       []stopDishWire `json:"dishes"`
}

type recommendationReasonWire struct {
	ProductID          string `json:"productID"`
	ProductName        string `json:"productName"`
	BatchID            string `json:"batchID"`
	QuantityPerPortion string `json:"quantityPerPortion"`
	RiskQuantity       string `json:"riskQuantity,omitempty"`
	TargetPortions     string `json:"targetPortions,omitempty"`
	RemainingSeconds   string `json:"remainingSeconds,omitempty"`
	BaseUnit           string `json:"baseUnit"`
	ExpiresAt          string `json:"expiresAt"`
}

type recommendationWire struct {
	DishID            string                     `json:"dishID"`
	DishName          string                     `json:"dishName"`
	Score             string                     `json:"score,omitempty"`
	DemandBasis       string                     `json:"demandBasis,omitempty"`
	PriorityExpiresAt string                     `json:"priorityExpiresAt"`
	Reasons           []recommendationReasonWire `json:"reasons"`
}

type recommendationsWire struct {
	EvaluatedAt   string               `json:"evaluatedAt"`
	StateVersion  string               `json:"stateVersion"`
	PolicyVersion string               `json:"policyVersion,omitempty"`
	HorizonHours  int                  `json:"horizonHours,omitempty"`
	Window        *planningWindowWire  `json:"window,omitempty"`
	Coverage      *historyCoverageWire `json:"coverage,omitempty"`
	Items         []recommendationWire `json:"items"`
}

type planningWindowWire struct {
	From string `json:"from"`
	To   string `json:"to"`
	Days int    `json:"days"`
}

type historyCoverageWire struct {
	Epoch         string `json:"epoch"`
	CoverageFrom  string `json:"coverageFrom"`
	Complete      bool   `json:"complete"`
	RequestedFrom string `json:"requestedFrom"`
	AvailableFrom string `json:"availableFrom"`
}

type confirmWire struct {
	Kind         contract.ConfirmResultKind `json:"kind"`
	Order        *orderWire                 `json:"order,omitempty"`
	Proposal     *proposalWire              `json:"proposal,omitempty"`
	SupersededBy string                     `json:"supersededBy,omitempty"`
}

type errorWire struct {
	Retryable *bool  `json:"retryable,omitempty"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	Outcome   string `json:"outcome"`
}

type registerProductInput struct {
	CommandID string `json:"commandID"`
	ProductID string `json:"productID"`
	Name      string `json:"name"`
	BaseUnit  string `json:"baseUnit"`
}

type defineDishInput struct {
	CommandID   string            `json:"commandID"`
	DishID      string            `json:"dishID"`
	Name        string            `json:"name"`
	Ingredients []ingredientInput `json:"ingredients"`
}

type ingredientInput struct {
	ProductID string  `json:"productID"`
	Quantity  string  `json:"quantity"`
	BaseUnit  *string `json:"baseUnit,omitempty"`
}

type preparePositionInput struct {
	DishID   string `json:"dishID"`
	Portions string `json:"portions"`
}

type prepareOrderInput struct {
	Positions []preparePositionInput `json:"positions"`
}
type confirmInput struct {
	CommandID  string `json:"commandID"`
	ProposalID string `json:"proposalID"`
}

func productToWire(value contract.Product) productWire {
	return productWire{ProductID: value.ProductID, Name: value.Name, BaseUnit: value.BaseUnit}
}

func dishToWire(value contract.Dish) dishWire {
	result := dishWire{DishID: value.DishID, Name: value.Name, Ingredients: make([]ingredientWire, len(value.Recipe))}
	for index, ingredient := range value.Recipe {
		result.Ingredients[index] = ingredientWire{ProductID: ingredient.ProductID, Quantity: strconv.FormatInt(ingredient.Quantity, 10), BaseUnit: ingredient.BaseUnit}
	}
	return result
}

func proposalToWire(value contract.OrderProposal) proposalWire {
	result := proposalWire{WarehouseEpoch: value.WarehouseEpoch, ProposalID: value.ProposalID, EvaluatedAt: formatTime(value.EvaluatedAt), StateVersion: strconv.FormatInt(value.StateVersion, 10), RequestedPositions: positionsToWire(value.RequestedPositions), AcceptedPositions: acceptedToWire(value.AcceptedPositions), Changes: make([]changeWire, len(value.Changes))}
	for index, change := range value.Changes {
		result.Changes[index] = changeWire{LineNo: strconv.Itoa(change.LineNo), DishID: change.DishID, RequestedPortions: strconv.FormatInt(change.RequestedPortions, 10), AcceptedPortions: strconv.FormatInt(change.AcceptedPortions, 10), Deficits: deficitsToWire(change.Deficits)}
	}
	return result
}

func orderToWire(value contract.Order) orderWire {
	result := orderWire{OrderID: value.OrderID, ProposalID: value.ProposalID, AcceptedPositions: acceptedToWire(value.Positions), Allocations: make([]allocationWire, len(value.Allocations)), PlacedAt: formatTime(value.PlacedAt)}
	for index, allocation := range value.Allocations {
		result.Allocations[index] = allocationWire{ProductID: allocation.ProductID, BatchID: allocation.BatchID, Quantity: strconv.FormatInt(allocation.Quantity, 10)}
	}
	return result
}

func positionsToWire(values []contract.Position) []positionWire {
	result := make([]positionWire, len(values))
	for index, value := range values {
		result[index] = positionWire{LineNo: strconv.Itoa(value.LineNo), DishID: value.DishID, Portions: strconv.FormatInt(value.Portions, 10)}
	}
	return result
}

func acceptedToWire(values []contract.AcceptedPosition) []positionWire {
	result := make([]positionWire, len(values))
	for index, value := range values {
		result[index] = positionWire{LineNo: strconv.Itoa(value.LineNo), DishID: value.DishID, Portions: strconv.FormatInt(value.Portions, 10)}
	}
	return result
}

func deficitsToWire(values []contract.Deficit) []deficitWire {
	result := make([]deficitWire, len(values))
	for index, value := range values {
		result[index] = deficitWire{ProductID: value.ProductID, RequiredQuantity: strconv.FormatInt(value.RequiredQuantity, 10), AvailableQuantity: strconv.FormatInt(value.AvailableQuantity, 10)}
	}
	return result
}

func confirmToWire(value contract.ConfirmResult) (confirmWire, error) {
	result := confirmWire{Kind: value.Kind}
	switch value.Kind {
	case contract.OrderConfirmed:
		if value.Order == nil || value.Proposal != nil || value.SupersededBy != "" {
			return confirmWire{}, fmt.Errorf("invalid OrderConfirmed result")
		}
		order := orderToWire(*value.Order)
		result.Order = &order
	case contract.RequiresConfirmation:
		if value.Proposal == nil || value.Order != nil || value.SupersededBy != "" {
			return confirmWire{}, fmt.Errorf("invalid RequiresConfirmation result")
		}
		proposal := proposalToWire(*value.Proposal)
		result.Proposal = &proposal
	case contract.EmptyOrder:
		if value.Order != nil || value.SupersededBy != "" {
			return confirmWire{}, fmt.Errorf("invalid EmptyOrder result")
		}
	case contract.ConfirmationPending:
		if value.Order != nil || value.Proposal != nil || value.SupersededBy != "" {
			return confirmWire{}, fmt.Errorf("invalid empty confirmation result")
		}
	case contract.ProposalSuperseded:
		if value.SupersededBy == "" || value.Order != nil || value.Proposal != nil {
			return confirmWire{}, fmt.Errorf("invalid ProposalSuperseded result")
		}
		result.SupersededBy = value.SupersededBy
	default:
		return confirmWire{}, fmt.Errorf("unknown confirmation result kind %q", value.Kind)
	}
	return result, nil
}

func formatTime(value time.Time) string {
	return value.In(time.FixedZone("MSK", 3*60*60)).Format(time.RFC3339Nano)
}

func parsePositiveInt64(value string) (int64, error) { return parseWireInt64(value, true) }

func parseWireInt64(value string, positive bool) (int64, error) {
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || strconv.FormatInt(parsed, 10) != value || (positive && parsed <= 0) || (!positive && parsed < 0) {
		return 0, fmt.Errorf("invalid canonical int64")
	}
	return parsed, nil
}
