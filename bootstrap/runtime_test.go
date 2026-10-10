package bootstrap

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/JustIkra/umir-serving/contract"
)

type warehouseStub struct {
	evaluation contract.OrderEvaluation
	menu       contract.MenuEvaluation
	catalog    contract.DishCatalog
	products   contract.Products
	commit     contract.CommitResult
	committed  []contract.AcceptedPosition
}

func (w *warehouseStub) EvaluateOrder(_ context.Context, request contract.EvaluateOrderRequest) (contract.OrderEvaluation, error) {
	if len(request.Positions) != 1 || request.Positions[0].DishID != "soup" || request.Positions[0].LineNo != 1 {
		return contract.OrderEvaluation{}, errors.New("unexpected order evaluation request")
	}
	return w.evaluation, nil
}

func (w *warehouseStub) CommitAcceptedOrder(_ context.Context, request contract.CommitOrderRequest) (contract.CommitResult, error) {
	w.committed = append([]contract.AcceptedPosition(nil), request.AcceptedPositions...)
	return w.commit, nil
}

func (*warehouseStub) FindRecordedCommand(context.Context, string) (contract.CommandRecord, bool, error) {
	return contract.CommandRecord{}, false, nil
}

func (*warehouseStub) FindOrderByProposal(context.Context, string) (contract.Order, bool, error) {
	return contract.Order{}, false, nil
}

func (w *warehouseStub) GetProducts(context.Context) (contract.Products, error) {
	return w.products, nil
}

func (w *warehouseStub) GetDishCatalog(context.Context) (contract.DishCatalog, error) {
	return w.catalog, nil
}

func (*warehouseStub) GetDish(context.Context, string) (contract.Dish, error) {
	return contract.Dish{}, nil
}

func (*warehouseStub) RegisterProduct(context.Context, contract.RegisterProductRequest) (contract.Product, error) {
	return contract.Product{}, nil
}

func (*warehouseStub) DefineDish(context.Context, contract.DefineDishRequest) (contract.Dish, error) {
	return contract.Dish{}, nil
}

func (w *warehouseStub) EvaluateMenu(context.Context) (contract.MenuEvaluation, error) {
	return w.menu, nil
}

func TestNewInMemoryRejectsInvalidOptions(t *testing.T) {
	base := validOptions(&warehouseStub{})
	var nilOrders *warehouseStub
	var nilCatalog *warehouseStub
	var nilMenu *warehouseStub

	tests := []struct {
		name string
		edit func(*Options)
	}{
		{name: "nil clock", edit: func(options *Options) { options.Now = nil }},
		{name: "nil id generator", edit: func(options *Options) { options.NewID = nil }},
		{name: "nil orders port", edit: func(options *Options) { options.OrdersWarehouse = nil }},
		{name: "nil catalog port", edit: func(options *Options) { options.CatalogWarehouse = nil }},
		{name: "nil menu port", edit: func(options *Options) { options.MenuWarehouse = nil }},
		{name: "typed nil orders port", edit: func(options *Options) { options.OrdersWarehouse = nilOrders }},
		{name: "typed nil catalog port", edit: func(options *Options) { options.CatalogWarehouse = nilCatalog }},
		{name: "typed nil menu port", edit: func(options *Options) { options.MenuWarehouse = nilMenu }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options := base
			test.edit(&options)
			runtime, err := NewInMemory(options)
			if err == nil || runtime != nil {
				t.Fatalf("NewInMemory() = (%v, %v), want (nil, error)", runtime, err)
			}
		})
	}
}

func TestNewInMemoryDoesNotConsumeDependencies(t *testing.T) {
	warehouse := &warehouseStub{}
	nowCalls := 0
	idCalls := 0
	options := validOptions(warehouse)
	options.Now = func() time.Time {
		nowCalls++
		return time.Date(2026, 10, 10, 12, 0, 0, 0, time.FixedZone("MSK", 3*60*60))
	}
	options.NewID = func() string {
		idCalls++
		return "proposal-1"
	}

	if _, err := NewInMemory(options); err != nil {
		t.Fatalf("NewInMemory() error = %v", err)
	}
	if nowCalls != 0 || idCalls != 0 {
		t.Fatalf("constructor consumed dependencies: clock=%d id=%d", nowCalls, idCalls)
	}
}

