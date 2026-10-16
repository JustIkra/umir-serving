package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"os"
	"syscall"
)

// EventResponseFailed is written when the handler cannot finish a response.
const EventResponseFailed = "http.response_failed"

func (h *Handler) logger() *slog.Logger {
	if h.options.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return h.options.Logger
}

// responseFailed records which stage of the response failed and a bounded kind
// of the cause. The error text is never written: network errors carry the
// addresses of both ends.
func (h *Handler) responseFailed(level slog.Level, stage string, err error) {
	h.logger().LogAttrs(context.Background(), level, EventResponseFailed,
		slog.String("event", EventResponseFailed), slog.String("stage", stage), slog.String("error_kind", errorKind(err)))
}

func errorKind(err error) string {
	var syntax *json.SyntaxError
	var unsupported *json.UnsupportedTypeError
	var unsupportedValue *json.UnsupportedValueError
	var marshaler *json.MarshalerError
	var network net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		return "deadline"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, syscall.EPIPE), errors.Is(err, syscall.ECONNRESET), errors.Is(err, net.ErrClosed):
		return "connection_closed"
	case errors.As(err, &network) && network.Timeout():
		return "timeout"
	case errors.As(err, &syntax), errors.As(err, &unsupported), errors.As(err, &unsupportedValue), errors.As(err, &marshaler):
		return "encoding"
	}
	return "other"
}
