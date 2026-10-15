package authhttp

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type stepClock struct{ nanos atomic.Int64 }

func newStepClock(at time.Time) *stepClock {
	c := &stepClock{}
	c.nanos.Store(at.UnixNano())
	return c
}
func (c *stepClock) Now() time.Time          { return time.Unix(0, c.nanos.Load()).UTC() }
func (c *stepClock) Advance(d time.Duration) { c.nanos.Add(int64(d)) }

// slowAuth serves the first JWKS request at once and holds every later one
// until release is closed or the client gives up.
type slowAuth struct {
	server    *httptest.Server
	calls     atomic.Int32
	entered   chan struct{}
	release   chan struct{}
	cancelled chan struct{}
	once      sync.Once
	oncec     sync.Once
}

func newSlowAuth(t *testing.T, key ed25519.PublicKey) *slowAuth {
	t.Helper()
	a := &slowAuth{entered: make(chan struct{}), release: make(chan struct{}), cancelled: make(chan struct{})}
	body := `{"keys":[{"kty":"OKP","crv":"Ed25519","alg":"EdDSA","use":"sig","kid":"first","x":"` + base64.RawURLEncoding.EncodeToString(key) + `"}]}`
	a.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.calls.Add(1) > 1 {
			a.once.Do(func() { close(a.entered) })
			select {
			case <-a.release:
			case <-r.Context().Done():
				a.oncec.Do(func() { close(a.cancelled) })
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(body)); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(func() {
		select {
		case <-a.release:
		default:
			close(a.release)
		}
		a.server.Close()
	})
	return a
}

func (a *slowAuth) client(t *testing.T, clock *stepClock) *Client {
	t.Helper()
	// The attempt bound is the 5 second maximum: the held response outlives
	// every assertion below, so only release or Close ends it.
	c, err := NewWithRetryClock(a.server.URL, "synthetic-auth-client", a.server.Client(), 5*time.Second, clock.Now, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func waitIdle(t *testing.T, c *Client) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		c.mu.RLock()
		idle := c.inflight == nil
		c.mu.RUnlock()
		if idle {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("refresh did not finish")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestProactiveRefreshDoesNotBlockVerification(t *testing.T) {
	public, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	auth := newSlowAuth(t, public)
	start := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	clock := newStepClock(start)
	c := auth.client(t, clock)
	ctx := context.Background()
	if _, err := c.SigningKey(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	clock.Advance(20 * time.Second)
	if _, err := c.SigningKey(ctx, "first"); err != nil {
		t.Fatalf("key aged 20 seconds refused: %v", err)
	}
	select {
	case <-auth.entered:
	case <-time.After(time.Second):
		t.Fatal("proactive refresh did not start at age 20 seconds")
	}

	const verifiers = 50
	began := time.Now()
	var wg sync.WaitGroup
	failures := make(chan error, verifiers)
	for range verifiers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := c.SigningKey(ctx, "first")
			if err == nil && !got.Equal(public) {
				err = context.Canceled
			}
			failures <- err
		}()
	}
	wg.Wait()
	elapsed := time.Since(began)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatalf("verification failed during refresh: %v", err)
		}
	}
	if elapsed > 100*time.Millisecond {
		t.Fatalf("verification waited for the refresh: %v", elapsed)
	}
	if got := auth.calls.Load(); got != 2 {
		t.Fatalf("refresh was not single-flight: %d requests", got)
	}

	close(auth.release)
	waitIdle(t, c)
	clock.Advance(15 * time.Second)
	if _, err := c.SigningKey(ctx, "first"); err != nil || auth.calls.Load() != 2 {
		t.Fatalf("completed refresh did not renew the cache: calls=%d err=%v", auth.calls.Load(), err)
	}
}

func TestStaleKeyFailsClosedWhileRefreshIsHeld(t *testing.T) {
	public, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	auth := newSlowAuth(t, public)
	clock := newStepClock(time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC))
	c := auth.client(t, clock)
	if _, err := c.SigningKey(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	clock.Advance(25 * time.Second)
	if _, err := c.SigningKey(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-auth.entered:
	case <-time.After(time.Second):
		t.Fatal("proactive refresh did not start")
	}
	clock.Advance(5 * time.Second)
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			if _, err := c.SigningKey(ctx, "first"); err == nil {
				t.Error("key older than 30 seconds accepted")
			}
		}()
	}
	wg.Wait()
	if got := auth.calls.Load(); got != 2 {
		t.Fatalf("stale callers started extra requests: %d", got)
	}
}

func TestCloseCancelsAndWaitsForRefresh(t *testing.T) {
	public, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	auth := newSlowAuth(t, public)
	clock := newStepClock(time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC))
	c := auth.client(t, clock)
	if _, err := c.SigningKey(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	clock.Advance(20 * time.Second)
	if _, err := c.SigningKey(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-auth.entered:
	case <-time.After(time.Second):
		t.Fatal("proactive refresh did not start")
	}
	closed := make(chan struct{})
	go func() { c.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not stop the refresh")
	}
	select {
	case <-auth.cancelled:
	case <-time.After(time.Second):
		t.Fatal("refresh request was not cancelled")
	}
	c.mu.RLock()
	running := c.inflight != nil
	c.mu.RUnlock()
	if running {
		t.Fatal("refresh outlived Close")
	}
	clock.Advance(15 * time.Second)
	if _, err := c.SigningKey(context.Background(), "first"); err == nil {
		t.Fatal("expired key accepted after Close")
	}
	c.Close()
}
