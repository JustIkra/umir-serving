package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
)

const configPath = "/run/umir-fixtures/config/config.json"
const socketPath = "/run/umir-fixtures/control.sock"
const secretRoot = "/run/umir-fixtures/secrets/"

type fileRef struct {
	Target string `json:"target"`
	Path   string `json:"path"`
}
type serviceURLs struct {
	Auth      string `json:"auth"`
	Gateway   string `json:"gateway"`
	Warehouse string `json:"warehouse"`
	Serving   string `json:"serving"`
	Feedback  string `json:"feedback"`
}
type deliveryConfig struct {
	SMTPHost string  `json:"smtpHost"`
	SMTPPort int     `json:"smtpPort"`
	SMTPTLS  string  `json:"smtpTLS"`
	SMTPCA   fileRef `json:"smtpCAFile"`
	Proxy    string  `json:"notificationProxy"`
	ProxyCA  fileRef `json:"notificationCAFile"`
}
type ownerConfig struct {
	Version               int                `json:"version"`
	Protocol              string             `json:"protocol"`
	Project               string             `json:"project"`
	Owner                 string             `json:"owner"`
	FixtureID             string             `json:"fixtureID"`
	InitialInstant        string             `json:"initialInstant"`
	ClockGeneration       string             `json:"clockGeneration"`
	ControlCredentialFile fileRef            `json:"controlCredentialFile"`
	CredentialFiles       map[string]fileRef `json:"credentialFiles"`
	ControlSocket         string             `json:"controlSocket"`
	PrivatePort           int                `json:"privatePort"`
	PublicPort            int                `json:"publicPort"`
	Origin                string             `json:"origin"`
	ServiceURLs           serviceURLs        `json:"serviceURLs"`
	Delivery              deliveryConfig     `json:"delivery"`
	StateFile             string             `json:"stateFile"`
	Bootstrap             json.RawMessage    `json:"bootstrap"`
}

