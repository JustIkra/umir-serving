package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"testing"
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
