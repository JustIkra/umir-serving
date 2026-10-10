package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata"
)

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func handler() (http.Handler, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]string{"status": "ok", "service": "umir-serving"}); err != nil {
			log.Printf("health response: %v", err)
		}
	})
	return mux, nil
}

func healthcheck() (err error) {
	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Get("http://127.0.0.1:" + env("PORT", "8080") + "/healthz")
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, response.Body.Close())
	}()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("health status: %d", response.StatusCode)
	}
	return nil
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "healthcheck" {
		if err := healthcheck(); err != nil {
			log.Fatal(err)
		}
		return
	}
	app, err := handler()
	if err != nil {
		log.Fatal(err)
	}
	server := &http.Server{
		Addr:              ":" + env("PORT", "8080"),
		Handler:           app,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	signals, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-signals.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}()
	log.Printf("umir-serving listening on %s", server.Addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
