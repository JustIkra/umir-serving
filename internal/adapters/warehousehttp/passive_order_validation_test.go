package warehousehttp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/JustIkra/umir-serving/contract"
)

func TestS3RepairPassiveOrderValidation(t *testing.T) {
	for _, endpoint := range []string{"commands", "orders", "order-attempts"} {
		for _, defect := range []string{"none", "empty-allocations", "empty-positions", "future-instant"} {
			t.Run(endpoint+"/"+defect, func(t *testing.T) {
				order := map[string]any{"orderID": "order", "proposalID": "proposal", "placedAt": s2At.Format(time.RFC3339Nano), "acceptedPositions": []any{map[string]any{"lineNo": "1", "dishID": "dish", "portions": "9007199254740993"}}, "allocations": []any{map[string]any{"productID": "p", "batchID": "b", "quantity": "9007199254740993"}}}
				switch defect {
				case "empty-allocations":
					order["allocations"] = []any{}
				case "empty-positions":
					order["acceptedPositions"] = []any{}
				case "future-instant":
					order["placedAt"] = s2At.Add(time.Second).Format(time.RFC3339Nano)
				}
				var result any
				switch endpoint {
				case "commands":
					result = map[string]any{"found": true, "record": map[string]any{"commandID": "command", "commandType": "ConfirmOrder", "commandFingerprint": strings.Repeat("a", 64), "result": map[string]any{"kind": "OrderPlaced", "order": order}}}
				case "orders":
					result = map[string]any{"found": true, "order": order}
				case "order-attempts":
					result = map[string]any{"state": "Completed", "result": map[string]any{"kind": "OrderCommitted", "order": order}}
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodGet || r.URL.Path != "/internal/warehouse/v2/r/"+s2Tenant+"/"+endpoint {
						t.Errorf("unexpected request: %s %s", r.Method, r.URL)
					}
					s2Envelope(t, w, result, nil)
				}))
				defer server.Close()
				scope := s2Scope("resolveOrder", "consumer.order.resolve")
				client := s2Client(t, server, scope)
				observation := &s2Observation{}
				ctx := contract.WithWarehouseObserver(contract.WithRequestScope(context.Background(), scope), observation)
				var got *contract.Order
				var err error
				switch endpoint {
				case "commands":
					var record contract.CommandRecord
					record, _, err = client.FindRecordedCommand(ctx, "command")
					got = record.Result.Order
				case "orders":
					var value contract.Order
					value, _, err = client.FindOrderByID(ctx, "order")
					got = &value
				case "order-attempts":
					var attempt contract.OrderAttempt
					attempt, err = client.FindOrderAttempt(ctx, "command", "proposal")
					if attempt.Result != nil {
						got = attempt.Result.Order
					}
				}
				if defect != "none" {
					var operation *contract.OperationError
					if !errors.As(err, &operation) || operation.Code != "ProtocolError" || operation.Outcome != "unavailable" || len(observation.values) != 0 {
						t.Fatalf("malformed passive order accepted: %+v err=%v observations=%+v", got, err, observation.values)
					}
					return
				}
				if err != nil || got == nil || len(got.Allocations) != 1 || len(got.Positions) != 1 || got.Allocations[0].Quantity != 9007199254740993 || got.Positions[0].Portions != 9007199254740993 || !got.PlacedAt.Equal(s2At) || len(observation.values) != 1 {
					t.Fatalf("valid passive order changed: %+v err=%v observations=%+v", got, err, observation.values)
				}
			})
		}
	}
}
