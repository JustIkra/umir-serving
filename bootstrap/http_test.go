package bootstrap_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/JustIkra/umir-serving/bootstrap"
)

func TestNewHTTPComposesClientRuntimeAndPublicHandlerWithoutWarehouseReadiness(t *testing.T) {
	warehouseCalls := 0
	warehouse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		warehouseCalls++
		if r.Method != http.MethodGet || r.URL.Path != "/internal/v1/products" {
			t.Errorf("warehouse request = %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		writeTestBody(t, w, `{"evaluatedAt":"2026-10-11T12:00:00+03:00","stateVersion":"9007199254740993","items":[]}`)
	}))
	defer warehouse.Close()
	runtime, handler, err := bootstrap.NewHTTP(bootstrap.HTTPOptions{WarehouseInternalURL: warehouse.URL, WarehouseTimeout: time.Second, RequestTimeout: time.Second, MaxRequestBytes: 1024, Now: func() time.Time { return time.Date(2026, 10, 11, 12, 0, 0, 0, time.FixedZone("MSK", 3*60*60)) }, NewID: func() string { return "id-1" }, HTTPClient: warehouse.Client()})
	if err != nil || runtime == nil || handler == nil {
		t.Fatalf("NewHTTP() = runtime %v handler %v error %v", runtime, handler, err)
	}
	public := httptest.NewServer(handler)
	defer public.Close()
	response, err := http.Get(public.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || warehouseCalls != 0 {
		t.Fatalf("health status=%d warehouse calls=%d", response.StatusCode, warehouseCalls)
	}
	response, err = http.Get(public.URL + "/products")
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestBody(t, response.Body)
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), `"stateVersion":"9007199254740993"`) || !strings.Contains(string(body), `"items":[]`) || warehouseCalls != 1 {
		t.Fatalf("GET /products = status %d body %s warehouse calls %d", response.StatusCode, string(body), warehouseCalls)
	}
}

func TestNewHTTPRejectsInvalidWarehouseURLAndBudgets(t *testing.T) {
	base := bootstrap.HTTPOptions{WarehouseInternalURL: "http://user:secret@warehouse:8082", WarehouseTimeout: time.Second, RequestTimeout: time.Second, MaxRequestBytes: 1024, Now: time.Now, NewID: func() string { return "id-1" }}
	if runtime, handler, err := bootstrap.NewHTTP(base); err == nil || runtime != nil || handler != nil {
		t.Fatalf("NewHTTP() with credentials = (%v,%v,%v)", runtime, handler, err)
	}
	base.WarehouseInternalURL = "http://warehouse:8082?"
	if runtime, handler, err := bootstrap.NewHTTP(base); err == nil || runtime != nil || handler != nil {
		t.Fatalf("NewHTTP() with empty query marker = (%v,%v,%v)", runtime, handler, err)
	}
	base.WarehouseInternalURL = "http://warehouse:8082"
	base.WarehouseTimeout = 0
	if runtime, handler, err := bootstrap.NewHTTP(base); err == nil || runtime != nil || handler != nil {
		t.Fatalf("NewHTTP() with zero budget = (%v,%v,%v)", runtime, handler, err)
	}
}

