package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type closeIdleFunc func()

func (f closeIdleFunc) CloseIdleConnections() { f() }

func TestServerReturnsUnexpectedListenerErrorWithoutSignal(t *testing.T) {
	listenErr := errors.New("listener address is already in use")
	signals, stop := context.WithCancel(context.Background())
	defer stop()
	closed := false
	finished := make(chan error, 1)
	go func() {
		finished <- serveHTTP(&http.Server{}, closeIdleFunc(func() { closed = true }), signals, stop, func() error { return listenErr })
	}()
	select {
	case err := <-finished:
		if !errors.Is(err, listenErr) {
			t.Fatalf("serveHTTP() error = %v, want listener failure", err)
		}
	case <-time.After(time.Second):
		t.Fatal("serveHTTP() waited for a signal after the listener failed")
	}
	if !closed {
		t.Fatal("serveHTTP() did not close its idle HTTP client connections")
	}
}

func TestServerWaitsForActiveResponseDuringShutdown(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	finishHandler := func() { releaseOnce.Do(func() { close(release) }) }
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusOK)
		if _, err := io.WriteString(w, "finished"); err != nil {
			t.Errorf("write active response: %v", err)
		}
	})}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("close test listener: %v", err)
		}
	})
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Errorf("close test server: %v", err)
		}
	})
	t.Cleanup(finishHandler)
	signals, stop := context.WithCancel(context.Background())
	defer stop()
	finished := make(chan error, 1)
	go func() {
		finished <- serveHTTP(server, closeIdleFunc(func() {}), signals, stop, func() error { return server.Serve(listener) })
	}()
	requestDone := make(chan error, 1)
	go func() {
		response, requestErr := http.Get("http://" + listener.Addr().String())
		if requestErr != nil {
			requestDone <- requestErr
			return
		}
		if response.StatusCode != http.StatusOK {
			requestDone <- errors.Join(errors.New("active response status was not 200"), response.Body.Close())
			return
		}
		body, readErr := io.ReadAll(response.Body)
		if readErr == nil && string(body) != "finished" {
			readErr = errors.New("active response body was incomplete")
		}
		requestDone <- errors.Join(readErr, response.Body.Close())
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}
	stop()
	select {
	case err := <-finished:
		t.Fatalf("server returned before active response drained: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	finishHandler()
	if err := <-requestDone; err != nil {
		t.Fatal(err)
	}
	if err := <-finished; err != nil {
		t.Fatalf("serveHTTP() error = %v", err)
	}
}

type closeErrorBody struct {
	err error
}

func (b closeErrorBody) Read([]byte) (int, error) {
	return 0, io.EOF
}

func (b closeErrorBody) Close() error {
	return b.err
}

func TestHealthcheckReturnsResponseBodyCloseError(t *testing.T) {
	t.Setenv("PORT", "12345")
	closeErr := errors.New("response body close failed")
	previousTransport := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       closeErrorBody{err: closeErr},
			Header:     make(http.Header),
		}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = previousTransport })

	err := healthcheck()
	if !errors.Is(err, closeErr) {
		t.Fatalf("healthcheck() error = %v, want response body close failure", err)
	}
}

func TestHealthcheckPreservesStatusAndBodyCloseErrors(t *testing.T) {
	t.Setenv("PORT", "12345")
	closeErr := errors.New("response body close failed")
	previousTransport := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Body:       closeErrorBody{err: closeErr},
			Header:     make(http.Header),
		}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = previousTransport })

	err := healthcheck()
	if !errors.Is(err, closeErr) || err == nil || !strings.Contains(err.Error(), "health status: 503") {
		t.Fatalf("healthcheck() error = %v, want status and close errors", err)
	}
}
