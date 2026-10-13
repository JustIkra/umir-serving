package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestProtocolWireEvidence(t *testing.T) {
	c := controlForTest(t, func(context.Context) bool { return true })
	records := []struct {
		Schema string          `json:"schema"`
		Value  json.RawMessage `json:"value"`
	}{}
	for i, tc := range []struct{ op, data, schema string }{{"fixture.ready", "{}", "fixture_ready_response"}, {"clock.read", "{}", "clock_read_response"}, {"clock.set", `{"expectedInstanceID":"33333333-3333-4333-8333-333333333333","expectedGeneration":"1","instant":"2026-10-14T09:00:00+03:00"}`, "clock_set_response"}, {"clock.checkpoint", `{"expectedInstanceID":"33333333-3333-4333-8333-333333333333","expectedGeneration":"2"}`, "clock_checkpoint_response"}, {"scheduler.tick", "{}", "Error"}} {
		w := send(t, c, input(i+1, tc.op, tc.data), "test-control-credential", context.Background())
		if tc.schema == "Error" && w.Code != 404 || tc.schema != "Error" && w.Code != 200 {
			t.Fatal("wire operation failed")
		}
		records = append(records, struct {
			Schema string          `json:"schema"`
			Value  json.RawMessage `json:"value"`
		}{tc.schema, append(json.RawMessage(nil), w.Body.Bytes()...)})
	}
	records = append(records, struct {
		Schema string          `json:"schema"`
		Value  json.RawMessage `json:"value"`
	}{"OwnerConfig", configBytes(t)})
	if directory := os.Getenv("SI_WIRE_EVIDENCE_DIR"); directory != "" {
		raw, err := json.MarshalIndent(records, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "unit-wire.json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
}
