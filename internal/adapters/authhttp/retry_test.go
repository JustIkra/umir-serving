package authhttp

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/JustIkra/umir-serving/contract"
)

type retryTransport func(*http.Request) (*http.Response, error)

func (f retryTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRetryClockRecoversWithFixedBusinessTime(t *testing.T) {
	for _, method := range []string{"readiness", "signing"} {
		t.Run(method, func(t *testing.T) {
			owner := time.Date(2026, 10, 13, 9, 0, 0, 0, time.FixedZone("MSK", 10800))
			initial := owner
			host := owner
			calls := 0
			healthy := false
			transport := retryTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Path != "/internal/auth/v1/jwks" || r.Header.Get("X-Umir-Service-Key") != "discovery" {
					t.Error("incorrect discovery request")
				}
				status, body := 503, `{}`
				if healthy {
					status = 200
					body = `{"keys":[{"kty":"OKP","crv":"Ed25519","alg":"EdDSA","use":"sig","kid":"first","x":"` + base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.PublicKeySize)) + `"}]}`
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			c, err := NewWithRetryClock("http://auth:8080", "discovery", &http.Client{Transport: transport}, time.Second, func() time.Time { return owner }, func() time.Time { return host })
			if err != nil {
				t.Fatal(err)
			}
			check := func(ctx context.Context) error {
				if method == "readiness" {
					return c.CheckSigningKeys(ctx)
				}
				_, err := c.SigningKey(ctx, "first")
				return err
			}
			ctx := context.Background()
			if check(ctx) == nil || calls != 1 {
				t.Fatal("failed initial fetch accepted")
			}
			healthy = true
			for range 64 {
				if check(ctx) == nil {
					t.Fatal("retry not throttled")
				}
			}
			host = host.Add(999 * time.Millisecond)
			if check(ctx) == nil || calls != 1 {
				t.Fatal("subsecond retry not bounded")
			}
			host = host.Add(time.Millisecond)
			cancelled, cancel := context.WithCancel(ctx)
			cancel()
			if err := check(cancelled); !errors.Is(err, context.Canceled) || calls != 1 {
				t.Fatal("cancellation lost or attempted fetch")
			}
			host = initial.Add(-time.Hour)
			for range 64 {
				if check(ctx) == nil {
					t.Fatal("host rollback bypassed throttle")
				}
			}
			if calls != 1 {
				t.Fatal("rollback amplified requests")
			}
			host = host.Add(999 * time.Millisecond)
			if check(ctx) == nil || calls != 1 {
				t.Fatal("rollback retry too early")
			}
			host = host.Add(time.Millisecond)
			if err := check(ctx); err != nil || calls != 2 || owner != initial {
				t.Fatalf("fixed-time recovery failed: calls=%d error=%v", calls, err)
			}
			host = host.Add(24 * time.Hour)
			if _, err := c.SigningKey(ctx, "first"); err != nil || calls != 2 {
				t.Fatal("host time expired business key cache")
			}
			if _, err := c.SigningKey(ctx, "missing"); err == nil || calls != 3 {
				t.Fatal("unknown kid did not refresh")
			}
			for range 64 {
				if _, err := c.SigningKey(ctx, "missing"); err == nil {
					t.Fatal("unknown kid accepted")
				}
			}
			if calls != 3 {
				t.Fatal("unknown kid repeated refresh")
			}
			owner = owner.Add(30 * time.Second)
			if check(ctx) == nil || calls != 3 {
				t.Fatal("business advance bypassed host retry bound")
			}
			host = host.Add(time.Second)
			if err := check(ctx); err != nil || calls != 4 {
				t.Fatal("stale business cache did not refresh")
			}
			owner = initial.Add(-time.Second)
			host = host.Add(time.Second)
			if err := check(ctx); err != nil || calls != 5 {
				t.Fatal("business rollback retained invalid cache")
			}
		})
	}
}

func TestRetryClockRejectsMissingAndZeroMechanismTime(t *testing.T) {
	owner := time.Date(2026, 10, 13, 9, 0, 0, 0, time.FixedZone("MSK", 10800))
	if _, err := NewWithRetryClock("http://auth:8080", "discovery", &http.Client{}, time.Second, func() time.Time { return owner }, nil); !errors.Is(err, contract.ErrInvalidArgument) {
		t.Fatal("missing retry clock accepted")
	}
	c, err := NewWithRetryClock("http://auth:8080", "discovery", &http.Client{Transport: retryTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("zero retry clock issued request")
		return nil, errors.New("unexpected request")
	})}, time.Second, func() time.Time { return owner }, func() time.Time { return time.Time{} })
	if err != nil {
		t.Fatal(err)
	}
	if c.CheckSigningKeys(context.Background()) == nil {
		t.Fatal("zero retry time accepted")
	}
	if _, err := c.SigningKey(context.Background(), "first"); err == nil {
		t.Fatal("zero retry time accepted")
	}
}
