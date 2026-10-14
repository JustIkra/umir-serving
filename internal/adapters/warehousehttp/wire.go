package warehousehttp

import (
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/JustIkra/umir-serving/contract"
)

type wireProduct struct {
	ProductID string `json:"productID"`
	Name      string `json:"name"`
	BaseUnit  string `json:"baseUnit"`
}

type wireIngredient struct {
	ProductID string `json:"productID"`
	Quantity  string `json:"quantity"`
	BaseUnit  string `json:"baseUnit,omitempty"`
}

type wireDish struct {
	DishID string           `json:"dishID"`
	Name   string           `json:"name"`
	Recipe []wireIngredient `json:"ingredients"`
}

type wirePosition struct {
	LineNo   string `json:"lineNo"`
	DishID   string `json:"dishID"`
	Portions string `json:"portions"`
}

type wireAcceptedPosition = wirePosition

type wireDeficit struct {
	ProductID         string `json:"productID"`
	RequiredQuantity  string `json:"requiredQuantity"`
	AvailableQuantity string `json:"availableQuantity"`
}

type wireChange struct {
	LineNo            string        `json:"lineNo"`
	DishID            string        `json:"dishID"`
	RequestedPortions string        `json:"requestedPortions"`
	AcceptedPortions  string        `json:"acceptedPortions"`
	Deficits          []wireDeficit `json:"deficits"`
}

type wireOrderEvaluation struct {
	EvaluatedAt        string                 `json:"evaluatedAt"`
	StateVersion       string                 `json:"stateVersion"`
	RequestedPositions []wirePosition         `json:"requestedPositions"`
	AcceptedPositions  []wireAcceptedPosition `json:"acceptedPositions"`
	Changes            []wireChange           `json:"changes"`
}

type wireMenuAllocation struct {
	ProductID   string `json:"productID"`
	ProductName string `json:"productName"`
	BatchID     string `json:"batchID"`
	Quantity    string `json:"quantity"`
	BaseUnit    string `json:"baseUnit"`
	ExpiresAt   string `json:"expiresAt"`
}

type wireDishEvaluation struct {
	DishID      string               `json:"dishID"`
	DishName    string               `json:"dishName"`
	IsAvailable *bool                `json:"isAvailable"`
	Deficits    []wireDeficit        `json:"deficits"`
	Allocations []wireMenuAllocation `json:"allocations"`
}

type wireMenuEvaluation struct {
	EvaluatedAt  string               `json:"evaluatedAt"`
	StateVersion string               `json:"stateVersion"`
	Dishes       []wireDishEvaluation `json:"dishes"`
}

type wireAllocation struct {
	ProductID string `json:"productID"`
	BatchID   string `json:"batchID"`
	Quantity  string `json:"quantity"`
}

type wireOrder struct {
	OrderID           string                 `json:"orderID"`
	ProposalID        string                 `json:"proposalID"`
	AcceptedPositions []wireAcceptedPosition `json:"acceptedPositions"`
	Allocations       []wireAllocation       `json:"allocations"`
	PlacedAt          string                 `json:"placedAt"`
}

type wireCommitResult struct {
	Kind       contract.CommitResultKind `json:"kind"`
	Order      *wireOrder                `json:"order,omitempty"`
	Evaluation *wireOrderEvaluation      `json:"evaluation,omitempty"`
}

type wireRecordedIngredient struct {
	ProductID string `json:"productID"`
	Quantity  string `json:"quantity"`
}

type wireRecordedDish struct {
	DishID      string                   `json:"dishID"`
	Name        string                   `json:"name"`
	Ingredients []wireRecordedIngredient `json:"ingredients"`
}

type wireCommandResult struct {
	Kind     contract.CommandResultKind `json:"kind"`
	Product  *wireProduct               `json:"product,omitempty"`
	Dish     *wireRecordedDish          `json:"dish,omitempty"`
	Delivery *wireDeliveryReceipt       `json:"delivery,omitempty"`
	Order    *wireOrder                 `json:"order,omitempty"`
	Expiry   *wireExpiryResult          `json:"expiry,omitempty"`
}

