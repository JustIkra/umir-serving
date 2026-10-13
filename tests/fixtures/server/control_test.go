package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const testProject = "umir-backend-0123456789abcdef01234567"
const testInstance = "33333333-3333-4333-8333-333333333333"
const testFixture = "44444444-4444-4444-8444-444444444444"

func controlForTest(t *testing.T, ready func(context.Context) bool) *control {
	t.Helper()
	c, err := newControl(testProject, testInstance, testFixture, "2026-10-13T09:00:00+03:00", "1", "test-control-credential", strings.Repeat("a", 64), strings.Repeat("b", 64), ready)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func input(id int, op string, data string) string {
	return fmt.Sprintf(`{"version":1,"project":%q,"requestID":"22222222-2222-4222-8222-%012x","operation":%q,"data":%s}`, testProject, id, op, data)
}
func send(t *testing.T, c http.Handler, body string, token string, ctx context.Context) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/fixture/v1/control", strings.NewReader(body)).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	c.ServeHTTP(w, r)
	return w
}
func errorCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var e struct {
		Error struct {
			Code   string `json:"code"`
			Effect string `json:"effect"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	if e.Error.Effect != "none" {
		t.Fatalf("unexpected effect %q", e.Error.Effect)
	}
	return e.Error.Code
}
func TestProtocolRejectsMalformedAndPrivateInputs(t *testing.T) {
	c := controlForTest(t, func(context.Context) bool { return false })
	valid := input(1, "clock.read", "{}")
	cases := []string{strings.Replace(valid, `"version":1`, `"version":1,"version":1`, 1), strings.Replace(valid, `"data":{}`, `"data":{"password":"private-sentinel"}`, 1), valid + " {}", strings.Replace(valid, `"data":{}`, `"data":null`, 1), strings.Replace(valid, `"version":1`, `"Version":1`, 1), strings.Replace(valid, `"data":{}`, `"data":{"x":`+strings.Repeat("[", 17)+"0"+strings.Repeat("]", 17)+"}", 1), strings.Replace(valid, "clock.read", "\xff", 1), strings.Replace(valid, `"version":1,`, "", 1), strings.Replace(valid, `"data":{}`, `"data":[]`, 1)}
	for _, body := range cases {
		w := send(t, c, body, "test-control-credential", context.Background())
		if w.Code != 400 || errorCode(t, w) != "InvalidRequest" {
			t.Fatalf("malformed accepted: status=%d", w.Code)
		}
		if strings.Contains(w.Body.String(), "private-sentinel") {
			t.Fatal("private input echoed")
		}
	}
	w := send(t, c, strings.Repeat(" ", 16385), "test-control-credential", context.Background())
	if w.Code != 413 {
		t.Fatalf("oversize status=%d", w.Code)
	}
}
func TestClockCASReplayCheckpointAndBackward(t *testing.T) {
	c := controlForTest(t, func(context.Context) bool { return true })
	body := input(2, "clock.set", `{"expectedInstanceID":"33333333-3333-4333-8333-333333333333","expectedGeneration":"1","instant":"2026-10-14T09:00:00+03:00"}`)
	first := send(t, c, body, "test-control-credential", context.Background())
	if first.Code != 200 {
		t.Fatalf("set status=%d", first.Code)
	}
	replay := send(t, c, body, "test-control-credential", context.Background())
	if !bytes.Equal(first.Body.Bytes(), replay.Body.Bytes()) {
		t.Fatal("replay changed result")
	}
	var out struct {
		Data struct {
			Generation string `json:"generation"`
			Instant    string `json:"instant"`
		} `json:"data"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Data.Generation != "2" || out.Data.Instant != "2026-10-14T09:00:00+03:00" {
		t.Fatal("clock did not advance once")
	}
	if !c.now().Equal(time.Date(2026, 10, 14, 6, 0, 0, 0, time.UTC)) {
		t.Fatal("readback does not match injected clock")
	}
	for _, tc := range []struct {
		id                        int
		generation, instant, want string
	}{{3, "1", "2026-10-14T09:00:00+03:00", "ClockConflict"}, {4, "2", "2026-10-13T09:00:00+03:00", "ClockBackward"}} {
		w := send(t, c, input(tc.id, "clock.set", fmt.Sprintf(`{"expectedInstanceID":%q,"expectedGeneration":%q,"instant":%q}`, testInstance, tc.generation, tc.instant)), "test-control-credential", context.Background())
		if w.Code != 409 || errorCode(t, w) != tc.want {
			t.Fatal("clock conflict not enforced")
		}
	}
	conflict := send(t, c, strings.Replace(body, "2026-10-14", "2026-10-15", 1), "test-control-credential", context.Background())
	if conflict.Code != 409 {
		t.Fatal("request ID conflict reapplied")
	}
	checkpoint := send(t, c, input(5, "clock.checkpoint", fmt.Sprintf(`{"expectedInstanceID":%q,"expectedGeneration":"2"}`, testInstance)), "test-control-credential", context.Background())
	if checkpoint.Code != 200 || !strings.Contains(checkpoint.Body.String(), `"checkpointSHA256"`) {
		t.Fatal("checkpoint unavailable")
	}
	same := send(t, c, input(6, "clock.set", fmt.Sprintf(`{"expectedInstanceID":%q,"expectedGeneration":"2","instant":"2026-10-14T09:00:00+03:00"}`, testInstance)), "test-control-credential", context.Background())
	if same.Code != 200 || !strings.Contains(same.Body.String(), `"generation":"3"`) {
		t.Fatal("same instant CAS did not increment")
	}
}
func TestPrivacyAuthenticationCapabilitiesAndCacheBound(t *testing.T) {
	c := controlForTest(t, func(context.Context) bool { return false })
	for _, token := range []string{"", "wrong-credential"} {
		w := send(t, c, input(1, "clock.read", "{}"), token, context.Background())
		if w.Code != 401 {
			t.Fatal("unauthenticated control succeeded")
		}
	}
	w := send(t, c, strings.Replace(input(1, "clock.read", "{}"), testProject, "umir-backend-ffffffffffffffffffffffff", 1), "test-control-credential", context.Background())
	if w.Code != 403 || errorCode(t, w) != "WrongProject" {
		t.Fatal("wrong project succeeded")
	}
	for i, op := range []string{"owner.fault.arm", "scheduler.tick", "owner.jobs", "owner.jobs.discover", "bootstrap.read", "feedback.observe", "sink.reset"} {
		w := send(t, c, input(i+20, op, "{}"), "test-control-credential", context.Background())
		if w.Code != 404 || errorCode(t, w) != "UnsupportedOperation" {
			t.Fatal("unsupported capability succeeded")
		}
	}
	w = send(t, c, input(40, "fixture.ready", "{}"), "test-control-credential", context.Background())
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"ready":false`) {
		t.Fatal("readiness fabricated")
	}
	c = controlForTest(t, func(context.Context) bool { return true })
	for i := 1; i <= 256; i++ {
		w = send(t, c, input(i, "clock.read", "{}"), "test-control-credential", context.Background())
		if w.Code != 200 {
			t.Fatal("early cache exhaustion")
		}
	}
	w = send(t, c, input(257, "clock.read", "{}"), "test-control-credential", context.Background())
	if w.Code != 413 || errorCode(t, w) != "CapacityExceeded" {
		t.Fatal("cache silently evicted")
	}
	w = send(t, c, input(1, "clock.read", "{}"), "test-control-credential", context.Background())
	if w.Code != 200 {
		t.Fatal("cached request lost at capacity")
	}
}
func TestCancellationNeverMutatesClock(t *testing.T) {
	c := controlForTest(t, func(ctx context.Context) bool { <-ctx.Done(); return false })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	body := input(1, "clock.set", fmt.Sprintf(`{"expectedInstanceID":%q,"expectedGeneration":"1","instant":"2026-10-15T09:00:00+03:00"}`, testInstance))
	w := send(t, c, body, "test-control-credential", ctx)
	if w.Code == 200 {
		t.Fatal("canceled set succeeded")
	}
	read := send(t, c, input(2, "clock.read", "{}"), "test-control-credential", context.Background())
	if read.Code != 200 || !strings.Contains(read.Body.String(), `"generation":"1"`) {
		t.Fatal("canceled mutation changed time")
	}
	ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	send(t, c, input(3, "fixture.ready", "{}"), "test-control-credential", ctx)
	if time.Since(start) > time.Second {
		t.Fatal("readiness ignored cancellation")
	}
}
func TestConcurrentClockCASAppliesOnlyOnce(t *testing.T) {
	c := controlForTest(t, func(context.Context) bool { return true })
	var wg sync.WaitGroup
	results := make(chan int, 16)
	for i := 1; i <= 16; i++ {
		wg.Go(func() {
			w := send(t, c, input(i, "clock.set", fmt.Sprintf(`{"expectedInstanceID":%q,"expectedGeneration":"1","instant":"2026-10-15T09:00:00+03:00"}`, testInstance)), "test-control-credential", context.Background())
			results <- w.Code
		})
	}
	wg.Wait()
	close(results)
	applied := 0
	for status := range results {
		if status == 200 {
			applied++
		} else if status != 409 && status != 413 {
			t.Fatalf("unexpected CAS status=%d", status)
		}
	}
	if applied != 1 {
		t.Fatalf("applied=%d", applied)
	}
}

func TestCanceledMutationIsNeverRetryable(t *testing.T) {
	c := controlForTest(t, func(context.Context) bool { return true })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	body := input(77, "clock.set", `{"expectedInstanceID":"33333333-3333-4333-8333-333333333333","expectedGeneration":"1","instant":"2026-10-14T09:00:00+03:00"}`)
	w := send(t, c, body, "test-control-credential", ctx)
	var out struct {
		Error struct {
			Retryable bool `json:"retryable"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Error.Retryable {
		t.Fatal("mutation error marked retryable")
	}
}
