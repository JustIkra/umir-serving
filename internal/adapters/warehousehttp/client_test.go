package warehousehttp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/JustIkra/umir-serving/contract"
)

func TestClientEncodesOpaqueDishIDAndPreservesLargeStateVersion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/internal/v1/dish" {
			t.Errorf("request = %s %s, want GET /internal/v1/dish", r.Method, r.URL.Path)
		}
		if got, want := r.URL.Query().Get("dishID"), "Борщ /+&?"; got != want {
			t.Errorf("dishID query = %q, want %q", got, want)
		}
		if len(r.URL.Query()) != 1 {
			t.Errorf("query = %v, want only dishID", r.URL.Query())
		}
		w.Header().Set("Content-Type", "application/json")
		writeResponse(t, w, `{"evaluatedAt":"2026-10-11T12:00:00+03:00","stateVersion":"9007199254740993","dish":{"dishID":"Борщ /+&?","name":"Суп","ingredients":[]}}`)
	}))
	defer server.Close()

	client, err := NewClient(server.URL, server.Client(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.GetDish(context.Background(), "Борщ /+&?")
	if err != nil {
		t.Fatal(err)
	}
	if got.StateVersion != 9007199254740993 || got.Dish.DishID != "Борщ /+&?" || got.EvaluatedAt.Format(time.RFC3339) != "2026-10-11T12:00:00+03:00" {
		t.Fatalf("GetDish() = %+v", got)
	}
}

func TestClientRejectsDishDetailsForDifferentQueryID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writeResponse(t, w, `{"evaluatedAt":"2026-10-11T12:00:00+03:00","stateVersion":"1","dish":{"dishID":"other","name":"Dish","ingredients":[]}}`)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, server.Client(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetDish(context.Background(), "requested"); err == nil {
		t.Fatal("GetDish accepted details for a different dishID")
	}
}

func TestNewClientRejectsURLWithEmptyQueryMarker(t *testing.T) {
	if _, err := NewClient("http://warehouse:8082?", &http.Client{}, time.Second); err == nil {
		t.Fatal("NewClient() accepted URL with ForceQuery")
	}
}

func TestNewClientDoesNotInheritAmbientProxy(t *testing.T) {
	client, err := NewClient("http://warehouse:8082", nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := client.http.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatalf("internal HTTP transport = %#v, want an owned direct transport", client.http.Transport)
	}
}

func TestClientRejectsMalformedSuccessfulCommitAndDoesNotRetry(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		writeResponse(t, w, `{"kind":"OrderCommitted","order":{"orderID":"o1"},"evaluation":{}}`)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, server.Client(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.CommitAcceptedOrder(context.Background(), structCommitRequest())
	if err == nil {
		t.Fatal("CommitAcceptedOrder() accepted malformed successful response")
	}
	if calls != 1 {
		t.Fatalf("commit request count = %d, want one attempt", calls)
	}
}

func TestClientCommitTimeoutRemainsUnknownWithoutRetry(t *testing.T) {
	calls := 0
	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		close(entered)
		<-release
	}))
	defer server.Close()
	defer close(release)
	client, err := NewClient(server.URL, server.Client(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.CommitAcceptedOrder(context.Background(), structCommitRequest())
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout probe did not reach the owner")
	}
	var operation *contract.OperationError
	if !errors.As(err, &operation) || operation.Code != "UpstreamTimeout" || operation.Outcome != "unknown" {
		t.Fatalf("commit timeout error = %#v, want UpstreamTimeout/unknown", err)
	}
	if calls != 1 {
		t.Fatalf("commit requests = %d, want one", calls)
	}
}

