package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JustIkra/umir-serving/contract"
)

type runtimeStub struct {
	Runtime
	registerCalls int
	dishCalls     int
	defineCalls   int
	definedDish   contract.DefineDishRequest
	confirmResult contract.ConfirmResult
	resolveErr    error
}

func (r *runtimeStub) RegisterProduct(_ context.Context, _ contract.RegisterProductRequest) (contract.Product, error) {
	r.registerCalls++
	return contract.Product{ProductID: "p1", Name: "Product", BaseUnit: "g"}, nil
}

func (r *runtimeStub) GetDish(_ context.Context, dishID string) (contract.DishDetails, error) {
	r.dishCalls++
	return contract.DishDetails{Dish: contract.Dish{DishID: dishID, Name: "Dish", Recipe: []contract.Ingredient{}}}, nil
}

func (r *runtimeStub) DefineDish(_ context.Context, request contract.DefineDishRequest) (contract.Dish, error) {
	r.defineCalls++
	r.definedDish = request
	return contract.Dish{DishID: request.DishID, Name: request.Name, Recipe: append([]contract.Ingredient(nil), request.Recipe...)}, nil
}

func (r *runtimeStub) ConfirmOrder(context.Context, contract.ConfirmOrderRequest) (contract.ConfirmResult, error) {
	return r.confirmResult, nil
}

func (r *runtimeStub) ResolveConfirmation(context.Context, contract.ConfirmOrderRequest) (contract.ConfirmResult, error) {
	return contract.ConfirmResult{}, r.resolveErr
}

