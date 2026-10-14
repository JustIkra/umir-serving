package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/JustIkra/umir-serving/tests/fixtures/catalog"
)

func TestCatalogEndpointRejectsCredentialBeforeObservation(t *testing.T) {
	c := controlForTest(t, func(context.Context) bool { return true })
	r := httptest.NewRequest(http.MethodPost, "/owner-profile/v1", strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	newCatalogHandler(c).ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), `"error":"Unauthorized"`) {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func catalogInput() string {
	return `{"amendment":"umir-owner-profiles/0.1.0-unpublished","project":"umir-backend-0123456789abcdef01234567","requestID":"22222222-2222-4222-8222-000000000001","expectedInstanceID":"33333333-3333-4333-8333-333333333333","expectedGeneration":"1","action":"catalog-report"}`
}
func catalogSend(h http.Handler, body, token, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func TestCatalogStrictBindingAndReplay(t *testing.T) {
	c := controlForTest(t, func(context.Context) bool { return true })
	h := newCatalogHandler(c)
	calls := 0
	h.observe = func(_ context.Context, clock catalog.Clock) (catalog.CatalogReport, error) {
		calls++
		report := clock.CatalogSnapshot()
		report.Catalog = catalog.Fact{Owner: "serving", Version: catalog.Version, ArchiveSHA256: strings.Repeat("c", 64), ArchiveBytes: 10, BuildToolchain: "go1.26.9", Ready: true, ProvenanceSHA256: strings.Repeat("d", 64)}
		return report, nil
	}
	body := catalogInput()
	w := catalogSend(h, body, "test-control-credential", catalog.Path)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if replay := catalogSend(h, body, "test-control-credential", catalog.Path); replay.Body.String() != w.Body.String() || calls != 1 {
		t.Fatal("read-only replay repeated observation")
	}
	changed := strings.Replace(body, `"expectedGeneration":"1"`, `"expectedGeneration":"2"`, 1)
	if w := catalogSend(h, changed, "test-control-credential", catalog.Path); w.Code != 409 || !strings.Contains(w.Body.String(), "RequestConflict") {
		t.Fatal("request conflict accepted")
	}
	before := c.readClock()
	for _, tc := range []struct {
		from, to string
		status   int
	}{{`"expectedInstanceID":"33333333`, `"expectedInstanceID":"55555555`, 409}, {`"expectedGeneration":"1"`, `"expectedGeneration":"2"`, 409}, {`"expectedGeneration":"1"`, `"expectedGeneration":1`, 400}, {`"expectedGeneration":"1"`, `"expectedGeneration":null`, 400}, {`"action":"catalog-report"`, `"action":"publish-next"`, 400}, {`"amendment":`, `"amendment":"x","amendment":`, 400}, {`"action":"catalog-report"`, `"action":"catalog-report","profile":"auth-rotation"`, 400}, {testProject, "umir-backend-ffffffffffffffffffffffff", 403}} {
		raw := strings.Replace(strings.Replace(body, "000000000001", "000000000002", 1), tc.from, tc.to, 1)
		if w := catalogSend(h, raw, "test-control-credential", catalog.Path); w.Code != tc.status {
			t.Fatalf("bad binding accepted: %s status=%d", tc.from, w.Code)
		}
	}
	for _, raw := range []string{body + " {}", strings.Repeat(" ", 65537), strings.Replace(body, "catalog-report", "\xff", 1)} {
		if w := catalogSend(h, raw, "test-control-credential", catalog.Path); w.Code != 400 {
			t.Fatal("malformed request accepted")
		}
	}
	for _, path := range []string{catalog.Path + "?x=1", catalog.Path + "?", catalog.Path + "/"} {
		if w := catalogSend(h, body, "test-control-credential", path); w.Code != 400 {
			t.Fatal("path alias accepted")
		}
	}
	if calls != 1 || c.readClock().Generation != before.Generation || c.readClock().Instant != before.Instant {
		t.Fatal("refusal caused observation or clock mutation")
	}
	h = newCatalogHandler(c)
	h.observe = func(_ context.Context, clock catalog.Clock) (catalog.CatalogReport, error) {
		return clock.CatalogSnapshot(), nil
	}
	if w := catalogSend(h, body, "test-control-credential", catalog.Path); w.Code != 200 {
		t.Fatal("fresh capacity fixture failed")
	}
	for i := 2; i <= 64; i++ {
		raw := strings.Replace(body, "000000000001", fmt.Sprintf("%012x", i), 1)
		if w := catalogSend(h, raw, "test-control-credential", catalog.Path); w.Code != 200 {
			t.Fatal("capacity reached early", i)
		}
	}
	if w := catalogSend(h, strings.Replace(body, "000000000001", "000000000065", 1), "test-control-credential", catalog.Path); w.Code != 413 {
		t.Fatal("capacity unbounded")
	}
}
func TestCatalogUnavailableOwnerDoesNotObserve(t *testing.T) {
	c := controlForTest(t, func(context.Context) bool { return false })
	h := newCatalogHandler(c)
	h.observe = func(context.Context, catalog.Clock) (catalog.CatalogReport, error) {
		t.Fatal("observed unavailable owner")
		return catalog.CatalogReport{}, nil
	}
	if w := catalogSend(h, catalogInput(), "test-control-credential", catalog.Path); w.Code != 503 {
		t.Fatal("unavailable owner accepted")
	}
}

func TestCatalogRefusalReplayIsStableAndBounded(t *testing.T) {
	ready := false
	c := controlForTest(t, func(context.Context) bool { return ready })
	h := newCatalogHandler(c)
	h.observe = func(_ context.Context, clock catalog.Clock) (catalog.CatalogReport, error) {
		return clock.CatalogSnapshot(), nil
	}
	first := catalogSend(h, catalogInput(), "test-control-credential", catalog.Path)
	if first.Code != 503 {
		t.Fatal("missing owner was admitted")
	}
	ready = true
	if replay := catalogSend(h, catalogInput(), "test-control-credential", catalog.Path); replay.Code != 503 || replay.Body.String() != first.Body.String() {
		t.Fatal("refusal replay changed measured result")
	}
	for i := 2; i <= 64; i++ {
		raw := strings.Replace(strings.Replace(catalogInput(), "000000000001", fmt.Sprintf("%012x", i), 1), `"expectedGeneration":"1"`, `"expectedGeneration":"2"`, 1)
		if w := catalogSend(h, raw, "test-control-credential", catalog.Path); w.Code != 409 {
			t.Fatal("stale observation admitted")
		}
	}
	if w := catalogSend(h, strings.Replace(catalogInput(), "000000000001", "000000000065", 1), "test-control-credential", catalog.Path); w.Code != 413 {
		t.Fatal("refusal requests exceeded bound")
	}
}
