package pgstore

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/JustIkra/umir-serving/contract"
	"github.com/JustIkra/umir-serving/internal/domain"
)

// The stored documents keep exact instants and versions as strings (RFC 3339
// with nanoseconds, decimal): timestamptz would cut instants to microseconds,
// and a JSON number loses integers over 2^53 in other readers.

type contextDoc struct {
	OrgID           string `json:"orgID"`
	TenantID        string `json:"tenantID"`
	Epoch           string `json:"epoch"`
	TimeZone        string `json:"timeZone"`
	SettingsVersion string `json:"settingsVersion"`
	EvaluatedAt     string `json:"evaluatedAt"`
	StateVersion    string `json:"stateVersion"`
}

func formatInstant(t time.Time) string { return t.Format(time.RFC3339Nano) }

func parseInstant(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("stored instant is invalid: %w", err)
	}
	return t, nil
}

func warehouseContextDoc(c contract.WarehouseContext) contextDoc {
	doc := contextDoc{OrgID: c.OrgID, TenantID: c.TenantID, Epoch: c.Epoch, TimeZone: c.TimeZone, SettingsVersion: c.SettingsVersion, StateVersion: strconv.FormatInt(c.StateVersion, 10)}
	if !c.EvaluatedAt.IsZero() {
		doc.EvaluatedAt = formatInstant(c.EvaluatedAt)
	}
	return doc
}

func (d contextDoc) warehouseContext() (contract.WarehouseContext, error) {
	at, err := parseInstant(d.EvaluatedAt)
	if err != nil {
		return contract.WarehouseContext{}, err
	}
	version, err := strconv.ParseInt(d.StateVersion, 10, 64)
	if err != nil {
		return contract.WarehouseContext{}, fmt.Errorf("stored state version is invalid: %w", err)
	}
	return contract.WarehouseContext{OrgID: d.OrgID, TenantID: d.TenantID, Epoch: d.Epoch, TimeZone: d.TimeZone, SettingsVersion: d.SettingsVersion, EvaluatedAt: at, StateVersion: version}, nil
}

// proposalDoc is the context column of serving_proposals: the context of the
// warehouse evaluation and the exact instant and version of the proposal.
type proposalDoc struct {
	Context      contextDoc `json:"context"`
	EvaluatedAt  string     `json:"evaluatedAt"`
	StateVersion string     `json:"stateVersion"`
}

func toProposalDoc(p domain.OrderProposal) proposalDoc {
	return proposalDoc{
		Context: warehouseContextDoc(contract.WarehouseContext{
			OrgID: p.Context.OrgID, TenantID: p.Context.TenantID, Epoch: p.Context.Epoch, TimeZone: p.Context.TimeZone,
			SettingsVersion: p.Context.SettingsVersion, EvaluatedAt: p.Context.EvaluatedAt, StateVersion: p.Context.StateVersion,
		}),
		EvaluatedAt:  formatInstant(p.EvaluatedAt),
		StateVersion: strconv.FormatInt(p.StateVersion, 10),
	}
}

type positionDoc struct {
	LineNo   int    `json:"lineNo"`
	DishID   string `json:"dishID"`
	Portions string `json:"portions"`
}

type deficitDoc struct {
	ProductID         string `json:"productID"`
	RequiredQuantity  string `json:"requiredQuantity"`
	AvailableQuantity string `json:"availableQuantity"`
}

type changeDoc struct {
	LineNo            int          `json:"lineNo"`
	DishID            string       `json:"dishID"`
	RequestedPortions string       `json:"requestedPortions"`
	AcceptedPortions  string       `json:"acceptedPortions"`
	Deficits          []deficitDoc `json:"deficits"`
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

func atoi(v string) (int64, error) {
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("stored number is invalid: %w", err)
	}
	return n, nil
}

