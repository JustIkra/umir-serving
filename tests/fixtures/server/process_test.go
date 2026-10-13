package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestCompositionReadinessPrivacyClockAndCancellation(t *testing.T) {
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var healthy atomic.Bool
	healthy.Store(true)
	var jwks atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("X-Umir-Service-Key") != "discovery-key" {
			return nil, errors.New("wrong discovery authentication")
		}
		var body string
		if !healthy.Load() {
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		switch r.URL.Path {
		case "/internal/auth/v1/registry":
			body = `{"registryRevision":"1","complete":true,"entries":[],"tombstones":[]}`
		case "/internal/auth/v1/jwks":
			jwks.Add(1)
			body = `{"keys":[{"kty":"OKP","crv":"Ed25519","alg":"EdDSA","use":"sig","kid":"unit-composition","x":"` + base64.RawURLEncoding.EncodeToString(publicKey) + `"}]}`
		default:
			return nil, errors.New("unexpected peer route")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	clock := controlForTest(t, func(context.Context) bool { return false })
	app, err := compose(ownerConfig{ServiceURLs: serviceURLs{Auth: "http://auth:8080", Warehouse: "http://warehouse:8080"}}, serviceKeys{Discovery: "discovery-key", Lifecycle: "lifecycle-key", Gateway: "gateway-key", Warehouse: "warehouse-key"}, clock.now, testFixture, client)
	if err != nil {
		t.Fatal(err)
	}
	defer app.CloseIdleConnections()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	for !ownerReady(context.Background(), app, "lifecycle-key", testFixture) {
		select {
		case <-deadline.C:
			t.Fatal("real registry composition stayed not ready")
		case <-poll.C:
		}
	}
	w := send(t, app, input(30, "clock.read", "{}"), "test-control-credential", context.Background())
	if w.Code == 200 || strings.Contains(w.Body.String(), "owner-injected") {
		t.Fatal("control exposed on business handler")
	}
	req := httptest.NewRequest(http.MethodGet, "/internal/serving/v2/runtime", nil)
	req.Header.Set("X-Umir-Service-Key", "lifecycle-key")
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, req)
	var status struct {
		Epoch string `json:"epoch"`
		Ready bool   `json:"ready"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Epoch != testFixture || !status.Ready {
		t.Fatal("runtime epoch or readiness fabricated")
	}
	before := jwks.Load()
	advance := send(t, clock, input(31, "clock.set", `{"expectedInstanceID":"33333333-3333-4333-8333-333333333333","expectedGeneration":"1","instant":"2026-10-14T09:00:00+03:00"}`), "test-control-credential", context.Background())
	if advance.Code != 200 {
		t.Fatal("clock advance failed")
	}
	deadline2 := time.NewTimer(2 * time.Second)
	defer deadline2.Stop()
	for jwks.Load() <= before {
		select {
		case <-deadline2.C:
			t.Fatal("real JWKS cache did not observe controlled time")
		case <-poll.C:
		}
	}
	healthy.Store(false)
	start := time.Now()
	app.CloseIdleConnections()
	if time.Since(start) > time.Second {
		t.Fatal("process cancellation did not stop discovery")
	}
}
func TestPrivateCredentialRejectsSymlinkPermissionsAndBound(t *testing.T) {
	dir := physicalTempDir(t)
	path := filepath.Join(dir, "key")
	if err := os.WriteFile(path, []byte("private-sentinel"), 0600); err != nil {
		t.Fatal(err)
	}
	b, err := readPrivate(path, 64)
	if err != nil || string(b) != "private-sentinel" {
		t.Fatal("valid private file rejected")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivate(path, 64); err == nil {
		t.Fatal("public credential accepted")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivate(path, 4); err == nil {
		t.Fatal("unbounded credential accepted")
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivate(link, 64); err == nil {
		t.Fatal("symlink credential accepted")
	}
}
func TestSocketDACAndOwnedShutdown(t *testing.T) {
	dir, err := os.MkdirTemp("", "si-socket-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "control.sock")
	listener, err := privateListener(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatal("socket not private")
	}
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	c := controlForTest(t, func(context.Context) bool { return true })
	go func() { done <- serveListeners(ctx, http.NotFoundHandler(), c, tcp, listener) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("listeners did not cancel")
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("owned socket leaked")
	}
}

func TestPrivateFileRejectsSymlinkParent(t *testing.T) {
	dir := physicalTempDir(t)
	real := filepath.Join(dir, "real")
	if err := os.Mkdir(real, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "key"), []byte("private-credential"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "linked")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivate(filepath.Join(link, "key"), 4096); err == nil {
		t.Fatal("symlink parent accepted")
	}
}

func physicalTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}
