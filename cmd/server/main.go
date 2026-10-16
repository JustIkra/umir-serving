package main

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/JustIkra/umir-serving/bootstrap"
)

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// secretFileMaxBytes bounds a service key file.
const secretFileMaxBytes = 64 << 10

// secretEnv returns the secret NAME from the environment, or from the file
// named by NAME_FILE. Setting both is an error. Only the trailing line break of
// a file is removed. Errors name the variable and never carry the value.
func secretEnv(getenv func(string) string, name string) (string, error) {
	value, path := getenv(name), getenv(name+"_FILE")
	if path == "" {
		return value, nil
	}
	if value != "" {
		return "", fmt.Errorf("%s and %s_FILE are both set", name, name)
	}
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("%s_FILE: cannot read: %w", name, err)
	}
	data, readErr := io.ReadAll(io.LimitReader(file, secretFileMaxBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return "", fmt.Errorf("%s_FILE: cannot read: %w", name, errors.Join(readErr, closeErr))
	}
	if len(data) > secretFileMaxBytes {
		return "", fmt.Errorf("%s_FILE: file is too large", name)
	}
	secret := strings.TrimRight(string(data), "\r\n")
	if secret == "" {
		return "", fmt.Errorf("%s_FILE: file is empty", name)
	}
	return secret, nil
}

// handler assembles the process handler with logging discarded.
func handler() (*bootstrap.HTTPHandler, error) {
	return handlerWithLogger(slog.New(slog.DiscardHandler))
}

func handlerWithLogger(logger *slog.Logger) (*bootstrap.HTTPHandler, error) {
	requestTimeout, err := durationSetting("REQUEST_TIMEOUT", "12s")
	if err != nil {
		return nil, err
	}
	warehouseTimeout, err := durationSetting("WAREHOUSE_TIMEOUT", "5s")
	if err != nil {
		return nil, err
	}
	maxRequestBytes, err := positiveInt64Setting("MAX_REQUEST_BYTES", strconv.FormatInt(1<<20, 10))
	if err != nil {
		return nil, err
	}
	location, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		return nil, err
	}
	authTimeout, err := durationSetting("AUTH_TIMEOUT", "5s")
	if err != nil {
		return nil, err
	}
	pollInterval, err := durationSetting("REGISTRY_POLL_INTERVAL", "30s")
	if err != nil {
		return nil, err
	}
	var keys [4]string
	for i, name := range []string{"SERVING_AUTH_KEY", "AUTH_RUNTIME_KEY", "GATEWAY_SERVING_KEY", "SERVING_WAREHOUSE_KEY"} {
		if keys[i], err = secretEnv(os.Getenv, name); err != nil {
			return nil, err
		}
	}
	publicHandler, err := bootstrap.NewProcessHTTP(bootstrap.ProcessHTTPOptions{
		HTTPOptions: bootstrap.HTTPOptions{
			WarehouseInternalURL: env("WAREHOUSE_INTERNAL_URL", "http://warehouse:8082"),
			WarehouseTimeout:     warehouseTimeout,
			RequestTimeout:       requestTimeout,
			MaxRequestBytes:      maxRequestBytes,
			Now:                  func() time.Time { return time.Now().In(location) },
			NewID:                newID,
			Logger:               logger,
		},
		AuthInternalURL:      os.Getenv("AUTH_INTERNAL_URL"),
		AuthTimeout:          authTimeout,
		ServingAuthKey:       keys[0],
		AuthRuntimeKey:       keys[1],
		GatewayServingKey:    keys[2],
		ServingWarehouseKey:  keys[3],
		TZDataVersion:        os.Getenv("TZDATA_VERSION"),
		Epoch:                newUUID(),
		RegistryPollInterval: pollInterval,
	})
	if err != nil {
		return nil, err
	}
	return publicHandler, nil
}

func serverPort() (int, error) {
	value := env("PORT", "8080")
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("invalid PORT")
	}
	return port, nil
}

func durationSetting(name, fallback string) (time.Duration, error) {
	value := env(name, fallback)
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("invalid %s", name)
	}
	return duration, nil
}

func positiveInt64Setting(name, fallback string) (int64, error) {
	value := env(name, fallback)
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("invalid %s", name)
	}
	return parsed, nil
}

