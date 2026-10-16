package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const secretCanary = "canary-secret-value-7f3a91"

var serviceKeyNames = []string{"SERVING_AUTH_KEY", "AUTH_RUNTIME_KEY", "GATEWAY_SERVING_KEY", "SERVING_WAREHOUSE_KEY"}

func writeSecretFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func envMap(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func TestSecretEnvReadsPlainVariable(t *testing.T) {
	got, err := secretEnv(envMap(map[string]string{"K": " plain value "}), "K")
	if err != nil || got != " plain value " {
		t.Fatalf("got %q err=%v", got, err)
	}
	got, err = secretEnv(envMap(nil), "K")
	if err != nil || got != "" {
		t.Fatalf("unset: got %q err=%v", got, err)
	}
	got, err = secretEnv(envMap(map[string]string{"K": "", "K_FILE": ""}), "K")
	if err != nil || got != "" {
		t.Fatalf("empty: got %q err=%v", got, err)
	}
}

func TestSecretEnvReadsFileAndTrimsOnlyTrailingNewline(t *testing.T) {
	for name, tc := range map[string]struct{ content, want string }{
		"no newline":   {secretCanary, secretCanary},
		"lf":           {secretCanary + "\n", secretCanary},
		"crlf":         {secretCanary + "\r\n", secretCanary},
		"inner spaces": {" a b \n", " a b "},
		"inner lf":     {"a\nb\n", "a\nb"},
		"two lf":       {secretCanary + "\n\n", secretCanary + "\n"},
		"crlf then lf": {secretCanary + "\r\n\n", secretCanary + "\r\n"},
	} {
		t.Run(name, func(t *testing.T) {
			path := writeSecretFile(t, tc.content)
			got, err := secretEnv(envMap(map[string]string{"K_FILE": path}), "K")
			if err != nil || got != tc.want {
				t.Fatalf("got %q err=%v", got, err)
			}
		})
	}
}

func TestSecretEnvRejectsBadSources(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")
	tooLarge := writeSecretFile(t, strings.Repeat("x", secretFileMaxBytes+1))
	cases := map[string]map[string]string{
		"both set":     {"K": secretCanary, "K_FILE": writeSecretFile(t, secretCanary)},
		"empty file":   {"K_FILE": writeSecretFile(t, "")},
		"only newline": {"K_FILE": writeSecretFile(t, "\r\n")},
		"missing file": {"K_FILE": missing},
		"directory":    {"K_FILE": t.TempDir()},
		"too large":    {"K_FILE": tooLarge},
	}
	for name, values := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := secretEnv(envMap(values), "K")
			if err == nil || got != "" {
				t.Fatalf("got %q err=%v", got, err)
			}
			if strings.Contains(err.Error(), secretCanary) {
				t.Fatalf("error leaks the value: %v", err)
			}
		})
	}
}

func TestSecretFileAtLimitIsAccepted(t *testing.T) {
	path := writeSecretFile(t, strings.Repeat("x", secretFileMaxBytes))
	got, err := secretEnv(envMap(map[string]string{"K_FILE": path}), "K")
	if err != nil || len(got) != secretFileMaxBytes {
		t.Fatalf("len=%d err=%v", len(got), err)
	}
}

func TestHandlerReadsServiceKeysFromFiles(t *testing.T) {
	for _, name := range serviceKeyNames {
		t.Run(name, func(t *testing.T) {
			s4Configure(t, "http://127.0.0.1:1", "http://127.0.0.1:2")
			t.Setenv(name, "")
			t.Setenv(name+"_FILE", writeSecretFile(t, "file-"+strings.ToLower(name)+"\n"))
			app, err := handler()
			if err != nil {
				t.Fatalf("file key rejected: %v", err)
			}
			app.CloseIdleConnections()
		})
	}
}

func TestHandlerRejectsBadServiceKeyFilesWithoutLeaking(t *testing.T) {
	for _, name := range serviceKeyNames {
		for scenario, setup := range map[string]func(t *testing.T){
			"both": func(t *testing.T) {
				t.Setenv(name, secretCanary)
				t.Setenv(name+"_FILE", writeSecretFile(t, secretCanary))
			},
			"empty file": func(t *testing.T) {
				t.Setenv(name, "")
				t.Setenv(name+"_FILE", writeSecretFile(t, "\n"))
			},
			"missing file": func(t *testing.T) {
				t.Setenv(name, "")
				t.Setenv(name+"_FILE", filepath.Join(t.TempDir(), "absent"))
			},
		} {
			t.Run(name+"/"+scenario, func(t *testing.T) {
				s4Configure(t, "http://127.0.0.1:1", "http://127.0.0.1:2")
				setup(t)
				var logs bytes.Buffer
				logger := slog.New(slog.NewJSONHandler(&logs, nil))
				app, err := handlerWithLogger(logger)
				if app != nil {
					app.CloseIdleConnections()
				}
				if err == nil {
					t.Fatal("bad key source started the process")
				}
				if strings.Contains(err.Error(), secretCanary) || strings.Contains(startupErrorText(err), secretCanary) {
					t.Fatalf("error leaks the value: %v", err)
				}
				var fatalLogs bytes.Buffer
				slog.New(slog.NewJSONHandler(&fatalLogs, nil)).Error("process.start_failed", slog.String("error", startupErrorText(err)))
				if strings.Contains(logs.String(), secretCanary) || strings.Contains(fatalLogs.String(), secretCanary) {
					t.Fatal("logs leak the value")
				}
			})
		}
	}
}

func TestHandlerLeavesLegacyEnvironmentKeysWorking(t *testing.T) {
	s4Configure(t, "http://127.0.0.1:1", "http://127.0.0.1:2")
	app, err := handler()
	if err != nil {
		t.Fatal(err)
	}
	app.CloseIdleConnections()
}
