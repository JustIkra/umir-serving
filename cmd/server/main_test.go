package main

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
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
