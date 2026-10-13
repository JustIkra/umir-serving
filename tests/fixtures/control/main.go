package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/JustIkra/umir-serving/tests/fixtures/catalog"
)

const socketPath = "/run/umir-fixtures/control.sock"
const configPath = "/run/umir-fixtures/config/config.json"
const credentialPath = "/run/umir-fixtures/secrets/fixture-serving"

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var generationPattern = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)
var projectPattern = regexp.MustCompile(`^umir-backend-[0-9a-f]{24}$`)
var instantPattern = regexp.MustCompile(`^20[0-9]{2}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,9})?(Z|[+-](0[0-9]|1[0-9]|2[0-3]):[0-5][0-9])$`)

func readPrivate(path string, limit int) (result []byte, err error) {
	for parent := filepath.Dir(path); parent != filepath.Dir(parent); parent = filepath.Dir(parent) {
		info, checkErr := os.Lstat(parent)
		if checkErr != nil || !info.IsDir() {
			return nil, errors.New("invalid private directory")
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("invalid private directory")
		}
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("private file unavailable")
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	info, err := f.Stat()
	if err != nil {
		return nil, errors.New("private file unavailable")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || int(stat.Uid) != os.Getuid() || int(stat.Gid) != os.Getgid() || info.Size() < 1 || info.Size() > int64(limit) {
		return nil, errors.New("invalid private file")
	}
	result, err = io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil || len(result) > limit {
		return nil, errors.New("private file exceeds bound")
	}
	return result, nil
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
				return errors.New("duplicate field")
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
		return errors.New("invalid delimiter")
	}
	_, err = d.Token()
	return err
}
func object(raw []byte) (map[string]json.RawMessage, error) {
	if !utf8.Valid(raw) {
		return nil, errors.New("invalid JSON")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := scanJSON(d, 0); err != nil {
		return nil, errors.New("invalid JSON")
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, errors.New("trailing JSON")
	}
	var v map[string]json.RawMessage
	if err := json.Unmarshal(raw, &v); err != nil || v == nil {
		return nil, errors.New("object required")
	}
	return v, nil
}
func exact(v map[string]json.RawMessage, fields ...string) bool {
	if len(v) != len(fields) {
		return false
	}
	for _, field := range fields {
		if b, ok := v[field]; !ok || bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
			return false
		}
	}
	return true
}
func textField(v map[string]json.RawMessage, key string) string {
	var s string
	if err := json.Unmarshal(v[key], &s); err != nil {
		return ""
	}
	return s
}
func validBoolean(raw []byte) bool {
	return bytes.Equal(raw, []byte("true")) || bytes.Equal(raw, []byte("false"))
}
func validateClock(raw []byte) bool {
	v, err := object(raw)
	if err != nil || !exact(v, "owner", "instanceID", "source", "fixtureID", "generation", "instant", "injectedConsumers", "healthy") || textField(v, "owner") != "serving" || textField(v, "source") != "owner-injected" || !uuidPattern.MatchString(textField(v, "instanceID")) || !uuidPattern.MatchString(textField(v, "fixtureID")) || !generationPattern.MatchString(textField(v, "generation")) || !validBoolean(v["healthy"]) {
		return false
	}
	instant := textField(v, "instant")
	if !instantPattern.MatchString(instant) {
		return false
	}
	t, err := time.Parse(time.RFC3339Nano, instant)
	if err != nil || t.Year() < 2000 || t.Year() > 2099 {
		return false
	}
	var consumers []string
	if json.Unmarshal(v["injectedConsumers"], &consumers) != nil || len(consumers) != 5 {
		return false
	}
	allowed := map[string]bool{"bootstrap.Registry.Now": true, "bootstrap.Options.Now": true, "httpapi.ScopedOptions.Now": true, "httpapi.JWTOptions.Now": true, "authhttp.Client.now": true}
	for _, c := range consumers {
		if !allowed[c] {
			return false
		}
		delete(allowed, c)
	}
	return true
}
func validateResponse(raw []byte, request map[string]json.RawMessage, status int) bool {
	v, err := object(raw)
	if err != nil || !bytes.Equal(v["version"], []byte("1")) || textField(v, "project") != textField(request, "project") || textField(v, "requestID") != textField(request, "requestID") || textField(v, "operation") != textField(request, "operation") {
		return false
	}
	if e, ok := v["error"]; ok {
		if !exact(v, "version", "project", "requestID", "operation", "error") {
			return false
		}
		f, err := object(e)
		if err != nil || !exact(f, "code", "effect", "retryable") || !validBoolean(f["retryable"]) {
			return false
		}
		effect := textField(f, "effect")
		if effect != "none" && effect != "applied" && effect != "unknown" {
			return false
		}
		codes := map[string]int{"InvalidRequest": 400, "Unauthenticated": 401, "Forbidden": 403, "WrongProject": 403, "OwnershipMismatch": 403, "UnsupportedOperation": 404, "NotFound": 404, "ClockConflict": 409, "ClockBackward": 409, "BootstrapConflict": 409, "CorrelationConflict": 409, "FaultConflict": 409, "CapacityExceeded": 413, "WaitExpired": 408, "NotReady": 503, "ClockUnavailable": 503, "StoreUnavailable": 503, "OutcomeUnknown": 503, "PrivacyViolation": 422}
		if bytes.Equal(f["retryable"], []byte("true")) {
			code := textField(f, "code")
			operation := textField(request, "operation")
			if effect != "none" || code != "NotReady" && code != "WaitExpired" || operation != "fixture.ready" && operation != "clock.read" && operation != "clock.checkpoint" {
				return false
			}
		}
		return status == codes[textField(f, "code")] && status != 0
	}
	if status != 200 || !exact(v, "version", "project", "requestID", "operation", "data") {
		return false
	}
	switch textField(v, "operation") {
	case "clock.read", "clock.set":
		return validateClock(v["data"])
	case "clock.checkpoint":
		d, err := object(v["data"])
		return err == nil && exact(d, "clock", "checkpointSHA256") && validateClock(d["clock"]) && digestPattern.MatchString(textField(d, "checkpointSHA256"))
	case "fixture.ready":
		d, err := object(v["data"])
		if err != nil || !exact(d, "target", "instanceID", "protocol", "binarySHA256", "configSHA256", "ready", "capabilities") || textField(d, "target") != "serving" || textField(d, "protocol") != "umir-operational-fixtures/1.1.0" || !uuidPattern.MatchString(textField(d, "instanceID")) || !digestPattern.MatchString(textField(d, "binarySHA256")) || !digestPattern.MatchString(textField(d, "configSHA256")) || !validBoolean(d["ready"]) {
			return false
		}
		var capabilities []string
		if json.Unmarshal(d["capabilities"], &capabilities) != nil || len(capabilities) != 4 {
			return false
		}
		allowed := map[string]bool{"fixture.ready": true, "clock.read": true, "clock.set": true, "clock.checkpoint": true}
		for _, capability := range capabilities {
			if !allowed[capability] {
				return false
			}
			delete(allowed, capability)
		}
		return true
	}
	return false
}
func requestControl(ctx context.Context, socket, credential string, raw []byte) ([]byte, error) {
	request, err := object(raw)
	if err != nil || len(raw) > 16384 || !exact(request, "version", "project", "requestID", "operation", "data") || !bytes.Equal(request["version"], []byte("1")) || !projectPattern.MatchString(textField(request, "project")) || !uuidPattern.MatchString(textField(request, "requestID")) {
		return nil, errors.New("invalid control input")
	}
	if _, err := object(request["data"]); err != nil {
		return nil, errors.New("invalid control input")
	}
	operation := textField(request, "operation")
	if len(operation) == 0 || len(operation) > 128 || strings.ContainsAny(operation, " \t\r\n") {
		return nil, errors.New("invalid control operation")
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://fixture/fixture/v1/control", bytes.NewReader(raw))
	if err != nil {
		return nil, errors.New("invalid control request")
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+credential)
	res, err := client.Do(r)
	if err != nil {
		return nil, errors.New("control unavailable")
	}
	body, readErr := io.ReadAll(io.LimitReader(res.Body, 262145))
	if err := errors.Join(readErr, res.Body.Close()); err != nil || len(body) > 262144 || !validateResponse(body, request, res.StatusCode) {
		return nil, errors.New("invalid or unavailable control response")
	}
	return body, nil
}
func run(ctx context.Context, args []string, in io.Reader, out io.Writer) error {
	profile := len(args) == 3 && args[0] == "profile" && args[1] == "--request" && args[2] == catalog.RequestPath
	if !profile && (len(args) != 1 || args[0] != "request") {
		return errors.New("expected request or fixed profile request")
	}
	var raw []byte
	var err error
	if profile {
		raw, err = readPrivate(catalog.RequestPath, 65536)
	} else {
		raw, err = io.ReadAll(io.LimitReader(in, 16385))
	}
	if err != nil || !profile && len(raw) > 16384 {
		return errors.New("input unavailable")
	}
	cfgRaw, err := readPrivate(configPath, 16384)
	if err != nil {
		return err
	}
	cfg, err := object(cfgRaw)
	if err != nil {
		return err
	}
	if textField(cfg, "owner") != "serving" || textField(cfg, "protocol") != "umir-operational-fixtures/1.1.0" || textField(cfg, "controlSocket") != socketPath || !projectPattern.MatchString(textField(cfg, "project")) {
		return errors.New("invalid Serving config")
	}
	ref, err := object(cfg["controlCredentialFile"])
	if err != nil || !exact(ref, "target", "path") || textField(ref, "target") != "serving" || textField(ref, "path") != credentialPath {
		return errors.New("invalid credential reference")
	}
	req, err := object(raw)
	if err != nil || textField(req, "project") != textField(cfg, "project") {
		return errors.New("project mismatch")
	}
	info, err := os.Lstat(socketPath)
	if err != nil {
		return errors.New("socket unavailable")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0600 || int(stat.Uid) != os.Getuid() || int(stat.Gid) != os.Getgid() {
		return errors.New("socket ownership mismatch")
	}
	credential, err := readPrivate(credentialPath, 4096)
	if err != nil {
		return err
	}
	var response []byte
	if profile {
		response, err = requestProfile(ctx, socketPath, string(credential), raw, cfgRaw)
	} else {
		response, err = requestControl(ctx, socketPath, string(credential), raw)
	}
	if err != nil {
		return err
	}
	_, err = out.Write(append(response, '\n'))
	return err
}

func validCatalogRequest(raw []byte) (map[string]json.RawMessage, error) {
	req, err := object(raw)
	if err != nil || len(raw) > 65536 || !exact(req, "amendment", "project", "requestID", "expectedInstanceID", "expectedGeneration", "action") || textField(req, "amendment") != catalog.Amendment || textField(req, "action") != "catalog-report" || !projectPattern.MatchString(textField(req, "project")) || !uuidPattern.MatchString(textField(req, "requestID")) || !uuidPattern.MatchString(textField(req, "expectedInstanceID")) || !generationPattern.MatchString(textField(req, "expectedGeneration")) {
		return nil, errors.New("invalid catalog request")
	}
	return req, nil
}
func validCatalogResponse(raw []byte, req map[string]json.RawMessage, status int, configSHA string) bool {
	v, err := object(raw)
	if err != nil || textField(v, "amendment") != catalog.Amendment || textField(v, "requestID") != textField(req, "requestID") {
		return false
	}
	if _, ok := v["error"]; ok {
		codes := map[string]int{"InvalidRequest": 400, "Unauthorized": 401, "RequestConflict": 409, "ClockConflict": 409, "OwnershipMismatch": 403, "UnsupportedAction": 400, "ProfileAdmissionFailed": 503, "ProfileExpired": 503, "ProfileCapacityExceeded": 413}
		return exact(v, "amendment", "requestID", "error") && codes[textField(v, "error")] != 0 && codes[textField(v, "error")] == status
	}
	if status != 200 || !exact(v, "amendment", "requestID", "project", "instanceID", "clock", "binarySHA256", "configSHA256", "catalog") || textField(v, "project") != textField(req, "project") || textField(v, "instanceID") != textField(req, "expectedInstanceID") || !digestPattern.MatchString(textField(v, "binarySHA256")) || textField(v, "configSHA256") != configSHA {
		return false
	}
	clock, err := object(v["clock"])
	if err != nil || !exact(clock, "instant", "generation", "instanceID", "source", "healthy") || textField(clock, "instanceID") != textField(req, "expectedInstanceID") || textField(clock, "generation") != textField(req, "expectedGeneration") || textField(clock, "source") != "owner" || !bytes.Equal(clock["healthy"], []byte("true")) {
		return false
	}
	instant := textField(clock, "instant")
	t, err := time.Parse(time.RFC3339Nano, instant)
	if err != nil || t.Year() < 2000 || t.Year() > 2099 || !strings.HasSuffix(instant, "+03:00") || !instantPattern.MatchString(instant) {
		return false
	}
	fact, err := object(v["catalog"])
	if err != nil || !exact(fact, "owner", "version", "archiveSHA256", "archiveBytes", "buildToolchain", "ready", "provenanceSHA256") || textField(fact, "owner") != "serving" || textField(fact, "version") != catalog.Version || textField(fact, "buildToolchain") != "go1.26.9" || !digestPattern.MatchString(textField(fact, "archiveSHA256")) || !digestPattern.MatchString(textField(fact, "provenanceSHA256")) || !bytes.Equal(fact["ready"], []byte("true")) {
		return false
	}
	var n int
	return json.Unmarshal(fact["archiveBytes"], &n) == nil && n >= 1 && n <= 16777216
}
func requestProfile(ctx context.Context, socket, credential string, raw, config []byte) ([]byte, error) {
	req, err := validCatalogRequest(raw)
	if err != nil {
		return nil, err
	}
	var decoded any
	if json.Unmarshal(config, &decoded) != nil {
		return nil, errors.New("invalid catalog config")
	}
	canonical, err := json.Marshal(decoded)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(canonical)
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://fixture"+catalog.Path, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+credential)
	res, err := client.Do(r)
	if err != nil {
		return nil, errors.New("catalog unavailable")
	}
	body, readErr := io.ReadAll(io.LimitReader(res.Body, 262145))
	if err := errors.Join(readErr, res.Body.Close()); err != nil || len(body) > 262144 || !validCatalogResponse(body, req, res.StatusCode, hex.EncodeToString(sum[:])) {
		return nil, errors.New("invalid catalog response")
	}
	return body, nil
}
func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdin, os.Stdout); err != nil {
		if _, writeErr := fmt.Fprintln(os.Stderr, "Serving fixture control failed closed"); writeErr != nil {
			os.Exit(1)
		}
		os.Exit(1)
	}
}
