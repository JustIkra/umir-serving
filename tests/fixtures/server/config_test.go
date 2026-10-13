package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func configBytes(t *testing.T) []byte {
	t.Helper()
	cfg := ownerConfig{Version: 1, Protocol: protocolVersion, Project: testProject, Owner: "serving", FixtureID: testFixture, InitialInstant: "2026-10-13T09:00:00+03:00", ClockGeneration: "1", ControlCredentialFile: fileRef{Target: "serving", Path: secretRoot + "fixture-serving"}, CredentialFiles: map[string]fileRef{}, ControlSocket: socketPath, PrivatePort: 8080, PublicPort: 0, Origin: "http://127.0.0.1:12345", ServiceURLs: serviceURLs{Auth: "http://auth:8080", Gateway: "http://gateway:8082", Warehouse: "http://warehouse:8080", Serving: "http://serving:8080", Feedback: "http://feedback:8080"}, Delivery: deliveryConfig{SMTPHost: "smtp", SMTPPort: 2525, SMTPTLS: "starttls", SMTPCA: fileRef{Target: "auth", Path: secretRoot + "synthetic-ca.pem"}, Proxy: "http://notification:3128", ProxyCA: fileRef{Target: "feedback", Path: secretRoot + "synthetic-ca.pem"}}, StateFile: "", Bootstrap: json.RawMessage("null")}
	for _, name := range []string{"serving-auth", "auth-serving", "gateway-serving", "serving-warehouse"} {
		cfg.CredentialFiles[name] = fileRef{Target: "serving", Path: secretRoot + name}
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func TestConfigRejectsSchemaAndCredentialBoundaryViolations(t *testing.T) {
	raw := string(configBytes(t))
	if _, _, err := parseConfig([]byte(raw)); err != nil {
		t.Fatal("valid owner config rejected", err)
	}
	for _, tc := range []struct{ from, to string }{{`"publicPort":0`, `"publicPort":null`}, {`"stateFile":""`, `"stateFile":null`}, {`"auth":"http://auth:8080"`, `"Auth":"http://auth:8080"`}, {`"publicPort":0`, `"publicPort":1`}, {`"target":"serving"`, `"target":"auth"`}, {`"clockGeneration":"1"`, `"clockGeneration":"0"`}, {`"initialInstant":"2026-10-13T09:00:00+03:00"`, `"initialInstant":"2026-10-13T09:00:00.1234567890+03:00"`}, {`"warehouse":"http://warehouse:8080"`, `"warehouse":"http://external.invalid:8080"`}, {`"path":"/run/umir-fixtures/secrets/fixture-serving"`, `"path":"/run/umir-fixtures/secrets/../fixture-serving"`}, {`"bootstrap":null`, `"bootstrap":{}`}, {`"version":1`, `"version":1,"version":1`}} {
		changed := strings.Replace(raw, tc.from, tc.to, 1)
		if changed == raw {
			t.Fatalf("probe not applied %s", tc.from)
		}
		if _, _, err := parseConfig([]byte(changed)); err == nil {
			t.Fatalf("invalid configuration accepted: %s", tc.from)
		}
	}
}

func TestInstantRejectsNonRFCOffsets(t *testing.T) {
	for _, s := range []string{"2026-10-13T09:00:00+24:00", "2026-10-13T09:00:00+00:60"} {
		if _, err := parseInstant(s); err == nil {
			t.Fatalf("invalid offset accepted: %s", s)
		}
	}
}
