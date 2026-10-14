package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const protocolVersion = "umir-operational-fixtures/1.1.0"
const controlPath = "/fixture/v1/control"
const maxRequest = 16384
const maxGeneration = uint64(9999999999999999999)

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
var projectPattern = regexp.MustCompile(`^umir-backend-[0-9a-f]{24}$`)
var generationPattern = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)
var instantPattern = regexp.MustCompile(`^20[0-9]{2}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,9})?(Z|[+-][0-9]{2}:[0-9]{2})$`)

type clockView struct {
	Owner             string   `json:"owner"`
	InstanceID        string   `json:"instanceID"`
	Source            string   `json:"source"`
	FixtureID         string   `json:"fixtureID"`
	Generation        string   `json:"generation"`
	Instant           string   `json:"instant"`
	InjectedConsumers []string `json:"injectedConsumers"`
	Healthy           bool     `json:"healthy"`
}
type response struct {
	Version   int      `json:"version"`
	Project   string   `json:"project"`
	RequestID string   `json:"requestID"`
	Operation string   `json:"operation"`
	Data      any      `json:"data,omitempty"`
	Error     *failure `json:"error,omitempty"`
}
type failure struct {
	Code      string `json:"code"`
	Effect    string `json:"effect"`
	Retryable bool   `json:"retryable"`
}
type request struct {
	Version   int             `json:"version"`
	Project   string          `json:"project"`
	RequestID string          `json:"requestID"`
	Operation string          `json:"operation"`
	Data      json.RawMessage `json:"data"`
}
type cached struct {
	canonical string
	status    int
	body      []byte
}
type control struct {
	project, instance, fixture, credential, binary, config string
	clockMu                                                sync.RWMutex
	instant                                                time.Time
	generation                                             uint64
	ready                                                  func(context.Context) bool
	gate                                                   chan struct{}
	serial                                                 chan struct{}
	cache                                                  map[string]cached
}

