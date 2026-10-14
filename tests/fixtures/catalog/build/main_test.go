package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRealToolchainASTMatchesArchive(t *testing.T) {
	source, err := os.ReadFile(filepath.Join(toolchainRoot(t), "src/time/tzdata/zzipdata.go"))
	if err != nil {
		t.Fatal(err)
	}
	archive, err := embeddedArchive(source)
	if err != nil {
		t.Fatal(err)
	}
	packaged, err := os.ReadFile(filepath.Join(toolchainRoot(t), "lib/time/zoneinfo.zip"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(archive, packaged) {
		t.Fatal("actual Go AST and archive differ")
	}
}
func TestEmbeddedArchiveRejectsGeneratedLayoutDrift(t *testing.T) {
	for _, source := range []string{`package tzdata`, `package other;const zipdata="a"`, `package tzdata;var zipdata="a"`, `package tzdata;const zipdata=""`, `package tzdata;const zipdata="a"+"b"`, `package tzdata;const zipdata="a";const zipdata="b"`, `package tzdata;const zipdata,other="a","b"`} {
		if _, err := embeddedArchive([]byte(source)); err == nil {
			t.Fatalf("unexpected layout accepted: %s", source)
		}
	}
}

func toolchainRoot(t *testing.T) string {
	t.Helper()
	raw, err := exec.Command("go", "env", "GOROOT").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(raw))
}
