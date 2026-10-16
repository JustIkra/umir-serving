package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func validUUID(value string) bool { return uuidV4.MatchString(value) }

func TestInstanceIDUsesEnvironmentOrRandomUUID(t *testing.T) {
	t.Setenv("SERVING_INSTANCE_ID", "serving-a")
	if id, err := instanceID(); err != nil || id != "serving-a" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	t.Setenv("SERVING_INSTANCE_ID", "")
	first, err := instanceID()
	if err != nil || !validUUID(first) {
		t.Fatalf("random id=%q err=%v", first, err)
	}
	second, err := instanceID()
	if err != nil || first == second {
		t.Fatalf("random ids repeat: %q %q err=%v", first, second, err)
	}
	for _, bad := range []string{"has space", "-leading", strings.Repeat("a", 65), "line\nbreak"} {
		t.Setenv("SERVING_INSTANCE_ID", bad)
		id, err := instanceID()
		if err == nil || strings.Contains(err.Error(), bad) || !validUUID(id) {
			t.Fatalf("invalid SERVING_INSTANCE_ID %q: id=%q err=%v", bad, id, err)
		}
	}
}

func TestProcessLoggerWritesJSONWithInstanceID(t *testing.T) {
	t.Setenv("SERVING_INSTANCE_ID", "serving-b")
	var out bytes.Buffer
	logger, err := newProcessLogger(&out)
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("process.started")
	var record map[string]any
	if err := json.Unmarshal(out.Bytes(), &record); err != nil {
		t.Fatalf("not JSON: %v: %q", err, out.String())
	}
	if record["instance_id"] != "serving-b" || record["service"] != "umir-serving" || record["log_schema_version"] != float64(1) || record["version"] == "" {
		t.Fatalf("record=%v", record)
	}
}

func TestStartupErrorTextDropsTheURL(t *testing.T) {
	_, err := url.Parse(fmt.Sprintf("http://user:pass@bad%chost/", ' '))
	if err == nil {
		t.Fatal("URL parsed")
	}
	text := startupErrorText(errors.Join(err))
	if strings.Contains(text, "pass") || strings.Contains(text, "bad host") || !regexp.MustCompile(`^parse: `).MatchString(text) {
		t.Fatalf("text=%q", text)
	}
}

// lockedBuffer lets the server goroutine and the test share one log buffer.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *lockedBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}

func TestServerErrorLogKeepsOnlyAKind(t *testing.T) {
	var out lockedBuffer
	logger, err := newProcessLogger(&out)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("CANARY-PANIC-VALUE")
	}))
	server.Config.ErrorLog = newServerErrorLog(logger)
	server.Start()
	defer server.Close()
	response, err := server.Client().Get(server.URL)
	if err == nil {
		if closeErr := response.Body.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(out.String(), "http.server_error") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	text := out.String()
	if !strings.Contains(text, `"event":"http.server_error"`) || !strings.Contains(text, `"kind":"panic"`) {
		t.Fatalf("no classified record: %q", text)
	}
	host := strings.TrimPrefix(server.URL, "http://")
	for _, leaked := range []string{"CANARY-PANIC-VALUE", "127.0.0.1", host, "goroutine", "panic serving"} {
		if strings.Contains(text, leaked) {
			t.Fatalf("log contains %q: %s", leaked, text)
		}
	}
	for message, kind := range map[string]string{"http: TLS handshake error from 203.0.113.9:1: EOF": "tls_handshake", "http: Accept error: x; retrying in 5ms": "accept", "something else 203.0.113.9": "other"} {
		out.Reset()
		newServerErrorLog(logger).Print(message)
		if !strings.Contains(out.String(), `"kind":"`+kind+`"`) || strings.Contains(out.String(), "203.0.113.9") {
			t.Fatalf("%q: %s", message, out.String())
		}
	}
}

func TestHealthcheckIgnoresInvalidInstanceID(t *testing.T) {
	t.Setenv("SERVING_INSTANCE_ID", "bad id!")
	if _, err := processLogger([]string{"server", "healthcheck"}, io.Discard); err != nil {
		t.Fatalf("healthcheck must not depend on SERVING_INSTANCE_ID: %v", err)
	}
	if _, err := processLogger([]string{"server"}, io.Discard); err == nil {
		t.Fatal("the server must still reject an invalid SERVING_INSTANCE_ID")
	}
}
