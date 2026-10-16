package bootstrap

import (
	"io"
	"log/slog"
	"time"
)

// LogSchemaVersion is the log_schema_version of every record the process writes.
const LogSchemaVersion = 1

// LogServiceName is the service field of every record.
const LogServiceName = "umir-serving"

// Events written by the registry and the process loop.
const (
	EventReconcileFailed        = "reconcile.failed"
	EventRegistryTenantMismatch = "registry.tenant_mismatch"
)

var logZone = time.FixedZone("Europe/Moscow", 3*60*60)

// NewLogger returns the process logger: JSON lines with the common fields of
// the factory observability format (time with the +03:00 offset, level, msg,
// service, version, instance_id, log_schema_version). Call sites add event.
func NewLogger(w io.Writer, instanceID, version string, level slog.Leveler) *slog.Logger {
	handler := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(groups []string, attr slog.Attr) slog.Attr {
			if len(groups) == 0 && attr.Key == slog.TimeKey {
				attr.Value = slog.TimeValue(attr.Value.Time().In(logZone))
			}
			return attr
		},
	})
	return slog.New(handler).With(
		slog.Int("log_schema_version", LogSchemaVersion),
		slog.String("service", LogServiceName),
		slog.String("version", version),
		slog.String("instance_id", instanceID),
	)
}

// nopLogger discards everything; it is the default of every Logger option.
func nopLogger(logger *slog.Logger) *slog.Logger {
	if logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return logger
}