func newControl(project, instance, fixture, instant, generation, credential, binary, config string, ready func(context.Context) bool) (*control, error) {
	t, err := parseInstant(instant)
	g, gerr := strconv.ParseUint(generation, 10, 64)
	if err != nil || gerr != nil || !generationPattern.MatchString(generation) || !projectPattern.MatchString(project) || !uuidPattern.MatchString(instance) || !uuidPattern.MatchString(fixture) || credential == "" || ready == nil || !validDigest(binary) || !validDigest(config) {
		return nil, errors.New("invalid fixture binding")
	}
	return &control{project: project, instance: instance, fixture: fixture, instant: t, generation: g, credential: credential, binary: binary, config: config, ready: ready, gate: make(chan struct{}, 8), serial: make(chan struct{}, 1), cache: make(map[string]cached)}, nil
}
func validDigest(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 32 && strings.ToLower(s) == s
}
func parseInstant(s string) (time.Time, error) {
	if !instantPattern.MatchString(s) {
		return time.Time{}, errors.New("invalid instant")
	}
	if s[len(s)-1] != 'Z' {
		offset := s[len(s)-6:]
		hour, hourErr := strconv.Atoi(offset[1:3])
		minute, minuteErr := strconv.Atoi(offset[4:6])
		if hourErr != nil || minuteErr != nil || hour > 23 || minute > 59 {
			return time.Time{}, errors.New("invalid offset")
		}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil || t.Year() < 2000 || t.Year() > 2099 {
		return time.Time{}, errors.New("invalid instant")
	}
	return t, nil
}
func (c *control) now() time.Time { c.clockMu.RLock(); defer c.clockMu.RUnlock(); return c.instant }
func (c *control) clockLocked() clockView {
	return clockView{Owner: "serving", InstanceID: c.instance, Source: "owner-injected", FixtureID: c.fixture, Generation: strconv.FormatUint(c.generation, 10), Instant: c.instant.Format(time.RFC3339Nano), InjectedConsumers: []string{"bootstrap.Registry.Now", "bootstrap.Options.Now", "httpapi.ScopedOptions.Now", "httpapi.JWTOptions.Now", "authhttp.Client.now"}, Healthy: true}
}
func (c *control) readClock() clockView {
	c.clockMu.RLock()
	defer c.clockMu.RUnlock()
	return c.clockLocked()
}
func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}
func canonical(data []byte) ([]byte, error) {
	if !utf8.Valid(data) {
		return nil, errors.New("invalid UTF-8")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := scanJSON(d, 0); err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, errors.New("trailing JSON")
	}
	var value any
	d = json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := d.Decode(&value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}
func scanJSON(d *json.Decoder, depth int) error {
	tok, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	if depth >= 16 {
		return errors.New("JSON too deep")
	}
	switch delim {
	case '{':
		seen := make(map[string]bool)
		for d.More() {
			k, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := k.(string)
			if !ok || seen[name] {
				return errors.New("duplicate JSON field")
			}
			seen[name] = true
			if err := scanJSON(d, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := scanJSON(d, depth+1); err != nil {
				return err
			}
		}
	default:
		return errors.New("invalid JSON delimiter")
	}
	_, err = d.Token()
	return err
}
func decodeObject(data []byte, dst any, fields ...string) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return errors.New("object required")
	}
	if _, err := canonical(data); err != nil {
		return err
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return err
	}
	if len(object) != len(fields) {
		return errors.New("invalid fields")
	}
	for _, f := range fields {
		v, ok := object[f]
		if !ok || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return errors.New("missing field")
		}
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	return d.Decode(dst)
}
func encodeResponse(v response) ([]byte, error) { return json.Marshal(v) }
func (c *control) fail(req request, code string) (int, []byte) {
	status := map[string]int{"InvalidRequest": 400, "Unauthenticated": 401, "Forbidden": 403, "WrongProject": 403, "UnsupportedOperation": 404, "ClockConflict": 409, "ClockBackward": 409, "CapacityExceeded": 413, "NotReady": 503, "WaitExpired": 408}[code]
	if status == 0 {
		status = 503
	}
	b, err := encodeResponse(response{Version: 1, Project: c.project, RequestID: req.RequestID, Operation: req.Operation, Error: &failure{Code: code, Effect: "none", Retryable: (code == "NotReady" || code == "WaitExpired") && (req.Operation == "fixture.ready" || req.Operation == "clock.read" || req.Operation == "clock.checkpoint")}})
	if err != nil {
		panic(err)
	}
	return status, b
}
func writeControl(w http.ResponseWriter, status int, b []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if _, err := w.Write(b); err != nil {
		return
	}
}
func (c *control) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id, err := newUUID()
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	req := request{RequestID: id, Operation: "fixture.ready"}
	reject := func(code string) { status, b := c.fail(req, code); writeControl(w, status, b) }
	if r.Method != http.MethodPost || r.URL.Path != controlPath || r.URL.RawQuery != "" || r.URL.ForceQuery {
		reject("InvalidRequest")
		return
	}
	authorization := r.Header.Values("Authorization")
	if len(authorization) != 1 || subtle.ConstantTimeCompare([]byte(authorization[0]), []byte("Bearer "+c.credential)) != 1 {
		reject("Unauthenticated")
		return
	}
	content, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || content != "application/json" {
		reject("InvalidRequest")
		return
	}
	select {
	case c.gate <- struct{}{}:
		defer func() { <-c.gate }()
	default:
		reject("CapacityExceeded")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	data, err := io.ReadAll(io.LimitReader(r.Body, maxRequest+1))
	if err != nil {
		reject("InvalidRequest")
		return
	}
	if len(data) > maxRequest {
		reject("CapacityExceeded")
		return
	}
	var parsed request
	if err := decodeObject(data, &parsed, "version", "project", "requestID", "operation", "data"); err != nil || parsed.Version != 1 || !uuidPattern.MatchString(parsed.RequestID) || !projectPattern.MatchString(parsed.Project) || !knownOperation(parsed.Operation) || len(bytes.TrimSpace(parsed.Data)) == 0 || bytes.TrimSpace(parsed.Data)[0] != '{' {
		reject("InvalidRequest")
		return
	}
	req = parsed
	if req.Project != c.project {
		reject("WrongProject")
		return
	}
	canon, err := canonical(data)
	if err != nil {
		reject("InvalidRequest")
		return
	}
	select {
	case c.serial <- struct{}{}:
		defer func() { <-c.serial }()
	case <-ctx.Done():
		reject("WaitExpired")
		return
	}
	if ctx.Err() != nil {
		reject("WaitExpired")
		return
	}
	if previous, ok := c.cache[req.RequestID]; ok {
		if previous.canonical != string(canon) {
			reject("ClockConflict")
			return
		}
		writeControl(w, previous.status, previous.body)
		return
	}
	if len(c.cache) >= 256 {
		reject("CapacityExceeded")
		return
	}
	result, code := c.execute(ctx, req)
	var status int
	var body []byte
	if code != "" {
		status, body = c.fail(req, code)
	} else {
		status = 200
		body, err = encodeResponse(response{Version: 1, Project: c.project, RequestID: req.RequestID, Operation: req.Operation, Data: result})
		if err != nil {
			status, body = c.fail(req, "NotReady")
		}
	}
	c.cache[req.RequestID] = cached{canonical: string(canon), status: status, body: body}
	writeControl(w, status, body)
}
func knownOperation(s string) bool {
	switch s {
	case "fixture.ready", "bootstrap.read", "clock.read", "clock.set", "clock.checkpoint", "scheduler.tick", "owner.jobs", "owner.jobs.discover", "feedback.observe", "owner.fault.arm", "owner.fault.read", "owner.fault.clear", "sink.capture", "sink.correlation.bind", "sink.otp.write", "sink.fault.arm", "sink.fault.read", "sink.fault.clear", "sink.reset":
		return true
	}
	return false
}
func (c *control) execute(ctx context.Context, req request) (any, string) {
	switch req.Operation {
	case "fixture.ready", "clock.read":
		var empty struct{}
		if err := decodeObject(req.Data, &empty); err != nil {
			return nil, "InvalidRequest"
		}
		if req.Operation == "clock.read" {
			return c.readClock(), ""
		}
		ready := c.ready(ctx)
		if ctx.Err() != nil {
			return nil, "WaitExpired"
		}
		return struct {
			Target       string   `json:"target"`
			InstanceID   string   `json:"instanceID"`
			Protocol     string   `json:"protocol"`
			BinarySHA256 string   `json:"binarySHA256"`
			ConfigSHA256 string   `json:"configSHA256"`
			Ready        bool     `json:"ready"`
			Capabilities []string `json:"capabilities"`
		}{"serving", c.instance, protocolVersion, c.binary, c.config, ready, []string{"fixture.ready", "clock.read", "clock.set", "clock.checkpoint"}}, ""
	case "clock.set", "clock.checkpoint":
		var data struct {
			Instance   string `json:"expectedInstanceID"`
			Generation string `json:"expectedGeneration"`
			Instant    string `json:"instant"`
		}
		fields := []string{"expectedInstanceID", "expectedGeneration"}
		if req.Operation == "clock.set" {
			fields = append(fields, "instant")
		}
		if err := decodeObject(req.Data, &data, fields...); err != nil || !uuidPattern.MatchString(data.Instance) || !generationPattern.MatchString(data.Generation) {
			return nil, "InvalidRequest"
		}
		c.clockMu.Lock()
		defer c.clockMu.Unlock()
		if data.Instance != c.instance || data.Generation != strconv.FormatUint(c.generation, 10) {
			return nil, "ClockConflict"
		}
		if ctx.Err() != nil {
			return nil, "WaitExpired"
		}
		if req.Operation == "clock.set" {
			t, err := parseInstant(data.Instant)
			if err != nil {
				return nil, "InvalidRequest"
			}
			if t.Before(c.instant) {
				return nil, "ClockBackward"
			}
			if c.generation >= maxGeneration {
				return nil, "ClockConflict"
			}
			c.instant = t
			c.generation++
			return c.clockLocked(), ""
		}
		view := c.clockLocked()
		raw, err := json.Marshal(view)
		if err != nil {
			return nil, "NotReady"
		}
		b, err := canonical(raw)
		if err != nil {
			return nil, "NotReady"
		}
		sum := sha256.Sum256(b)
		return struct {
			Clock  clockView `json:"clock"`
			Digest string    `json:"checkpointSHA256"`
		}{view, hex.EncodeToString(sum[:])}, ""
	default:
		return nil, "UnsupportedOperation"
	}
}
