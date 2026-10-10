package main

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
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

func handler() (*bootstrap.HTTPHandler, error) {
	requestTimeout, err := durationSetting("REQUEST_TIMEOUT", "12s")
	if err != nil {
		return nil, err
	}
	warehouseTimeout, err := durationSetting("WAREHOUSE_TIMEOUT", "10s")
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
	_, publicHandler, err := bootstrap.NewHTTP(bootstrap.HTTPOptions{
		WarehouseInternalURL: env("WAREHOUSE_INTERNAL_URL", "http://warehouse:8082"),
		WarehouseTimeout:     warehouseTimeout,
		RequestTimeout:       requestTimeout,
		MaxRequestBytes:      maxRequestBytes,
		Now:                  func() time.Time { return time.Now().In(location) },
		NewID:                newID,
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

func main() {
	if len(os.Args) == 2 && os.Args[1] == "healthcheck" {
		if err := healthcheck(); err != nil {
			log.Fatal(err)
		}
		return
	}
	port, err := serverPort()
	if err != nil {
		log.Fatal(err)
	}
	app, err := handler()
	if err != nil {
		log.Fatal(err)
	}
	server := &http.Server{
		Addr:              net.JoinHostPort("", strconv.Itoa(port)),
		Handler:           app,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	signals, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Printf("umir-serving listening on %s", server.Addr)
	if err := serveHTTP(server, app, signals, stop, server.ListenAndServe); err != nil {
		log.Fatal(err)
	}
}