func TestRuntimeServicesPreserveProposalDataAndReturnCatalogCopies(t *testing.T) {
	at := time.Date(2026, 10, 10, 12, 0, 0, 0, time.FixedZone("MSK", 3*60*60))
	warehouse := &warehouseStub{
		evaluation: contract.OrderEvaluation{
			EvaluatedAt:        at,
			StateVersion:       7,
			RequestedPositions: []contract.Position{{LineNo: 1, DishID: "soup", Portions: 3}},
			AcceptedPositions:  []contract.AcceptedPosition{{LineNo: 1, DishID: "soup", Portions: 2}},
			Changes:            []contract.Change{{LineNo: 1, DishID: "soup", RequestedPortions: 3, AcceptedPortions: 2, Deficits: []contract.Deficit{{ProductID: "tomato", RequiredQuantity: 3000, AvailableQuantity: 2000}}}},
		},
		commit:  contract.CommitResult{Kind: contract.OrderCommitted, Order: &contract.Order{OrderID: "order-1", ProposalID: "proposal-1", Positions: []contract.AcceptedPosition{{LineNo: 1, DishID: "soup", Portions: 2}}, PlacedAt: at}},
		catalog: contract.DishCatalog{{DishID: "soup", Name: "Soup", Recipe: []contract.Ingredient{{ProductID: "tomato", Quantity: 1000, BaseUnit: "g"}}}},
	}
	options := validOptions(warehouse)
	options.Now = func() time.Time { return at }
	idCalls := 0
	options.NewID = func() string {
		idCalls++
		return "proposal-1"
	}
	runtime, err := NewInMemory(options)
	if err != nil {
		t.Fatalf("NewInMemory() error = %v", err)
	}

	proposal, err := runtime.PrepareOrder(context.Background(), contract.PrepareOrderRequest{Positions: []contract.Position{{DishID: "soup", Portions: 3}}})
	if err != nil {
		t.Fatalf("PrepareOrder() error = %v", err)
	}
	proposal.AcceptedPositions[0].Portions = 99
	proposal.Changes[0].Deficits[0].AvailableQuantity = 99
	confirmed, err := runtime.ConfirmOrder(context.Background(), contract.ConfirmOrderRequest{CommandID: "cmd-1", ProposalID: "proposal-1"})
	if err != nil {
		t.Fatalf("ConfirmOrder() error = %v", err)
	}
	if confirmed.Kind != contract.OrderConfirmed || confirmed.Order == nil || confirmed.Order.OrderID != "order-1" {
		t.Fatalf("ConfirmOrder() = %#v, want order-1 confirmation", confirmed)
	}
	if want := []contract.AcceptedPosition{{LineNo: 1, DishID: "soup", Portions: 2}}; !reflect.DeepEqual(warehouse.committed, want) {
		t.Fatalf("warehouse received accepted positions %#v, want %#v", warehouse.committed, want)
	}

	catalog, err := runtime.GetDishCatalog(context.Background())
	if err != nil {
		t.Fatalf("GetDishCatalog() error = %v", err)
	}
	catalog[0].Recipe[0].Quantity = 9000
	again, err := runtime.GetDishCatalog(context.Background())
	if err != nil {
		t.Fatalf("second GetDishCatalog() error = %v", err)
	}
	if again[0].Recipe[0].Quantity != 1000 {
		t.Fatalf("catalog mutation leaked into later result: quantity=%d", again[0].Recipe[0].Quantity)
	}
	if idCalls != 1 {
		t.Fatalf("ID generator calls = %d, want 1 after one proposal", idCalls)
	}
}

func validOptions(warehouse *warehouseStub) Options {
	return Options{
		Now:              func() time.Time { return time.Time{} },
		NewID:            func() string { return "proposal-id" },
		OrdersWarehouse:  warehouse,
		CatalogWarehouse: warehouse,
		MenuWarehouse:    warehouse,
	}
}