func TestServingHTTPRoutesCatalogMenuOrdersAndPassiveResolve(t *testing.T) {
	commitCalls := 0
	commitCallsByCommand := make(map[string]int)
	commitPendingC2 := false
	warehouse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /internal/v1/products":
			writeTestBody(t, w, `{"evaluatedAt":"2026-10-11T12:00:00+03:00","stateVersion":"1","items":[]}`)
		case "POST /internal/v1/products":
			writeTestBody(t, w, `{"productID":"p1","name":"Product","baseUnit":"g"}`)
		case "GET /internal/v1/dish-catalog":
			writeTestBody(t, w, `{"evaluatedAt":"2026-10-11T12:00:00+03:00","stateVersion":"1","items":[]}`)
		case "GET /internal/v1/dish":
			writeTestBody(t, w, `{"evaluatedAt":"2026-10-11T12:00:00+03:00","stateVersion":"1","dish":{"dishID":"d1","name":"Dish","ingredients":[]}}`)
		case "POST /internal/v1/dishes":
			writeTestBody(t, w, `{"dishID":"d1","name":"Dish","ingredients":[{"productID":"p1","quantity":"1","baseUnit":"g"}]}`)
		case "GET /internal/v1/menu-evaluation":
			writeTestBody(t, w, `{"evaluatedAt":"2026-10-11T12:00:00+03:00","stateVersion":"1","dishes":[{"dishID":"d1","dishName":"Dish","isAvailable":true,"deficits":[],"allocations":[{"productID":"p1","productName":"Product","batchID":"b1","quantity":"1","baseUnit":"g","expiresAt":"2026-10-12T12:00:00+03:00"}]}]}`)
		case "POST /internal/v1/order-evaluations":
			writeTestBody(t, w, `{"evaluatedAt":"2026-10-11T12:00:00+03:00","stateVersion":"1","requestedPositions":[{"lineNo":"1","dishID":"d1","portions":"1"}],"acceptedPositions":[{"lineNo":"1","dishID":"d1","portions":"1"}],"changes":[]}`)
		case "GET /internal/v1/commands", "GET /internal/v1/orders":
			writeTestBody(t, w, `{"found":false}`)
		case "POST /internal/v1/order-commits":
			commitCalls++
			var body struct {
				CommandID  string `json:"commandID"`
				ProposalID string `json:"proposalID"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			commitCallsByCommand[body.CommandID]++
			if body.CommandID == "c2" {
				commitPendingC2 = true
				w.WriteHeader(http.StatusAccepted)
				writeTestBody(t, w, `{"kind":"Pending"}`)
				return
			}
			writeTestBody(t, w, `{"kind":"OrderCommitted","order":{"orderID":"o1","proposalID":"p1","acceptedPositions":[{"lineNo":"1","dishID":"d1","portions":"1"}],"allocations":[{"productID":"p1","batchID":"b1","quantity":"1"}],"placedAt":"2026-10-11T12:00:00+03:00"}}`)
		case "GET /internal/v1/order-attempts":
			if r.URL.Query().Get("commandID") == "c2" && commitPendingC2 {
				writeTestBody(t, w, `{"state":"Completed","result":{"kind":"OrderCommitted","order":{"orderID":"o2","proposalID":"p2","acceptedPositions":[{"lineNo":"1","dishID":"d1","portions":"1"}],"allocations":[{"productID":"p1","batchID":"b1","quantity":"1"}],"placedAt":"2026-10-11T12:00:00+03:00"}}}`)
			} else {
				writeTestBody(t, w, `{"state":"Unseen"}`)
			}
		default:
			t.Errorf("unexpected warehouse request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer warehouse.Close()
	ids := 0
	_, handler, err := bootstrap.NewHTTP(bootstrap.HTTPOptions{WarehouseInternalURL: warehouse.URL, WarehouseTimeout: time.Second, RequestTimeout: time.Second, MaxRequestBytes: 4096, Now: func() time.Time { return time.Date(2026, 10, 11, 12, 0, 0, 0, time.FixedZone("MSK", 3*60*60)) }, NewID: func() string { ids++; return "p" + strconv.Itoa(ids) }, HTTPClient: warehouse.Client()})
	if err != nil {
		t.Fatal(err)
	}
	defer handler.CloseIdleConnections()
	public := httptest.NewServer(handler)
	defer public.Close()
	requestWithContentType := func(method, path, body, contentType string) (int, string) {
		req, err := http.NewRequestWithContext(context.Background(), method, public.URL+path, bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		response, err := public.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer closeTestBody(t, response.Body)
		data, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, string(data)
	}
	request := func(method, path, body string) (int, string) {
		contentType := ""
		if body != "" {
			contentType = "application/json"
		}
		return requestWithContentType(method, path, body, contentType)
	}
	cases := []struct {
		method, path, body string
		status             int
		kind               string
	}{
		{method: http.MethodGet, path: "/products", status: 200},
		{method: http.MethodPost, path: "/products", body: `{"commandID":"register-1","productID":"p1","name":"Product","baseUnit":"g"}`, status: 200},
		{method: http.MethodGet, path: "/dish-catalog", status: 200},
		{method: http.MethodGet, path: "/dish?dishID=d1", status: 200},
		{method: http.MethodPost, path: "/dishes", body: `{"commandID":"define-1","dishID":"d1","name":"Dish","ingredients":[{"productID":"p1","quantity":"1","baseUnit":"g"}]}`, status: 200},
		{method: http.MethodGet, path: "/menu", status: 200},
		{method: http.MethodGet, path: "/stop-list", status: 200},
		{method: http.MethodGet, path: "/recommendations", status: 200},
		{method: http.MethodPost, path: "/order-proposals", body: `{"positions":[{"dishID":"d1","portions":"1"}]}`, status: 200},
		{method: http.MethodPost, path: "/order-confirmations", body: `{"commandID":"c1","proposalID":"p1"}`, status: 200, kind: "OrderConfirmed"},
		{method: http.MethodPost, path: "/order-proposals", body: `{"positions":[{"dishID":"d1","portions":"1"}]}`, status: 200},
		{method: http.MethodPost, path: "/order-confirmations", body: `{"commandID":"c2","proposalID":"p2"}`, status: 202, kind: "ConfirmationPending"},
		{method: http.MethodPost, path: "/order-confirmations/resolve", body: `{"commandID":"c2","proposalID":"p2"}`, status: 200, kind: "OrderConfirmed"},
	}
	for _, test := range cases {
		status, response := request(test.method, test.path, test.body)
		if status != test.status {
			t.Errorf("%s %s status=%d body=%s want %d", test.method, test.path, status, response, test.status)
		}
		if test.kind != "" && !strings.Contains(response, `"kind":"`+test.kind+`"`) {
			t.Errorf("%s %s body=%s, want kind %s", test.method, test.path, response, test.kind)
		}
	}
	for _, test := range []struct {
		path        string
		body        string
		contentType string
	}{
		{path: "/order-confirmations/resolve", body: `{}`},
		{path: "/order-confirmations/resolve?extra=value", body: `{"commandID":"c1","proposalID":"p1"}`},
		{path: "/order-confirmations/resolve", body: `{"commandID":"c1","proposalID":"p1"}`, contentType: "text/plain"},
		{path: "/order-confirmations/resolve", body: `{"commandID":`},
	} {
		contentType := test.contentType
		if contentType == "" {
			contentType = "application/json"
		}
		status, response := requestWithContentType(http.MethodPost, test.path, test.body, contentType)
		if status != http.StatusBadRequest || !strings.Contains(response, `"outcome":"unknown"`) {
			t.Errorf("invalid resolve %s status=%d body=%s, want unknown outcome", test.path, status, response)
		}
	}
	status, response := request(http.MethodPost, "/order-confirmations/resolve", `{"commandID":"c1","proposalID":"p1"}`)
	if status != http.StatusOK || !strings.Contains(response, `"kind":"OrderConfirmed"`) {
		t.Errorf("valid resolve c1 status=%d body=%s, want OrderConfirmed", status, response)
	}
	if commitCalls != 2 {
		t.Fatalf("warehouse commit requests = %d, want Confirm c1 and c2 only", commitCalls)
	}
	if commitCallsByCommand["c1"] != 1 {
		t.Fatalf("warehouse commit requests for confirmed c1 = %d, want one sale", commitCallsByCommand["c1"])
	}
}

func TestNewHTTPPreservesUnknownInventoryOutcomeForCatalogWrites(t *testing.T) {
	warehouse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || (r.URL.Path != "/internal/v1/products" && r.URL.Path != "/internal/v1/dishes") {
			t.Errorf("warehouse request = %s %s, want catalog POST", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		writeTestBody(t, w, `{"code":"InventoryUnavailable","message":"warehouse outcome is unknown","outcome":"unknown"}`)
	}))
	defer warehouse.Close()
	_, handler, err := bootstrap.NewHTTP(bootstrap.HTTPOptions{
		WarehouseInternalURL: warehouse.URL,
		WarehouseTimeout:     time.Second,
		RequestTimeout:       time.Second,
		MaxRequestBytes:      4096,
		Now:                  func() time.Time { return time.Date(2026, 10, 11, 12, 0, 0, 0, time.FixedZone("MSK", 3*60*60)) },
		NewID:                func() string { return "proposal-1" },
		HTTPClient:           warehouse.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer handler.CloseIdleConnections()
	public := httptest.NewServer(handler)
	defer public.Close()
	for _, test := range []struct {
		path string
		body string
	}{
		{path: "/products", body: `{"commandID":"register-1","productID":"p1","name":"Product","baseUnit":"g"}`},
		{path: "/dishes", body: `{"commandID":"define-1","dishID":"d1","name":"Dish","ingredients":[{"productID":"p1","quantity":"1000"}]}`},
	} {
		request, err := http.NewRequest(http.MethodPost, public.URL+test.path, strings.NewReader(test.body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := public.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(response.Body)
		closeErr := response.Body.Close()
		if readErr != nil || closeErr != nil {
			t.Fatalf("read/close response: %v, %v", readErr, closeErr)
		}
		if response.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), `"code":"InventoryUnavailable"`) || !strings.Contains(string(body), `"outcome":"unknown"`) {
			t.Errorf("POST %s response=%d %s, want 503 InventoryUnavailable/unknown", test.path, response.StatusCode, body)
		}
	}
}

func TestNewHTTPPreservesUnavailableInventoryOutcomeForOrderPreparation(t *testing.T) {
	warehouse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/internal/v1/order-evaluations" {
			t.Errorf("warehouse request = %s %s, want POST /internal/v1/order-evaluations", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		writeTestBody(t, w, `{"code":"InventoryUnavailable","message":"warehouse unavailable","outcome":"unavailable"}`)
	}))
	defer warehouse.Close()
	_, handler, err := bootstrap.NewHTTP(bootstrap.HTTPOptions{
		WarehouseInternalURL: warehouse.URL,
		WarehouseTimeout:     time.Second,
		RequestTimeout:       time.Second,
		MaxRequestBytes:      4096,
		Now:                  func() time.Time { return time.Date(2026, 10, 11, 12, 0, 0, 0, time.FixedZone("MSK", 3*60*60)) },
		NewID:                func() string { return "proposal-1" },
		HTTPClient:           warehouse.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer handler.CloseIdleConnections()
	public := httptest.NewServer(handler)
	defer public.Close()
	request, err := http.NewRequest(http.MethodPost, public.URL+"/order-proposals", strings.NewReader(`{"positions":[{"dishID":"d1","portions":"1"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := public.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read/close response: %v, %v", readErr, closeErr)
	}
	if response.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), `"code":"InventoryUnavailable"`) || !strings.Contains(string(body), `"outcome":"unavailable"`) {
		t.Fatalf("POST /order-proposals response=%d %s, want 503 InventoryUnavailable/unavailable", response.StatusCode, body)
	}
}

func writeTestBody(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()
	if _, err := io.WriteString(w, body); err != nil {
		t.Errorf("write fixture body: %v", err)
	}
}

func closeTestBody(t *testing.T, body io.Closer) {
	t.Helper()
	if err := body.Close(); err != nil {
		t.Errorf("close HTTP body: %v", err)
	}
}