func requestedDocs(positions []domain.Position) []positionDoc {
	docs := make([]positionDoc, len(positions))
	for i, p := range positions {
		docs[i] = positionDoc{LineNo: p.LineNo, DishID: p.DishID, Portions: itoa(p.Portions)}
	}
	return docs
}

func acceptedDocs(positions []domain.AcceptedPosition) []positionDoc {
	docs := make([]positionDoc, len(positions))
	for i, p := range positions {
		docs[i] = positionDoc{LineNo: p.LineNo, DishID: p.DishID, Portions: itoa(p.Portions)}
	}
	return docs
}

func contractAcceptedDocs(positions []contract.AcceptedPosition) []positionDoc {
	docs := make([]positionDoc, len(positions))
	for i, p := range positions {
		docs[i] = positionDoc{LineNo: p.LineNo, DishID: p.DishID, Portions: itoa(p.Portions)}
	}
	return docs
}

func changeDocs(changes []domain.Change) []changeDoc {
	docs := make([]changeDoc, len(changes))
	for i, c := range changes {
		deficits := make([]deficitDoc, len(c.Deficits))
		for j, d := range c.Deficits {
			deficits[j] = deficitDoc{ProductID: d.ProductID, RequiredQuantity: itoa(d.RequiredQuantity), AvailableQuantity: itoa(d.AvailableQuantity)}
		}
		docs[i] = changeDoc{LineNo: c.LineNo, DishID: c.DishID, RequestedPortions: itoa(c.RequestedPortions), AcceptedPortions: itoa(c.AcceptedPortions), Deficits: deficits}
	}
	return docs
}

func requestedFromDocs(docs []positionDoc) ([]domain.Position, error) {
	out := make([]domain.Position, len(docs))
	for i, d := range docs {
		n, err := atoi(d.Portions)
		if err != nil {
			return nil, err
		}
		out[i] = domain.Position{LineNo: d.LineNo, DishID: d.DishID, Portions: n}
	}
	return out, nil
}

func acceptedFromDocs(docs []positionDoc) ([]domain.AcceptedPosition, error) {
	out := make([]domain.AcceptedPosition, len(docs))
	for i, d := range docs {
		n, err := atoi(d.Portions)
		if err != nil {
			return nil, err
		}
		out[i] = domain.AcceptedPosition{LineNo: d.LineNo, DishID: d.DishID, Portions: n}
	}
	return out, nil
}

func contractAcceptedFromDocs(docs []positionDoc) ([]contract.AcceptedPosition, error) {
	out := make([]contract.AcceptedPosition, len(docs))
	for i, d := range docs {
		n, err := atoi(d.Portions)
		if err != nil {
			return nil, err
		}
		out[i] = contract.AcceptedPosition{LineNo: d.LineNo, DishID: d.DishID, Portions: n}
	}
	return out, nil
}

func changesFromDocs(docs []changeDoc) ([]domain.Change, error) {
	out := make([]domain.Change, len(docs))
	for i, c := range docs {
		requested, err := atoi(c.RequestedPortions)
		if err != nil {
			return nil, err
		}
		accepted, err := atoi(c.AcceptedPortions)
		if err != nil {
			return nil, err
		}
		deficits := make([]domain.Deficit, len(c.Deficits))
		for j, d := range c.Deficits {
			required, err := atoi(d.RequiredQuantity)
			if err != nil {
				return nil, err
			}
			available, err := atoi(d.AvailableQuantity)
			if err != nil {
				return nil, err
			}
			deficits[j] = domain.Deficit{ProductID: d.ProductID, RequiredQuantity: required, AvailableQuantity: available}
		}
		out[i] = domain.Change{LineNo: c.LineNo, DishID: c.DishID, RequestedPortions: requested, AcceptedPortions: accepted, Deficits: deficits}
	}
	return out, nil
}

func marshal(value any) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode stored document: %w", err)
	}
	return data, nil
}

func unmarshal(data []byte, target any) error {
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("decode stored document: %w", err)
	}
	return nil
}
