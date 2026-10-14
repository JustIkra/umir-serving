package warehousehttp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/JustIkra/umir-serving/contract"
)

func TestS2MenuSharedMetadataConsistency(t *testing.T) {
	scope := s2Scope("recommendations", "consumer.menu.evaluate")
	for _, test := range []struct {
		name    string
		unit    string
		batch   string
		expires time.Time
		valid   bool
	}{
		{name: "conflicting expiry", unit: "g", batch: "b", expires: s2At.Add(2 * time.Hour)},
		{name: "conflicting batch unit", unit: "ml", batch: "b", expires: s2At.Add(time.Hour)},
		{name: "conflicting product unit across batches", unit: "ml", batch: "other", expires: s2At.Add(time.Hour)},
		{name: "different quantities", unit: "g", batch: "b", expires: s2At.Add(time.Hour), valid: true},
		{name: "same instant different offset", unit: "g", batch: "b", expires: s2At.Add(time.Hour).In(time.FixedZone("MSK", 3*3600)), valid: true},
		{name: "different batches different expiry", unit: "g", batch: "other", expires: s2At.Add(2 * time.Hour), valid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				available := true
				a := wireMenuAllocation{ProductID: "p", ProductName: "P", BatchID: "b", Quantity: "1", BaseUnit: "g", ExpiresAt: s2At.Add(time.Hour).Format(time.RFC3339Nano)}
				b := a
				b.BaseUnit, b.BatchID = test.unit, test.batch
				b.ExpiresAt, b.Quantity = test.expires.Format(time.RFC3339Nano), "9007199254740993"
				s2Envelope(t, w, wireMenuEvaluation{EvaluatedAt: s2At.Format(time.RFC3339Nano), StateVersion: "9007199254740993", Dishes: []wireDishEvaluation{
					{DishID: "a", DishName: "A", IsAvailable: &available, Deficits: []wireDeficit{}, Allocations: []wireMenuAllocation{a}},
					{DishID: "b", DishName: "B", IsAvailable: &available, Deficits: []wireDeficit{}, Allocations: []wireMenuAllocation{b}},
				}}, nil)
			}))
			defer server.Close()
			client := s2Client(t, server, scope)
			observer := &s2Observation{}
			ctx := contract.WithWarehouseObserver(contract.WithRequestScope(context.Background(), scope), observer)
			got, err := client.EvaluateMenu(ctx)
			if !test.valid {
				var operation *contract.OperationError
				if !errors.As(err, &operation) || operation.Code != "ProtocolError" || operation.Outcome != "unavailable" || len(observer.values) != 0 {
					t.Fatalf("contradictory menu must fail before observation: result=%+v err=%v observations=%+v", got, err, observer.values)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			allocation := got.Dishes[1].Allocations[0]
			if len(observer.values) != 1 || allocation.Quantity != 9007199254740993 || allocation.BaseUnit != "g" || !allocation.ExpiresAt.Equal(test.expires) {
				t.Fatalf("valid shared metadata lost: result=%+v observations=%+v", got, observer.values)
			}
		})
	}
}
