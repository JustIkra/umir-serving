package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/JustIkra/umir-serving/tests/fixtures/catalog"
)

type catalogClock struct{ owner *control }

func (v catalogClock) CatalogSnapshot() catalog.CatalogReport {
	c := v.owner
	c.clockMu.RLock()
	defer c.clockMu.RUnlock()
	return catalog.CatalogReport{Amendment: catalog.Amendment, Project: c.project, InstanceID: c.instance, BinarySHA256: c.binary, ConfigSHA256: c.config, Clock: catalog.OwnerClock{InstanceID: c.instance, Generation: fmt.Sprint(c.generation), Instant: c.instant.In(time.FixedZone("Europe/Moscow", 10800)).Format(time.RFC3339Nano), Source: "owner", Healthy: true}}
}

type catalogHandler struct {
	owner   *control
	observe func(context.Context, catalog.Clock) (catalog.CatalogReport, error)
	cache   map[string]cached
}

func newCatalogHandler(c *control) *catalogHandler {
	return &catalogHandler{owner: c, observe: catalog.Observe, cache: make(map[string]cached)}
}
func (h *catalogHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != catalog.Path {
		h.owner.ServeHTTP(w, r)
		return
	}
	requestID := "00000000-0000-4000-8000-000000000000"
	cacheKey, canonicalKey := "", ""
	reject := func(status int, code string) {
		raw, err := json.Marshal(struct {
			Amendment string `json:"amendment"`
			RequestID string `json:"requestID"`
			Error     string `json:"error"`
		}{catalog.Amendment, requestID, code})
		if err != nil {
			panic(err)
		}
		if cacheKey != "" {
			h.cache[cacheKey] = cached{canonical: canonicalKey, status: status, body: raw}
		}
		writeControl(w, status, raw)
	}
	if r.Method != http.MethodPost || r.URL.RawQuery != "" || r.URL.ForceQuery || r.URL.RawPath != "" {
		reject(400, "InvalidRequest")
		return
	}
	credentials := r.Header.Values("Authorization")
	if len(credentials) != 1 || subtle.ConstantTimeCompare([]byte(credentials[0]), []byte("Bearer "+h.owner.credential)) != 1 {
		reject(401, "Unauthorized")
		return
	}
	content, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || content != "application/json" {
		reject(400, "InvalidRequest")
		return
	}
	select {
	case h.owner.gate <- struct{}{}:
		defer func() { <-h.owner.gate }()
	default:
		reject(413, "ProfileCapacityExceeded")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	raw, err := io.ReadAll(io.LimitReader(r.Body, 65537))
	if err != nil || len(raw) > 65536 {
		reject(400, "InvalidRequest")
		return
	}
	var req catalog.Request
	if decodeObject(raw, &req, "amendment", "project", "requestID", "expectedInstanceID", "expectedGeneration", "action") != nil || req.Amendment != catalog.Amendment || !uuidPattern.MatchString(req.RequestID) || !uuidPattern.MatchString(req.ExpectedInstanceID) || !generationPattern.MatchString(req.ExpectedGeneration) || !projectPattern.MatchString(req.Project) {
		reject(400, "InvalidRequest")
		return
	}
	requestID = req.RequestID
	if req.Project != h.owner.project {
		reject(403, "OwnershipMismatch")
		return
	}
	if req.Action != "catalog-report" {
		reject(400, "UnsupportedAction")
		return
	}
	canon, err := canonical(raw)
	if err != nil {
		reject(400, "InvalidRequest")
		return
	}
	select {
	case h.owner.serial <- struct{}{}:
		defer func() { <-h.owner.serial }()
	case <-ctx.Done():
		reject(503, "ProfileExpired")
		return
	}
	if ctx.Err() != nil {
		reject(503, "ProfileExpired")
		return
	}
	if previous, ok := h.cache[req.RequestID]; ok {
		if previous.canonical != string(canon) {
			reject(409, "RequestConflict")
			return
		}
		writeControl(w, previous.status, previous.body)
		return
	}
	if len(h.cache) >= 64 {
		reject(413, "ProfileCapacityExceeded")
		return
	}
	cacheKey, canonicalKey = req.RequestID, string(canon)
	snapshot := catalogClock{h.owner}.CatalogSnapshot()
	if req.ExpectedInstanceID != snapshot.InstanceID || req.ExpectedGeneration != snapshot.Clock.Generation {
		reject(409, "ClockConflict")
		return
	}
	if !h.owner.ready(ctx) || ctx.Err() != nil {
		reject(503, "ProfileAdmissionFailed")
		return
	}
	report, err := h.observe(ctx, catalogClock{h.owner})
	if err != nil || ctx.Err() != nil {
		reject(503, "ProfileAdmissionFailed")
		return
	}
	report.RequestID = req.RequestID
	body, err := json.Marshal(report)
	if err != nil {
		reject(503, "ProfileAdmissionFailed")
		return
	}
	h.cache[req.RequestID] = cached{canonical: string(canon), status: 200, body: body}
	writeControl(w, 200, body)
}

func binaryDigest() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, readErr := io.Copy(h, f)
	if err := errors.Join(readErr, f.Close()); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func serve(ctx context.Context) error {
	cfg, digest, err := loadConfig()
	if err != nil {
		return err
	}
	keys, credential, err := readKeys(cfg)
	if err != nil {
		return err
	}
	instance, err := newUUID()
	if err != nil {
		return err
	}
	epoch, err := newUUID()
	if err != nil {
		return err
	}
	binary, err := binaryDigest()
	if err != nil {
		return err
	}
	c, err := newControl(cfg.Project, instance, cfg.FixtureID, cfg.InitialInstant, cfg.ClockGeneration, credential, binary, digest, func(context.Context) bool { return false })
	if err != nil {
		return err
	}
	app, err := compose(cfg, keys, c.now, epoch, nil)
	if err != nil {
		return errors.New("real Serving composition rejected config")
	}
	defer app.CloseIdleConnections()
	c.ready = func(ctx context.Context) bool { return ownerReady(ctx, app, keys.Lifecycle, epoch) }
	unix, err := privateListener(cfg.ControlSocket)
	if err != nil {
		return errors.New("private control listener unavailable")
	}
	tcp, err := net.Listen("tcp", ":8080")
	if err != nil {
		return errors.Join(err, unix.Close())
	}
	return serveListeners(ctx, app, newCatalogHandler(c), tcp, unix)
}
func healthcheck(ctx context.Context) error {
	cfg, digest, err := loadConfig()
	if err != nil {
		return err
	}
	b, err := readPrivate(cfg.ControlCredentialFile.Path, 4096)
	if err != nil {
		return err
	}
	id, err := newUUID()
	if err != nil {
		return err
	}
	body, err := json.Marshal(request{Version: 1, Project: cfg.Project, RequestID: id, Operation: "fixture.ready", Data: json.RawMessage("{}")})
	if err != nil {
		return err
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", cfg.ControlSocket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://fixture"+controlPath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	r.Header.Set("Authorization", "Bearer "+string(b))
	r.Header.Set("Content-Type", "application/json")
	res, err := client.Do(r)
	if err != nil {
		return errors.New("fixture unavailable")
	}
	raw, readErr := io.ReadAll(io.LimitReader(res.Body, 262145))
	if err := errors.Join(readErr, res.Body.Close()); err != nil {
		return errors.New("fixture read unavailable")
	}
	var out struct {
		Version   int    `json:"version"`
		Project   string `json:"project"`
		RequestID string `json:"requestID"`
		Operation string `json:"operation"`
		Data      struct {
			Target       string   `json:"target"`
			Ready        bool     `json:"ready"`
			Protocol     string   `json:"protocol"`
			InstanceID   string   `json:"instanceID"`
			BinarySHA256 string   `json:"binarySHA256"`
			ConfigSHA256 string   `json:"configSHA256"`
			Capabilities []string `json:"capabilities"`
		} `json:"data"`
	}
	if res.StatusCode != 200 || len(raw) > 262144 || json.Unmarshal(raw, &out) != nil || out.Version != 1 || out.Project != cfg.Project || out.RequestID != id || out.Operation != "fixture.ready" || out.Data.Target != "serving" || out.Data.Protocol != protocolVersion || !out.Data.Ready || !uuidPattern.MatchString(out.Data.InstanceID) || !validDigest(out.Data.BinarySHA256) || out.Data.ConfigSHA256 != digest {
		return errors.New("fixture not ready")
	}
	return nil
}
func run(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("expected serve or healthcheck")
	}
	switch args[0] {
	case "serve":
		return serve(ctx)
	case "healthcheck":
		return healthcheck(ctx)
	default:
		return errors.New("unsupported command")
	}
}
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		if _, writeErr := fmt.Fprintln(os.Stderr, "Serving fixture failed closed"); writeErr != nil {
			os.Exit(1)
		}
		os.Exit(1)
	}
}