func TestHandlerRejectsCaseInsensitiveRequestFieldAliasBeforeRuntime(t *testing.T) {
	runtime := &runtimeStub{}
	handler, err := NewHandler(runtime, Options{RequestTimeout: time.Second, MaxRequestBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/products", strings.NewReader(`{"CommandID":"c1","productID":"p1","name":"Product","baseUnit":"g"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || runtime.registerCalls != 0 {
		t.Fatalf("response = %d %s; runtime calls = %d, want 400 and no call", response.Code, response.Body.String(), runtime.registerCalls)
	}
}

func TestHandlerRejectsAmbiguousDishQuery(t *testing.T) {
	runtime := &runtimeStub{}
	handler, err := NewHandler(runtime, Options{RequestTimeout: time.Second, MaxRequestBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/dish?dishID=one&dishID=two", nil))
	if response.Code != http.StatusBadRequest || runtime.dishCalls != 0 {
		t.Fatalf("response = %d; runtime calls = %d", response.Code, runtime.dishCalls)
	}
}

func TestHandlerInvalidArgumentOutcomeDependsOnOperation(t *testing.T) {
	runtime := &runtimeStub{}
	handler, err := NewHandler(runtime, Options{RequestTimeout: time.Second, MaxRequestBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		method  string
		target  string
		body    string
		outcome string
	}{
		{name: "read rejection", method: http.MethodGet, target: "/products?extra=value", outcome: "unavailable"},
		{name: "mutating rejection", method: http.MethodPost, target: "/products?extra=value", body: `{"commandID":"c1","productID":"p1","name":"Product","baseUnit":"g"}`, outcome: "not_committed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.target, strings.NewReader(test.body))
			if test.body != "" {
				request.Header.Set("Content-Type", "application/json")
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"outcome":"`+test.outcome+`"`) {
				t.Fatalf("response=%d %s, want outcome %s", response.Code, response.Body.String(), test.outcome)
			}
		})
	}
	if runtime.registerCalls != 0 {
		t.Fatalf("rejected requests called RegisterProduct %d times", runtime.registerCalls)
	}
}

func TestHandlerResolveInvalidArgumentKeepsAttemptUnknownUnlessOwnerEstablishedRefusal(t *testing.T) {
	for _, test := range []struct {
		name    string
		err     error
		outcome string
	}{
		{name: "passive lookup invalid argument", err: contract.ErrInvalidArgument, outcome: "unknown"},
		{name: "cached owner refusal", err: &contract.OperationError{Code: "InvalidArgument", Message: "owner refused confirmation", Outcome: "not_committed", Cause: contract.ErrInvalidArgument}, outcome: "not_committed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime := &runtimeStub{resolveErr: test.err}
			handler, err := NewHandler(runtime, Options{RequestTimeout: time.Second, MaxRequestBytes: 1024})
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/order-confirmations/resolve", strings.NewReader(`{"commandID":"c1","proposalID":"p1"}`))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"outcome":"`+test.outcome+`"`) {
				t.Fatalf("response=%d %s, want outcome %s", response.Code, response.Body.String(), test.outcome)
			}
		})
	}
}

func TestHandlerRejectsRawSemicolonDishQueryButAcceptsEncodedID(t *testing.T) {
	runtime := &runtimeStub{}
	handler, err := NewHandler(runtime, Options{RequestTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	bad := httptest.NewRecorder()
	handler.ServeHTTP(bad, httptest.NewRequest(http.MethodGet, "/dish?dishID=one;extra=value", nil))
	if bad.Code != http.StatusBadRequest || runtime.dishCalls != 0 {
		t.Fatalf("raw semicolon query response=%d calls=%d", bad.Code, runtime.dishCalls)
	}
	for _, query := range []string{"dishID=one%3Bextra%3Dvalue", "dishID=one%26other%3Dvalue"} {
		good := httptest.NewRecorder()
		handler.ServeHTTP(good, httptest.NewRequest(http.MethodGet, "/dish?"+query, nil))
		if good.Code != http.StatusOK {
			t.Fatalf("encoded ID query %q returned status %d", query, good.Code)
		}
	}
	if runtime.dishCalls != 2 {
		t.Fatalf("encoded ID lookups = %d, want two", runtime.dishCalls)
	}
	for _, target := range []string{"/dish?", "/dish?dishID=one&other=value", "/dish?dishID=one&dishID=two", "/dish?dishID=%"} {
		bad := httptest.NewRecorder()
		handler.ServeHTTP(bad, httptest.NewRequest(http.MethodGet, target, nil))
		if bad.Code != http.StatusBadRequest {
			t.Errorf("invalid query %q returned status %d", target, bad.Code)
		}
	}
}

func TestHandlerDistinguishesOmittedIngredientUnitFromInvalidSuppliedUnit(t *testing.T) {
	runtime := &runtimeStub{}
	handler, err := NewHandler(runtime, Options{RequestTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	register := httptest.NewRequest(http.MethodPost, "/products", strings.NewReader(`{"commandID":"c1","productID":"p1","name":"Product","baseUnit":"g"}`))
	register.Header.Set("Content-Type", "application/json")
	registerResponse := httptest.NewRecorder()
	handler.ServeHTTP(registerResponse, register)
	if registerResponse.Code != http.StatusOK || runtime.registerCalls != 1 {
		t.Fatalf("valid product registration status=%d calls=%d", registerResponse.Code, runtime.registerCalls)
	}
	for _, unit := range []string{"", "kg"} {
		body := `{"commandID":"d1","dishID":"dish1","name":"Dish","ingredients":[{"productID":"p1","quantity":"1000","baseUnit":"` + unit + `"}]}`
		request := httptest.NewRequest(http.MethodPost, "/dishes", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest || runtime.defineCalls != 0 {
			t.Fatalf("supplied baseUnit %q: status=%d define calls=%d", unit, response.Code, runtime.defineCalls)
		}
	}
	omitted := httptest.NewRequest(http.MethodPost, "/dishes", strings.NewReader(`{"commandID":"d2","dishID":"dish2","name":"Dish","ingredients":[{"productID":"p1","quantity":"1000"}]}`))
	omitted.Header.Set("Content-Type", "application/json")
	omittedResponse := httptest.NewRecorder()
	handler.ServeHTTP(omittedResponse, omitted)
	if omittedResponse.Code != http.StatusOK || runtime.defineCalls != 1 || runtime.definedDish.Recipe[0].BaseUnit != "" {
		t.Fatalf("omitted baseUnit: status=%d calls=%d request=%+v", omittedResponse.Code, runtime.defineCalls, runtime.definedDish)
	}
	explicit := httptest.NewRequest(http.MethodPost, "/dishes", strings.NewReader(`{"commandID":"d3","dishID":"dish3","name":"Dish","ingredients":[{"productID":"p1","quantity":"1000","baseUnit":"g"}]}`))
	explicit.Header.Set("Content-Type", "application/json")
	explicitResponse := httptest.NewRecorder()
	handler.ServeHTTP(explicitResponse, explicit)
	if explicitResponse.Code != http.StatusOK || runtime.defineCalls != 2 || runtime.definedDish.Recipe[0].BaseUnit != "g" {
		t.Fatalf("valid explicit baseUnit: status=%d calls=%d request=%+v", explicitResponse.Code, runtime.defineCalls, runtime.definedDish)
	}
}

func TestHandlerRejectsNullAndInvalidUTF8BeforeApplication(t *testing.T) {
	for name, body := range map[string][]byte{"null field": []byte(`{"commandID":"c1","productID":"p1","name":"Product","baseUnit":null}`), "invalid UTF-8": {'{', '"', 'c', 'o', 'm', 'm', 'a', 'n', 'd', 'I', 'D', '"', ':', '"', 0xff, '"', '}'}} {
		t.Run(name, func(t *testing.T) {
			runtime := &runtimeStub{}
			handler, err := NewHandler(runtime, Options{RequestTimeout: time.Second, MaxRequestBytes: 1024})
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/products", bytes.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest || runtime.registerCalls != 0 {
				t.Fatalf("response=%d calls=%d", response.Code, runtime.registerCalls)
			}
		})
	}
}

func TestHandlerEnforcesRequestSizeBeforeApplication(t *testing.T) {
	runtime := &runtimeStub{}
	handler, err := NewHandler(runtime, Options{RequestTimeout: time.Second, MaxRequestBytes: 10})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/products", strings.NewReader(`{"commandID":"c1"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || runtime.registerCalls != 0 {
		t.Fatalf("response=%d calls=%d", response.Code, runtime.registerCalls)
	}
}

func TestHandlerEncodesEmptyOrderWithoutApplicationProposalPayload(t *testing.T) {
	proposal := contract.OrderProposal{ProposalID: "p1", RequestedPositions: []contract.Position{}, AcceptedPositions: []contract.AcceptedPosition{}, Changes: []contract.Change{}}
	runtime := &runtimeStub{confirmResult: contract.ConfirmResult{Kind: contract.EmptyOrder, Proposal: &proposal}}
	handler, err := NewHandler(runtime, Options{RequestTimeout: time.Second, MaxRequestBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/order-confirmations", strings.NewReader(`{"commandID":"c1","proposalID":"p1"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != `{"kind":"EmptyOrder"}` {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
}

func TestHandlerBoundsIncompleteUploadAndFlushesError(t *testing.T) {
	runtime := &runtimeStub{}
	handler, err := NewHandler(runtime, Options{RequestTimeout: 100 * time.Millisecond, MaxRequestBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	connection, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := connection.Close(); err != nil {
			t.Errorf("close connection: %v", err)
		}
	})
	if err := connection.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	_, err = io.WriteString(connection, "POST /products HTTP/1.1\r\nHost: serving\r\nContent-Type: application/json\r\nContent-Length: 100\r\nConnection: keep-alive\r\n\r\n{")
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(connection), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := response.Body.Close(); err != nil {
			t.Errorf("close response body: %v", err)
		}
	})
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("incomplete request status = %d, want 400", response.StatusCode)
	}
}

func TestHandlerClearsBodyReadDeadlineForKeepAliveReuse(t *testing.T) {
	runtime := &runtimeStub{}
	handler, err := NewHandler(runtime, Options{RequestTimeout: 40 * time.Millisecond, MaxRequestBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	var connectionCount atomic.Int32
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connectionCount.Add(1)
		}
	}
	server.Start()
	defer server.Close()
	transport := &http.Transport{MaxIdleConns: 1, MaxIdleConnsPerHost: 1}
	client := &http.Client{Transport: transport}
	defer transport.CloseIdleConnections()
	for _, request := range []struct{ method, path, body string }{{http.MethodGet, "/healthz", ""}, {http.MethodPost, "/products", `{"commandID":"c1","productID":"p1","name":"Product","baseUnit":"g"}`}} {
		req, err := http.NewRequest(request.method, server.URL+request.path, strings.NewReader(request.body))
		if err != nil {
			t.Fatal(err)
		}
		if request.body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, response.Body); err != nil {
			t.Fatal(err)
		}
		if err := response.Body.Close(); err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s status = %d", request.path, response.StatusCode)
		}
	}
	time.Sleep(70 * time.Millisecond)
	request, err := http.NewRequest(http.MethodGet, server.URL+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := response.Body.Close(); err != nil {
			t.Errorf("close health response: %v", err)
		}
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("keep-alive health status = %d", response.StatusCode)
	}
	if connectionCount.Load() != 1 {
		t.Fatalf("HTTP connections = %d, want one reused connection", connectionCount.Load())
	}
}