func TestClientRequiresPendingKindToUseHTTP202(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writeResponse(t, w, `{"kind":"Pending"}`)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, server.Client(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.CommitAcceptedOrder(context.Background(), structCommitRequest())
	if err == nil || result.Kind != "" {
		t.Fatalf("CommitAcceptedOrder() = (%+v, %v), want status-kind protocol error", result, err)
	}
}

func TestClientTreatsUnknownRemoteErrorCodeAsUnknown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		writeResponse(t, w, `{"code":"FutureCode","message":"future","outcome":"not_committed"}`)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, server.Client(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.GetProducts(context.Background())
	var operationError *contract.OperationError
	if !errors.As(err, &operationError) || operationError.Outcome == "not_committed" {
		t.Fatalf("GetProducts() error = %#v, want unknown protocol error", err)
	}
}

func TestClientMapsInventoryUnavailableOutcomeByOperation(t *testing.T) {
	call := map[string]func(*Client) error{
		"read": func(client *Client) error {
			_, err := client.GetProducts(context.Background())
			return err
		},
		"register product": func(client *Client) error {
			_, err := client.RegisterProduct(context.Background(), contract.RegisterProductRequest{CommandID: "c1", ProductID: "p1", Name: "Product", BaseUnit: "g"})
			return err
		},
		"define dish": func(client *Client) error {
			_, err := client.DefineDish(context.Background(), contract.DefineDishRequest{CommandID: "c1", DishID: "d1", Name: "Dish", Recipe: []contract.Ingredient{{ProductID: "p1", Quantity: 1000}}})
			return err
		},
		"evaluate order": func(client *Client) error {
			_, err := client.EvaluateOrder(context.Background(), contract.EvaluateOrderRequest{Positions: []contract.Position{{LineNo: 1, DishID: "d1", Portions: 1}}})
			return err
		},
		"commit": func(client *Client) error {
			_, err := client.CommitAcceptedOrder(context.Background(), structCommitRequest())
			return err
		},
	}
	for _, test := range []struct {
		name        string
		call        string
		outcome     string
		body        string
		wantCause   bool
		wantOutcome string
	}{
		{name: "read unavailable", call: "read", outcome: "unavailable", wantCause: true, wantOutcome: "unavailable"},
		{name: "read rejects unknown", call: "read", outcome: "unknown", wantOutcome: "unavailable"},
		{name: "register product unknown", call: "register product", outcome: "unknown", wantCause: true, wantOutcome: "unknown"},
		{name: "define dish unknown", call: "define dish", outcome: "unknown", wantCause: true, wantOutcome: "unknown"},
		{name: "catalog write rejects unavailable", call: "register product", outcome: "unavailable", wantOutcome: "unknown"},
		{name: "catalog write rejects malformed", call: "define dish", body: `{`, wantOutcome: "unknown"},
		{name: "order evaluation unavailable", call: "evaluate order", outcome: "unavailable", wantCause: true, wantOutcome: "unavailable"},
		{name: "order evaluation rejects unknown", call: "evaluate order", outcome: "unknown", wantOutcome: "unknown"},
		{name: "order commit preserves definite refusal", call: "commit", outcome: "not_committed", wantCause: true, wantOutcome: "not_committed"},
		{name: "order commit preserves unknown", call: "commit", outcome: "unknown", wantCause: true, wantOutcome: "unknown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				body := test.body
				if body == "" {
					body = `{"code":"InventoryUnavailable","message":"warehouse unavailable","outcome":"` + test.outcome + `"}`
				}
				writeResponse(t, w, body)
			}))
			defer server.Close()
			client, err := NewClient(server.URL, server.Client(), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			err = call[test.call](client)
			var operation *contract.OperationError
			if !errors.As(err, &operation) {
				t.Fatalf("operation error = %#v, want OperationError", err)
			}
			if operation.Outcome != test.wantOutcome {
				t.Errorf("operation outcome = %q, want %q", operation.Outcome, test.wantOutcome)
			}
			if test.wantCause {
				if operation.Code != "InventoryUnavailable" || !errors.Is(err, contract.ErrInventoryUnavailable) {
					t.Errorf("operation error = %#v, want InventoryUnavailable cause", operation)
				}
			} else if operation.Code == "InventoryUnavailable" || errors.Is(err, contract.ErrInventoryUnavailable) {
				t.Errorf("operation error = %#v, want rejected remote response", operation)
			}
		})
	}
}

