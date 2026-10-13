package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestRecoveryFIFOAdmission(t *testing.T) {
	dir := physicalTempDir(t)
	p := filepath.Join(dir, "config-fifo")
	if err := syscall.Mkfifo(p, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := readPrivate(p, 16384); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("nonregular accepted")
		}
		t.Log("nonregular rejected without waiting")
	case <-time.After(200 * time.Millisecond):
		writer, err := os.OpenFile(p, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			t.Logf("read rejected only after opening FIFO writer: %t", err != nil)
		case <-time.After(time.Second):
			t.Fatal("reader did not join")
		}
		t.Error("nonregular private file blocked before its regular-file validation")
	}
}

func TestRecoveryFrozenClockJWKSRecovery(t *testing.T) {
	var healthy atomic.Bool
	var registryCalls, jwksCalls atomic.Int32
	public := ed25519.PublicKey(make([]byte, ed25519.PublicKeySize))
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		code, body := 200, ""
		switch r.URL.Path {
		case "/internal/auth/v1/registry":
			registryCalls.Add(1)
			body = `{"registryRevision":"1","complete":true,"entries":[],"tombstones":[]}`
		case "/internal/auth/v1/jwks":
			jwksCalls.Add(1)
			if !healthy.Load() {
				code = 503
				body = `{}`
			} else {
				body = fmt.Sprintf(`{"keys":[{"kty":"OKP","crv":"Ed25519","alg":"EdDSA","use":"sig","kid":"independent-recovery","x":%q}]}`, base64.RawURLEncoding.EncodeToString(public))
			}
		default:
			t.Errorf("unexpected route")
			code = 404
		}
		return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	c := controlForTest(t, func(context.Context) bool { return false })
	app, err := compose(ownerConfig{ServiceURLs: serviceURLs{Auth: "http://auth:8080", Warehouse: "http://warehouse:8080"}}, serviceKeys{Discovery: "discovery-key", Lifecycle: "lifecycle-key", Gateway: "gateway-key", Warehouse: "warehouse-key"}, c.now, testFixture, client)
	if err != nil {
		t.Fatal(err)
	}
	defer app.CloseIdleConnections()
	limit := time.Now().Add(time.Second)
	for jwksCalls.Load() == 0 && time.Now().Before(limit) {
		time.Sleep(10 * time.Millisecond)
	}
	if jwksCalls.Load() != 1 {
		t.Fatal("initial JWKS attempt not observed")
	}
	healthy.Store(true)
	time.Sleep(2200 * time.Millisecond)
	ready := ownerReady(context.Background(), app, "lifecycle-key", testFixture)
	t.Logf("after healthy transport at fixed owner time: registryCalls=%d jwksCalls=%d ready=%t", registryCalls.Load(), jwksCalls.Load(), ready)
	if registryCalls.Load() < 3 {
		t.Fatal("insufficient registry retries")
	}
	if !ready || jwksCalls.Load() < 2 {
		t.Error("healthy JWKS transport never retried while owner clock stays fixed")
	}
	if c.readClock().Instant != "2026-10-13T09:00:00+03:00" || c.readClock().Generation != "1" {
		t.Fatal("recovery changed owner clock")
	}
	if jwksCalls.Load() > 3 {
		t.Fatal("unbounded retries")
	}
}
