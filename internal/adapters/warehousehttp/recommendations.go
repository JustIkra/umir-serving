package warehousehttp

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/JustIkra/umir-serving/contract"
)

type wirePlanningWindow struct {
	From string `json:"from"`
	To   string `json:"to"`
	Days *int   `json:"days"`
}

type wireHistoryCoverage struct {
	Epoch         string `json:"epoch"`
	CoverageFrom  string `json:"coverageFrom"`
	Complete      *bool  `json:"complete"`
	RequestedFrom string `json:"requestedFrom"`
	AvailableFrom string `json:"availableFrom"`
}

type wireRiskBatch struct {
	ProductID         string `json:"productID"`
	ProductName       string `json:"productName"`
	BatchID           string `json:"batchID"`
	RemainingQuantity string `json:"remainingQuantity"`
	BaseUnit          string `json:"baseUnit"`
	ExpiresAt         string `json:"expiresAt"`
	RemainingSeconds  string `json:"remainingSeconds"`
}

type wireDishDemand struct {
	DishID   string `json:"dishID"`
	Portions string `json:"portions"`
}

type wireRecommendationInputs struct {
	EvaluatedAt  string              `json:"evaluatedAt"`
	StateVersion string              `json:"stateVersion"`
	HorizonHours *int                `json:"horizonHours"`
	Window       wirePlanningWindow  `json:"window"`
	Coverage     wireHistoryCoverage `json:"coverage"`
	Menu         wireMenuEvaluation  `json:"menu"`
	RiskBatches  []wireRiskBatch     `json:"riskBatches"`
	Demand       []wireDishDemand    `json:"demand"`
}

func validateFINEnvelope(data []byte) error {
	var envelope struct {
		Context struct {
			OrgID        string       `json:"orgID"`
			TenantID     string       `json:"tenantID"`
			RuntimeEpoch string       `json:"runtimeEpoch"`
			Settings     wireSettings `json:"settings"`
			EvaluatedAt  string       `json:"evaluatedAt"`
			StateVersion string       `json:"stateVersion"`
		} `json:"context"`
		Result json.RawMessage `json:"result"`
	}
	return decodeStrict(data, &envelope)
}

func (c *Client) RecommendationInputs(ctx context.Context) (contract.RecommendationInputs, error) {
	if c.scoped == nil {
		return contract.RecommendationInputs{}, &contract.OperationError{Code: "RuntimeNotReady", Message: "FIN1 recommendations require a scoped Warehouse client.", Outcome: "unavailable"}
	}
	var wire wireRecommendationInputs
	if err := c.get(ctx, "/recommendation-inputs", nil, &wire); err != nil {
		return contract.RecommendationInputs{}, err
	}
	inputs, err := c.recommendationInputsFromWire(wire)
	return inputs, c.observeProtocolError(ctx, err)
}

func (c *Client) recommendationInputsFromWire(v wireRecommendationInputs) (contract.RecommendationInputs, error) {
	invalid := protocolError(errors.New("invalid FIN1 recommendation inputs"))
	if v.HorizonHours == nil || v.Window.Days == nil || v.Coverage.Complete == nil || v.RiskBatches == nil || v.Demand == nil {
		return contract.RecommendationInputs{}, invalid
	}
	at, e1 := c.parseTimestamp(v.EvaluatedAt)
	version, e2 := parseInt64(v.StateVersion, false)
	from, e3 := c.parseTimestamp(v.Window.From)
	to, e4 := c.parseTimestamp(v.Window.To)
	coverageFrom, e5 := c.parseTimestamp(v.Coverage.CoverageFrom)
	requestedFrom, e6 := c.parseTimestamp(v.Coverage.RequestedFrom)
	availableFrom, e7 := c.parseTimestamp(v.Coverage.AvailableFrom)
	menu, e8 := c.menuFromWire(v.Menu)
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil || e5 != nil || e6 != nil || e7 != nil || e8 != nil || c.scoped == nil || v.Coverage.Epoch != c.scoped.Epoch {
		return contract.RecommendationInputs{}, invalid
	}
	result := contract.RecommendationInputs{EvaluatedAt: at, StateVersion: version, HorizonHours: *v.HorizonHours, Window: contract.PlanningWindow{From: from, To: to, Days: *v.Window.Days}, Coverage: contract.HistoryCoverage{Epoch: v.Coverage.Epoch, CoverageFrom: coverageFrom, Complete: *v.Coverage.Complete, RequestedFrom: requestedFrom, AvailableFrom: availableFrom}, Menu: menu, RiskBatches: make([]contract.RiskBatch, 0, len(v.RiskBatches)), Demand: make([]contract.DishDemand, 0, len(v.Demand))}
	for _, b := range v.RiskBatches {
		quantity, e1 := parseInt64(b.RemainingQuantity, true)
		expiry, e2 := c.parseTimestamp(b.ExpiresAt)
		seconds, e3 := parseInt64(b.RemainingSeconds, false)
		if e1 != nil || e2 != nil || e3 != nil {
			return contract.RecommendationInputs{}, invalid
		}
		result.RiskBatches = append(result.RiskBatches, contract.RiskBatch{ProductID: b.ProductID, ProductName: b.ProductName, BatchID: b.BatchID, RemainingQuantity: quantity, BaseUnit: b.BaseUnit, ExpiresAt: expiry, RemainingSeconds: seconds})
	}
	for _, d := range v.Demand {
		portions, err := parseInt64(d.Portions, true)
		if err != nil {
			return contract.RecommendationInputs{}, invalid
		}
		result.Demand = append(result.Demand, contract.DishDemand{DishID: d.DishID, Portions: portions})
	}
	if result.Validate() != nil {
		return contract.RecommendationInputs{}, invalid
	}
	return result, nil
}