func TestClientRejectsMissingFoundDiscriminator(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writeResponse(t, w, `{}`)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, server.Client(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := client.FindRecordedCommand(context.Background(), "c1"); err == nil || found {
		t.Fatalf("FindRecordedCommand() = found %v, error %v; want missing discriminator error", found, err)
	}
}

func TestRecordedDishPayloadRejectsBaseUnitAndDoesNotInferIt(t *testing.T) {
	responseBody := `{"found":true,"record":{"commandID":"c1","commandType":"DefineDish","commandFingerprint":"fp","result":{"kind":"DishDefined","dish":{"dishID":"d1","name":"Dish","ingredients":[{"productID":"p1","quantity":"1","baseUnit":"g"}]}}}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writeResponse(t, w, responseBody)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, server.Client(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.FindRecordedCommand(context.Background(), "c1"); err == nil {
		t.Fatal("FindRecordedCommand accepted baseUnit in recorded DishDefined payload")
	}
	responseBody = `{"found":true,"record":{"commandID":"c1","commandType":"DefineDish","commandFingerprint":"fp","result":{"kind":"DishDefined","dish":{"dishID":"d1","name":"Dish","ingredients":[{"productID":"p1","quantity":"1"}]}}}}`
	record, found, err := client.FindRecordedCommand(context.Background(), "c1")
	if err != nil || !found || record.Result.Dish == nil || record.Result.Dish.Recipe[0].BaseUnit != "" {
		t.Fatalf("recorded dish = (%+v,%v,%v), want no inferred base unit", record, found, err)
	}
}

func TestClientRejectsCaseInsensitiveJSONFieldAliases(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writeResponse(t, w, `{"EvaluatedAt":"2026-10-11T12:00:00+03:00","StateVersion":"1","Dish":{"DishID":"d1","Name":"Dish","Ingredients":[]}}`)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, server.Client(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetDish(context.Background(), "d1"); err == nil {
		t.Fatal("GetDish() accepted case-insensitive JSON aliases")
	}
}

func TestParseMoscowTimestampRejectsCommaAndExcessFraction(t *testing.T) {
	for _, value := range []string{"2026-10-11T12:00:00,1+03:00", "2026-10-11T12:00:00.1234567890+03:00"} {
		if _, err := parseMoscowTime(value); err == nil {
			t.Errorf("parseMoscowTime(%q) succeeded", value)
		}
	}
	if _, err := parseMoscowTime("2026-10-11T12:00:00.1200+03:00"); err != nil {
		t.Fatalf("valid fraction with trailing zero rejected: %v", err)
	}
}

func TestClientImplementsAllElevenWarehouseOperations(t *testing.T) {
	cases := []struct {
		name string
		path string
		call func(*Client) error
	}{
		{name: "get products", path: "/internal/v1/products", call: func(c *Client) error { _, err := c.GetProducts(context.Background()); return err }},
		{name: "get dish catalog", path: "/internal/v1/dish-catalog", call: func(c *Client) error { _, err := c.GetDishCatalog(context.Background()); return err }},
		{name: "get dish", path: "/internal/v1/dish", call: func(c *Client) error { _, err := c.GetDish(context.Background(), "суп/+ &?"); return err }},
		{name: "register product", path: "/internal/v1/products", call: func(c *Client) error {
			_, err := c.RegisterProduct(context.Background(), contract.RegisterProductRequest{CommandID: "c1", ProductID: "p1", Name: "Product", BaseUnit: "g"})
			return err
		}},
		{name: "define dish", path: "/internal/v1/dishes", call: func(c *Client) error {
			_, err := c.DefineDish(context.Background(), contract.DefineDishRequest{CommandID: "c1", DishID: "d1", Name: "Dish", Recipe: []contract.Ingredient{}})
			return err
		}},
		{name: "evaluate menu", path: "/internal/v1/menu-evaluation", call: func(c *Client) error { _, err := c.EvaluateMenu(context.Background()); return err }},
		{name: "evaluate order", path: "/internal/v1/order-evaluations", call: func(c *Client) error {
			_, err := c.EvaluateOrder(context.Background(), contract.EvaluateOrderRequest{Positions: []contract.Position{}})
			return err
		}},
		{name: "commit order", path: "/internal/v1/order-commits", call: func(c *Client) error {
			_, err := c.CommitAcceptedOrder(context.Background(), structCommitRequest())
			return err
		}},
		{name: "find command", path: "/internal/v1/commands", call: func(c *Client) error { _, _, err := c.FindRecordedCommand(context.Background(), "c1"); return err }},
		{name: "find order", path: "/internal/v1/orders", call: func(c *Client) error { _, _, err := c.FindOrderByProposal(context.Background(), "p1"); return err }},
		{name: "read attempt", path: "/internal/v1/order-attempts", call: func(c *Client) error { _, err := c.FindOrderAttempt(context.Background(), "c1", "p1"); return err }},
	}
	responses := map[string]string{
		"GET /internal/v1/products":           `{"evaluatedAt":"2026-10-11T12:00:00+03:00","stateVersion":"1","items":[]}`,
		"GET /internal/v1/dish-catalog":       `{"evaluatedAt":"2026-10-11T12:00:00+03:00","stateVersion":"1","items":[]}`,
		"GET /internal/v1/dish":               `{"evaluatedAt":"2026-10-11T12:00:00+03:00","stateVersion":"1","dish":{"dishID":"суп/+ &?","name":"Soup","ingredients":[]}}`,
		"POST /internal/v1/products":          `{"productID":"p1","name":"Product","baseUnit":"g"}`,
		"POST /internal/v1/dishes":            `{"dishID":"d1","name":"Dish","ingredients":[]}`,
		"GET /internal/v1/menu-evaluation":    `{"evaluatedAt":"2026-10-11T12:00:00+03:00","stateVersion":"1","dishes":[]}`,
		"POST /internal/v1/order-evaluations": `{"evaluatedAt":"2026-10-11T12:00:00+03:00","stateVersion":"1","requestedPositions":[],"acceptedPositions":[],"changes":[]}`,
		"POST /internal/v1/order-commits":     `{"kind":"OrderCommitted","order":{"orderID":"o1","proposalID":"p1","acceptedPositions":[{"lineNo":"1","dishID":"d1","portions":"1"}],"allocations":[],"placedAt":"2026-10-11T12:00:00+03:00"}}`,
		"GET /internal/v1/commands":           `{"found":false}`,
		"GET /internal/v1/orders":             `{"found":false}`,
		"GET /internal/v1/order-attempts":     `{"state":"Unseen"}`,
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if test.path == "/internal/v1/dish" && r.URL.Query().Get("dishID") != "суп/+ &?" {
					t.Errorf("dishID query = %q", r.URL.Query().Get("dishID"))
				}
				key := r.Method + " " + r.URL.Path
				if key != "GET "+test.path && key != "POST "+test.path {
					t.Errorf("request path = %s", key)
				}
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				writeResponse(t, w, responses[key])
			}))
			defer server.Close()
			client, err := NewClient(server.URL, server.Client(), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if err := test.call(client); err != nil {
				t.Fatalf("operation error: %v", err)
			}
			if calls != 1 {
				t.Fatalf("request count = %d, want one", calls)
			}
		})
	}
}

func writeResponse(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()
	if _, err := io.WriteString(w, body); err != nil {
		t.Errorf("write fixture response: %v", err)
	}
}

func structCommitRequest() contract.CommitOrderRequest {
	return contract.CommitOrderRequest{CommandID: "c1", ProposalID: "p1", AcceptedPositions: []contract.AcceptedPosition{{LineNo: 1, DishID: "d1", Portions: 1}}}
}
