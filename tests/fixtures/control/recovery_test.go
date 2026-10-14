package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRecoveryHelperFIFOAdmission(t *testing.T) {
	p := filepath.Join(physicalTempDir(t), "credential-fifo")
	if err := syscall.Mkfifo(p, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := readPrivate(p, 4096); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("nonregular accepted")
		}
	case <-time.After(200 * time.Millisecond):
		f, err := os.OpenFile(p, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			t.Logf("read rejected only after opening FIFO writer: %t", err != nil)
		case <-time.After(time.Second):
			t.Fatal("reader did not join")
		}
		t.Error("helper blocks on nonregular file before validation")
	}
}
func TestRecoveryHelperInstantBoundary(t *testing.T) {
	for _, operation := range []string{"clock.read", "clock.set", "clock.checkpoint"} {
		for _, tc := range []struct {
			instant string
			valid   bool
		}{
			{"2026-10-13T09:00:00+24:00", false},
			{"2026-10-13T09:00:00+00:60", false},
			{"2026-10-13T09:00:00-24:00", false},
			{"2026-10-13T09:00:00-00:60", false},
			{"2026-10-13T09:00:00,123+03:00", false},
			{"2026-10-13T09:00:00.1234567890+03:00", false},
			{"2026-10-13T09:00:00.+03:00", false},
			{"2026-02-30T09:00:00+03:00", false},
			{"2026-10-13T24:00:00+03:00", false},
			{"2026-10-13T09:00:60+03:00", false},
			{"1999-12-31T23:59:59Z", false},
			{"2100-01-01T00:00:00Z", false},
			{"2000-01-01T00:00:00Z", true},
			{"2099-12-31T23:59:59.999999999Z", true},
			{"2026-10-13T09:00:00.1+23:59", true},
			{"2026-10-13T09:00:00.000000001-23:59", true},
			{"2026-10-13T09:00:00+00:00", true},
			{"2026-10-13T09:00:00-00:00", true},
			{"2026-10-13T09:00:00.123456789+03:00", true},
		} {
			t.Run(operation+"/"+tc.instant, func(t *testing.T) {
				clock := fmt.Sprintf(`{"owner":"serving","instanceID":"33333333-3333-4333-8333-333333333333","source":"owner-injected","fixtureID":"44444444-4444-4444-8444-444444444444","generation":"1","instant":%q,"injectedConsumers":["bootstrap.Registry.Now","bootstrap.Options.Now","httpapi.ScopedOptions.Now","httpapi.JWTOptions.Now","authhttp.Client.now"],"healthy":true}`, tc.instant)
				data := clock
				if operation == "clock.checkpoint" {
					data = fmt.Sprintf(`{"clock":%s,"checkpointSHA256":%q}`, clock, strings.Repeat("a", 64))
				}
				body := fmt.Sprintf(`{"version":1,"project":"umir-backend-0123456789abcdef01234567","requestID":"22222222-2222-4222-8222-222222222222","operation":%q,"data":%s}`, operation, data)
				socket := helperServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if _, err := io.Copy(io.Discard, r.Body); err != nil {
						t.Error(err)
					}
					if _, err := io.WriteString(w, body); err != nil {
						t.Error(err)
					}
				}))
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				_, err := requestControl(ctx, socket, "private-credential", []byte(strings.Replace(helperInput, "clock.read", operation, 1)))
				if (err == nil) != tc.valid {
					t.Fatalf("valid=%t error=%v", tc.valid, err)
				}
			})
		}
	}
}
