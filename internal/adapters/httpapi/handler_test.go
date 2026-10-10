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
	confirmResult contract.ConfirmResult
}

func (r *runtimeStub) RegisterProduct(_ context.Context, _ contract.RegisterProductRequest) (contract.Product, error) {
	r.registerCalls++
	return contract.Product{ProductID: "p1", Name: "Product", BaseUnit: "g"}, nil
}

func (r *runtimeStub) GetDish(_ context.Context, dishID string) (contract.DishDetails, error) {
	r.dishCalls++
	return contract.DishDetails{Dish: contract.Dish{DishID: dishID, Name: "Dish", Recipe: []contract.Ingredient{}}}, nil
}

func (r *runtimeStub) ConfirmOrder(context.Context, contract.ConfirmOrderRequest) (contract.ConfirmResult, error) {
	return r.confirmResult, nil
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
