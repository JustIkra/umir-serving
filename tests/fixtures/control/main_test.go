package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JustIkra/umir-serving/tests/fixtures/catalog"
)

func helperServer(t *testing.T, handler http.Handler) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "si-helper-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	path := filepath.Join(dir, "socket")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	s := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	done := make(chan struct{})
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
		<-done
	})
	go func() {
		defer close(done)
		if err := s.Serve(l); err != nil && err != http.ErrServerClosed {
			t.Error(err)
		}
	}()
	return path
}

const helperInput = `{"version":1,"project":"umir-backend-0123456789abcdef01234567","requestID":"22222222-2222-4222-8222-222222222222","operation":"clock.read","data":{}}`

func TestHelperRejectsUnredactedResponseAndRedirect(t *testing.T) {
	for _, body := range []string{`{"version":1,"project":"umir-backend-0123456789abcdef01234567","requestID":"22222222-2222-4222-8222-222222222222","operation":"clock.read","error":{"code":"NotReady","effect":"none","retryable":true,"password":"private-sentinel"}}`, strings.Repeat("x", 262145)} {
		socket := helperServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, err := io.Copy(io.Discard, r.Body); err != nil {
				t.Error(err)
			}
			if _, err := io.WriteString(w, body); err != nil {
				t.Error(err)
			}
		}))
		if _, err := requestControl(context.Background(), socket, "private-credential", []byte(helperInput)); err == nil {
			t.Fatal("unsafe response accepted")
		}
	}
	socket := helperServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://external.invalid/", http.StatusFound)
	}))
	if _, err := requestControl(context.Background(), socket, "private-credential", []byte(helperInput)); err == nil {
		t.Fatal("redirect accepted")
	}
}
func TestHelperBoundsCancellationAndKeepsCredentialOutOfInput(t *testing.T) {
	socket := helperServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer private-credential" {
			t.Error("credential missing")
		}
		if r.URL.Path != "/fixture/v1/control" {
			t.Error("wrong control path")
		}
		<-r.Context().Done()
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := requestControl(ctx, socket, "private-credential", []byte(helperInput)); err == nil {
		t.Fatal("canceled helper succeeded")
	}
	if time.Since(start) > time.Second {
		t.Fatal("helper not bounded")
	}
	if _, err := requestControl(context.Background(), socket, "private-credential", []byte(helperInput+" {}")); err == nil {
		t.Fatal("trailing input accepted")
	}
}
func TestHelperPrivateFilesRejectPublicAndSymlink(t *testing.T) {
	path := filepath.Join(physicalTempDir(t), "key")
	if err := os.WriteFile(path, []byte("private-credential"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivate(path, 4096); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivate(path, 4096); err == nil {
		t.Fatal("public private-file accepted")
	}
}

func TestHelperPrivateFileRejectsSymlinkParent(t *testing.T) {
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

func TestHelperRejectsRetryableMutationError(t *testing.T) {
	raw := strings.Replace(helperInput, "clock.read", "clock.set", 1)
	body := `{"version":1,"project":"umir-backend-0123456789abcdef01234567","requestID":"22222222-2222-4222-8222-222222222222","operation":"clock.set","error":{"code":"WaitExpired","effect":"none","retryable":true}}`
	socket := helperServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusRequestTimeout)
		if _, err := io.WriteString(w, body); err != nil {
			t.Error(err)
		}
	}))
	if _, err := requestControl(context.Background(), socket, "private-credential", []byte(raw)); err == nil {
		t.Fatal("retryable mutation accepted")
	}
}

func TestCatalogResponseBindingsAndStrictFields(t *testing.T) {
	request := []byte(`{"amendment":"umir-owner-profiles/0.1.0-unpublished","project":"umir-backend-0123456789abcdef01234567","requestID":"22222222-2222-4222-8222-000000000001","expectedInstanceID":"33333333-3333-4333-8333-333333333333","expectedGeneration":"1","action":"catalog-report"}`)
	req, err := validCatalogRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	raw := `{"amendment":"umir-owner-profiles/0.1.0-unpublished","requestID":"22222222-2222-4222-8222-000000000001","project":"umir-backend-0123456789abcdef01234567","instanceID":"33333333-3333-4333-8333-333333333333","clock":{"instant":"2026-10-13T09:00:00+03:00","generation":"1","instanceID":"33333333-3333-4333-8333-333333333333","source":"owner","healthy":true},"binarySHA256":"` + strings.Repeat("a", 64) + `","configSHA256":"` + strings.Repeat("b", 64) + `","catalog":{"owner":"serving","version":"go1.26.9-embedded","archiveSHA256":"` + strings.Repeat("c", 64) + `","archiveBytes":401710,"buildToolchain":"go1.26.9","ready":true,"provenanceSHA256":"` + strings.Repeat("d", 64) + `"}}`
	if !validCatalogResponse([]byte(raw), req, 200, strings.Repeat("b", 64)) {
		t.Fatal("valid catalog response rejected")
	}
	for _, tc := range []struct{ from, to string }{{`"generation":"1"`, `"generation":"2"`}, {`"source":"owner"`, `"source":"host"`}, {`"ready":true`, `"ready":false`}, {`"healthy":true`, `"healthy":false`}, {`"owner":"serving"`, `"owner":"auth"`}, {`"archiveBytes":401710`, `"archiveBytes":0`}, {`"archiveBytes":401710`, `"archiveBytes":16777217`}, {`"archiveBytes":401710`, `"archiveBytes":401710.5`}, {`"version":"go1.26.9-embedded"`, `"version":"go1.26.9"`}, {`"generation":"1"`, `"generation":"1","generation":"1"`}, {`"catalog":{`, `"catalog":{"extra":true,`}, {`"clock":{`, `"clock":{"extra":true,`}, {`"configSHA256":"` + strings.Repeat("b", 64), `"configSHA256":"` + strings.Repeat("e", 64)}} {
		if validCatalogResponse([]byte(strings.Replace(raw, tc.from, tc.to, 1)), req, 200, strings.Repeat("b", 64)) {
			t.Fatalf("bad catalog response accepted: %s", tc.from)
		}
	}
	for _, bad := range []string{string(request) + " {}", strings.Replace(string(request), `"action":"catalog-report"`, `"action":"catalog-report","profile":"auth-rotation"`, 1), strings.Replace(string(request), `"action":"catalog-report"`, `"action":"publish-next"`, 1)} {
		if _, err := validCatalogRequest([]byte(bad)); err == nil {
			t.Fatal("bad catalog input accepted")
		}
	}
	for _, args := range [][]string{{"profile"}, {"profile", "--request", "/tmp/foreign.json"}, {"profile", "--request", catalog.RequestPath, "extra"}} {
		if err := run(context.Background(), args, strings.NewReader(""), io.Discard); err == nil {
			t.Fatal("non-fixed path accepted")
		}
	}
}