func newID() string {
	var value [16]byte
	if _, err := cryptorand.Read(value[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(value[:])
}

func newUUID() string {
	var value [16]byte
	if _, err := cryptorand.Read(value[:]); err != nil {
		return ""
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", value[:4], value[4:6], value[6:8], value[8:10], value[10:])
}

var instanceIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// instanceID returns SERVING_INSTANCE_ID when it is set, and a random UUID of
// this process otherwise.
func instanceID() (string, error) {
	value, set := os.LookupEnv("SERVING_INSTANCE_ID")
	if !set || value == "" {
		return newUUID(), nil
	}
	if !instanceIDPattern.MatchString(value) {
		return newUUID(), fmt.Errorf("invalid SERVING_INSTANCE_ID")
	}
	return value, nil
}

// buildVersion is the VCS revision the binary was built from, or unknown.
func buildVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" && setting.Value != "" {
				return setting.Value
			}
		}
	}
	return "unknown"
}

// startupErrorText is the error text for process lifecycle records. A
// *url.Error repeats the whole URL, which can carry credentials, so only its
// operation and cause are kept.
func startupErrorText(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Op + ": " + urlErr.Err.Error()
	}
	return err.Error()
}

// serverErrorWriter turns the net/http error log into one record with a kind.
// The messages carry the peer address, panic values and stacks, so none of the
// text is kept.
type serverErrorWriter struct{ logger *slog.Logger }

func (w serverErrorWriter) Write(p []byte) (int, error) {
	kind := "other"
	switch text := string(p); {
	case strings.HasPrefix(text, "http: panic serving"):
		kind = "panic"
	case strings.HasPrefix(text, "http: TLS handshake error"):
		kind = "tls_handshake"
	case strings.HasPrefix(text, "http: Accept error"):
		kind = "accept"
	}
	w.logger.Error("http.server_error", slog.String("event", "http.server_error"), slog.String("kind", kind))
	return len(p), nil
}

func newServerErrorLog(logger *slog.Logger) *log.Logger {
	return log.New(serverErrorWriter{logger: logger}, "", 0)
}

func fatal(logger *slog.Logger, event string, err error) {
	logger.Error(event, slog.String("event", event), slog.String("error", startupErrorText(err)))
	os.Exit(1)
}

func healthcheck() (result error) {
	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Get("http://127.0.0.1:" + env("PORT", "8080") + "/healthz")
	if err != nil {
		return err
	}
	defer func() {
		result = errors.Join(result, response.Body.Close())
	}()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("health status: %d", response.StatusCode)
	}
	return nil
}

type idleConnectionCloser interface{ CloseIdleConnections() }

func serveHTTP(server *http.Server, app idleConnectionCloser, signals context.Context, stop context.CancelFunc, listenAndServe func() error) error {
	shutdownDone := make(chan error, 1)
	go func() {
		<-signals.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		shutdownErr := server.Shutdown(ctx)
		if shutdownErr != nil {
			shutdownErr = errors.Join(shutdownErr, server.Close())
		}
		app.CloseIdleConnections()
		shutdownDone <- shutdownErr
	}()
	listenErr := listenAndServe()
	if listenErr == nil {
		listenErr = errors.New("HTTP listener stopped unexpectedly")
	}
	if !errors.Is(listenErr, http.ErrServerClosed) {
		stop()
		return errors.Join(listenErr, <-shutdownDone)
	}
	return <-shutdownDone
}

func newProcessLogger(w io.Writer) (*slog.Logger, error) {
	id, err := instanceID()
	return bootstrap.NewLogger(w, id, buildVersion(), slog.LevelInfo), err
}

func main() {
	logger, idErr := newProcessLogger(os.Stdout)
	if idErr != nil {
		fatal(logger, "process.start_failed", idErr)
	}
	if len(os.Args) == 2 && os.Args[1] == "healthcheck" {
		if err := healthcheck(); err != nil {
			fatal(logger, "process.healthcheck_failed", err)
		}
		return
	}
	port, err := serverPort()
	if err != nil {
		fatal(logger, "process.start_failed", err)
	}
	app, err := handlerWithLogger(logger)
	if err != nil {
		fatal(logger, "process.start_failed", err)
	}
	server := &http.Server{
		ErrorLog:          newServerErrorLog(logger),
		Addr:              net.JoinHostPort("", strconv.Itoa(port)),
		Handler:           app,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	signals, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger.Info("process.started", slog.String("event", "process.started"), slog.Int("port", port))
	if err := serveHTTP(server, app, signals, stop, server.ListenAndServe); err != nil {
		fatal(logger, "process.failed", err)
	}
	logger.Info("process.stopped", slog.String("event", "process.stopped"))
}
