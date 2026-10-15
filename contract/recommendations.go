package contract

import (
	"context"
	"time"
	"unicode/utf8"
)

type RecommendationWarehouse interface {
	RecommendationInputs(context.Context) (RecommendationInputs, error)
}

type PlanningWindow struct {
	From, To time.Time
	Days     int
}

type HistoryCoverage struct {
	Epoch                        string
	CoverageFrom                 time.Time
	Complete                     bool
	RequestedFrom, AvailableFrom time.Time
}

type RiskBatch struct {
	ProductID, ProductName, BatchID string
	RemainingQuantity               int64
	BaseUnit                        string
	ExpiresAt                       time.Time
	RemainingSeconds                int64
}

type DishDemand struct {
	DishID   string
	Portions int64
}

type RecommendationInputs struct {
	EvaluatedAt  time.Time
	StateVersion int64
	HorizonHours int
	Window       PlanningWindow
	Coverage     HistoryCoverage
	Menu         MenuEvaluation
	RiskBatches  []RiskBatch
	Demand       []DishDemand
}

func (v RecommendationInputs) Validate() error {
	from := v.EvaluatedAt.Add(-168 * time.Hour)
	c := v.Coverage
	available := from
	if c.CoverageFrom.After(from) {
		available = c.CoverageFrom
	}
	if v.EvaluatedAt.IsZero() || v.StateVersion < 0 || v.HorizonHours != 72 || v.Window.Days != 7 || !v.Window.From.Equal(from) || !v.Window.To.Equal(v.EvaluatedAt) || !ValidEntityID(c.Epoch) || c.CoverageFrom.IsZero() || c.CoverageFrom.After(v.EvaluatedAt) || c.Complete != !from.Before(c.CoverageFrom) || !c.RequestedFrom.Equal(from) || !c.AvailableFrom.Equal(available) || !v.Menu.EvaluatedAt.Equal(v.EvaluatedAt) || v.Menu.StateVersion != v.StateVersion || v.Menu.Dishes == nil || v.RiskBatches == nil || v.Demand == nil || len(v.Menu.Dishes) > 10000 || len(v.RiskBatches) > 10000 || len(v.Demand) > 10000 {
		return ErrInvalidArgument
	}
	type productInfo struct{ name, unit string }
	products := map[string]productInfo{}
	text := func(s string) bool { return s != "" && utf8.ValidString(s) }
	product := func(id, name, unit string) bool {
		if !text(id) || !text(name) || (unit != "g" && unit != "ml" && unit != "pcs") {
			return false
		}
		info := productInfo{name, unit}
		if old, ok := products[id]; ok && old != info {
			return false
		}
		products[id] = info
		return true
	}
	risk := make(map[string]RiskBatch, len(v.RiskBatches))
	cutoff := v.EvaluatedAt.Add(72 * time.Hour)
	for _, b := range v.RiskBatches {
		if !product(b.ProductID, b.ProductName, b.BaseUnit) || !text(b.BatchID) || b.RemainingQuantity <= 0 || !b.ExpiresAt.After(v.EvaluatedAt) || b.ExpiresAt.After(cutoff) || b.RemainingSeconds != int64(b.ExpiresAt.Sub(v.EvaluatedAt)/time.Second) {
			return ErrInvalidArgument
		}
		if _, ok := risk[b.BatchID]; ok {
			return ErrInvalidArgument
		}
		risk[b.BatchID] = b
	}
	dishes := make(map[string]bool, len(v.Menu.Dishes))
	batchFacts := map[string]MenuAllocation{}
	for _, d := range v.Menu.Dishes {
		if !text(d.DishID) || !text(d.DishName) || dishes[d.DishID] || d.Allocations == nil || d.Deficits == nil || (d.IsAvailable && (len(d.Allocations) == 0 || len(d.Deficits) != 0)) || (!d.IsAvailable && (len(d.Allocations) != 0 || len(d.Deficits) == 0)) {
			return ErrInvalidArgument
		}
		dishes[d.DishID] = true
		allocated := map[string]bool{}
		for _, a := range d.Allocations {
			if !product(a.ProductID, a.ProductName, a.BaseUnit) || !text(a.BatchID) || a.Quantity <= 0 || !a.ExpiresAt.After(v.EvaluatedAt) || allocated[a.BatchID] {
				return ErrInvalidArgument
			}
			allocated[a.BatchID] = true
			if old, ok := batchFacts[a.BatchID]; ok && (old.ProductID != a.ProductID || old.ProductName != a.ProductName || old.BaseUnit != a.BaseUnit || !old.ExpiresAt.Equal(a.ExpiresAt)) {
				return ErrInvalidArgument
			}
			batchFacts[a.BatchID] = a
			b, exists := risk[a.BatchID]
			if !a.ExpiresAt.After(cutoff) && !exists {
				return ErrInvalidArgument
			}
			if exists && (b.ProductID != a.ProductID || b.BaseUnit != a.BaseUnit || b.ProductName != a.ProductName || !b.ExpiresAt.Equal(a.ExpiresAt) || a.Quantity > b.RemainingQuantity) {
				return ErrInvalidArgument
			}
		}
		deficits := map[string]bool{}
		for _, deficit := range d.Deficits {
			if !text(deficit.ProductID) || deficits[deficit.ProductID] || deficit.RequiredQuantity <= 0 || deficit.AvailableQuantity < 0 || deficit.AvailableQuantity >= deficit.RequiredQuantity {
				return ErrInvalidArgument
			}
			deficits[deficit.ProductID] = true
		}
	}
	demand := make(map[string]bool, len(v.Demand))
	for _, d := range v.Demand {
		if !dishes[d.DishID] || d.Portions <= 0 || demand[d.DishID] {
			return ErrInvalidArgument
		}
		demand[d.DishID] = true
	}
	return nil
}