type wireCommandRecord struct {
	CommandID          string            `json:"commandID"`
	CommandType        string            `json:"commandType"`
	CommandFingerprint string            `json:"commandFingerprint"`
	Result             wireCommandResult `json:"result"`
}

type wireDeliveryReceipt struct {
	DeliveryID string   `json:"deliveryID"`
	BatchIDs   []string `json:"batchIDs"`
}

type wireExpiryResult struct {
	BatchID            string `json:"batchID"`
	ProductID          string `json:"productID"`
	WrittenOffQuantity string `json:"writtenOffQuantity"`
	Reason             string `json:"reason"`
	ExpiresAt          string `json:"expiresAt"`
	ProcessedAt        string `json:"processedAt"`
}

type wireAttemptError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Outcome string `json:"outcome"`
}

type wireOrderAttempt struct {
	State  contract.OrderAttemptState `json:"state"`
	Result *wireCommitResult          `json:"result,omitempty"`
	Error  *wireAttemptError          `json:"error,omitempty"`
}

var moscowTimestampPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T(?:[01]\d|2[0-3]):[0-5]\d:[0-5]\d(?:\.[0-9]{1,9})?\+03:00$`)

func wirePositionFromContract(position contract.Position) wirePosition {
	return wirePosition{LineNo: strconv.Itoa(position.LineNo), DishID: position.DishID, Portions: strconv.FormatInt(position.Portions, 10)}
}

func wireAcceptedPositionFromContract(position contract.AcceptedPosition) wireAcceptedPosition {
	return wirePositionFromContract(contract.Position(position))
}

func positionFromWire(position wirePosition) (contract.Position, error) {
	lineNo, err := parseInt(position.LineNo)
	if err != nil {
		return contract.Position{}, err
	}
	portions, err := parseInt64(position.Portions, true)
	if err != nil {
		return contract.Position{}, err
	}
	if position.DishID == "" {
		return contract.Position{}, fmt.Errorf("dishID is empty")
	}
	return contract.Position{LineNo: lineNo, DishID: position.DishID, Portions: portions}, nil
}

func acceptedPositionFromWire(position wireAcceptedPosition) (contract.AcceptedPosition, error) {
	parsed, err := positionFromWire(position)
	return contract.AcceptedPosition(parsed), err
}

func parseInt(value string) (int, error) {
	parsed, err := strconv.ParseInt(value, 10, strconv.IntSize)
	if err != nil || parsed <= 0 || strconv.FormatInt(parsed, 10) != value {
		return 0, fmt.Errorf("invalid canonical int: %q", value)
	}
	return int(parsed), nil
}

func parseInt64(value string, positive bool) (int64, error) {
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || strconv.FormatInt(parsed, 10) != value || (positive && parsed <= 0) || (!positive && parsed < 0) {
		return 0, fmt.Errorf("invalid canonical int64: %q", value)
	}
	return parsed, nil
}

func parseMoscowTime(value string) (time.Time, error) {
	if !moscowTimestampPattern.MatchString(value) {
		return time.Time{}, fmt.Errorf("timestamp must use +03:00: %q", value)
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid Moscow timestamp: %q", value)
	}
	return parsed, nil
}

func productFromWire(value wireProduct) (contract.Product, error) {
	if value.ProductID == "" || value.Name == "" || value.BaseUnit == "" {
		return contract.Product{}, fmt.Errorf("invalid product payload")
	}
	return contract.Product{ProductID: value.ProductID, Name: value.Name, BaseUnit: value.BaseUnit}, nil
}

func dishFromWire(value wireDish) (contract.Dish, error) {
	if value.DishID == "" || value.Name == "" || value.Recipe == nil {
		return contract.Dish{}, fmt.Errorf("invalid dish payload")
	}
	dish := contract.Dish{DishID: value.DishID, Name: value.Name, Recipe: make([]contract.Ingredient, len(value.Recipe))}
	for index, ingredient := range value.Recipe {
		quantity, err := parseInt64(ingredient.Quantity, true)
		if err != nil || ingredient.ProductID == "" {
			return contract.Dish{}, fmt.Errorf("invalid dish ingredient")
		}
		dish.Recipe[index] = contract.Ingredient{ProductID: ingredient.ProductID, Quantity: quantity, BaseUnit: ingredient.BaseUnit}
	}
	return dish, nil
}

func evaluationFromWire(value wireOrderEvaluation, parseTime func(string) (time.Time, error)) (contract.OrderEvaluation, error) {
	evaluatedAt, err := parseTime(value.EvaluatedAt)
	if err != nil {
		return contract.OrderEvaluation{}, err
	}
	version, err := parseInt64(value.StateVersion, false)
	if err != nil {
		return contract.OrderEvaluation{}, err
	}
	evaluation := contract.OrderEvaluation{EvaluatedAt: evaluatedAt, StateVersion: version}
	if value.RequestedPositions == nil || value.AcceptedPositions == nil || value.Changes == nil {
		return contract.OrderEvaluation{}, fmt.Errorf("order evaluation arrays must be present")
	}
	evaluation.RequestedPositions = make([]contract.Position, len(value.RequestedPositions))
	for index, position := range value.RequestedPositions {
		evaluation.RequestedPositions[index], err = positionFromWire(position)
		if err != nil {
			return contract.OrderEvaluation{}, err
		}
	}
	evaluation.AcceptedPositions = make([]contract.AcceptedPosition, len(value.AcceptedPositions))
	for index, position := range value.AcceptedPositions {
		evaluation.AcceptedPositions[index], err = acceptedPositionFromWire(position)
		if err != nil {
			return contract.OrderEvaluation{}, err
		}
	}
	evaluation.Changes = make([]contract.Change, len(value.Changes))
	for index, change := range value.Changes {
		lineNo, parseErr := parseInt(change.LineNo)
		if parseErr != nil {
			return contract.OrderEvaluation{}, parseErr
		}
		requested, parseErr := parseInt64(change.RequestedPortions, true)
		if parseErr != nil {
			return contract.OrderEvaluation{}, parseErr
		}
		accepted, parseErr := parseInt64(change.AcceptedPortions, false)
		if parseErr != nil {
			return contract.OrderEvaluation{}, parseErr
		}
		deficits, parseErr := deficitsFromWire(change.Deficits)
		if parseErr != nil {
			return contract.OrderEvaluation{}, parseErr
		}
		evaluation.Changes[index] = contract.Change{LineNo: lineNo, DishID: change.DishID, RequestedPortions: requested, AcceptedPortions: accepted, Deficits: deficits}
	}
	return evaluation, nil
}

func deficitsFromWire(values []wireDeficit) ([]contract.Deficit, error) {
	if values == nil {
		return nil, fmt.Errorf("deficits must be an array")
	}
	result := make([]contract.Deficit, len(values))
	for index, value := range values {
		required, err := parseInt64(value.RequiredQuantity, true)
		if err != nil {
			return nil, err
		}
		available, err := parseInt64(value.AvailableQuantity, false)
		if err != nil || value.ProductID == "" {
			return nil, fmt.Errorf("invalid deficit")
		}
		result[index] = contract.Deficit{ProductID: value.ProductID, RequiredQuantity: required, AvailableQuantity: available}
	}
	return result, nil
}

var instantPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T(?:[01]\d|2[0-3]):[0-5]\d:[0-5]\d(?:\.[0-9]{1,9})?(?:Z|[+-](?:[01]\d|2[0-3]):[0-5]\d)$`)

func parseInstant(value string) (time.Time, error) {
	if !instantPattern.MatchString(value) {
		return time.Time{}, fmt.Errorf("invalid explicit instant")
	}
	return time.Parse(time.RFC3339Nano, value)
}
func (c *Client) parseTimestamp(value string) (time.Time, error) {
	if c.scoped != nil {
		return parseInstant(value)
	}
	return parseMoscowTime(value)
}