var secretName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func parseConfig(raw []byte) (ownerConfig, string, error) {
	var cfg ownerConfig
	if len(raw) > maxRequest {
		return cfg, "", errors.New("config exceeds bound")
	}
	canon, err := canonical(raw)
	if err != nil {
		return cfg, "", errors.New("invalid config JSON")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return cfg, "", errors.New("invalid config")
	}
	required := []string{"version", "protocol", "project", "owner", "fixtureID", "initialInstant", "clockGeneration", "controlCredentialFile", "credentialFiles", "controlSocket", "privatePort", "publicPort", "origin", "serviceURLs", "delivery", "stateFile", "bootstrap"}
	if len(object) != len(required) {
		return cfg, "", errors.New("invalid config fields")
	}
	for _, k := range required {
		if v, ok := object[k]; !ok || k != "bootstrap" && bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return cfg, "", errors.New("missing config field")
		}
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&cfg); err != nil {
		return cfg, "", errors.New("invalid config fields")
	}
	if err := decodeObject(object["serviceURLs"], &cfg.ServiceURLs, "auth", "gateway", "warehouse", "serving", "feedback"); err != nil {
		return cfg, "", errors.New("invalid service URL fields")
	}
	if err := decodeObject(object["delivery"], &cfg.Delivery, "smtpHost", "smtpPort", "smtpTLS", "smtpCAFile", "notificationProxy", "notificationCAFile"); err != nil {
		return cfg, "", errors.New("invalid delivery fields")
	}
	var credentialObjects map[string]json.RawMessage
	if err := json.Unmarshal(object["credentialFiles"], &credentialObjects); err != nil {
		return cfg, "", errors.New("invalid credential fields")
	}
	refs := []json.RawMessage{object["controlCredentialFile"]}
	for _, v := range credentialObjects {
		refs = append(refs, v)
	}
	var deliveryObjects map[string]json.RawMessage
	if err := json.Unmarshal(object["delivery"], &deliveryObjects); err != nil {
		return cfg, "", errors.New("invalid delivery fields")
	}
	refs = append(refs, deliveryObjects["smtpCAFile"], deliveryObjects["notificationCAFile"])
	for _, raw := range refs {
		var ref fileRef
		if err := decodeObject(raw, &ref, "target", "path"); err != nil {
			return cfg, "", errors.New("invalid reference fields")
		}
	}
	if cfg.Version != 1 || cfg.Protocol != protocolVersion || cfg.Owner != "serving" || !projectPattern.MatchString(cfg.Project) || !uuidPattern.MatchString(cfg.FixtureID) || !generationPattern.MatchString(cfg.ClockGeneration) || cfg.ControlSocket != socketPath || cfg.PrivatePort != 8080 || cfg.PublicPort != 0 || cfg.StateFile != "" || !bytes.Equal(bytes.TrimSpace(cfg.Bootstrap), []byte("null")) {
		return cfg, "", errors.New("invalid Serving config")
	}
	if _, err := parseInstant(cfg.InitialInstant); err != nil {
		return cfg, "", err
	}
	if !validReference(cfg.ControlCredentialFile, "fixture-serving") {
		return cfg, "", errors.New("invalid control credential binding")
	}
	names := []string{"serving-auth", "auth-serving", "gateway-serving", "serving-warehouse"}
	if len(cfg.CredentialFiles) != len(names) {
		return cfg, "", errors.New("missing real service credential binding")
	}
	for _, name := range names {
		if !validReference(cfg.CredentialFiles[name], name) {
			return cfg, "", errors.New("invalid service credential binding")
		}
	}
	expected := map[string]string{"auth": cfg.ServiceURLs.Auth, "gateway": cfg.ServiceURLs.Gateway, "warehouse": cfg.ServiceURLs.Warehouse, "serving": cfg.ServiceURLs.Serving, "feedback": cfg.ServiceURLs.Feedback}
	for name, raw := range expected {
		u, err := url.Parse(raw)
		if err != nil || len(raw) > 128 || u.Scheme != "http" || u.Hostname() != name || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Port() != "8080" && (name != "gateway" || u.Port() != "8082") {
			return cfg, "", errors.New("invalid private service URL")
		}
	}
	origin, err := url.Parse(cfg.Origin)
	if err != nil || len(cfg.Origin) > 256 || origin.Scheme != "http" || origin.Hostname() != "127.0.0.1" || origin.Port() == "" || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.ForceQuery || origin.Fragment != "" {
		return cfg, "", errors.New("invalid loopback origin")
	}
	if cfg.Delivery.SMTPHost != "smtp" || cfg.Delivery.SMTPPort != 2525 || cfg.Delivery.SMTPTLS != "starttls" || cfg.Delivery.Proxy != "http://notification:3128" || !validCAReference(cfg.Delivery.SMTPCA) || !validCAReference(cfg.Delivery.ProxyCA) {
		return cfg, "", errors.New("invalid synthetic delivery metadata")
	}
	sum := sha256.Sum256(canon)
	return cfg, hex.EncodeToString(sum[:]), nil
}
func validReference(ref fileRef, name string) bool {
	return ref.Target == "serving" && secretName.MatchString(name) && name != "." && name != ".." && ref.Path == secretRoot+name && filepath.Clean(ref.Path) == ref.Path
}
func validCAReference(ref fileRef) bool {
	switch ref.Target {
	case "auth", "feedback", "smtp", "notification":
		return ref.Path == secretRoot+"synthetic-ca.pem"
	}
	return false
}
func loadConfig() (ownerConfig, string, error) {
	raw, err := readPrivate(configPath, maxRequest)
	if err != nil {
		return ownerConfig{}, "", err
	}
	return parseConfig(raw)
}
func readKeys(cfg ownerConfig) (serviceKeys, string, error) {
	values := make(map[string]string)
	seen := make(map[string]bool)
	refs := make(map[string]fileRef)
	for k, v := range cfg.CredentialFiles {
		refs[k] = v
	}
	refs["fixture-serving"] = cfg.ControlCredentialFile
	for k, ref := range refs {
		b, err := readPrivate(ref.Path, 4096)
		if err != nil {
			return serviceKeys{}, "", err
		}
		v := string(b)
		if len(v) < 16 || len(v) > 4096 || strings.ContainsAny(v, " \t\r\n") || seen[v] {
			return serviceKeys{}, "", errors.New("invalid or reused fixture credential")
		}
		seen[v] = true
		values[k] = v
	}
	return serviceKeys{Discovery: values["serving-auth"], Lifecycle: values["auth-serving"], Gateway: values["gateway-serving"], Warehouse: values["serving-warehouse"]}, values["fixture-serving"], nil
}
