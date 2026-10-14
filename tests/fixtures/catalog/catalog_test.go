package catalog

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func catalogInputs(t *testing.T) (CatalogReport, []byte, []byte) {
	t.Helper()
	archive, err := os.ReadFile(filepath.Join(toolchainRoot(t), "lib/time/zoneinfo.zip"))
	if err != nil {
		t.Fatal(err)
	}
	report := CatalogReport{BinarySHA256: strings.Repeat("a", 64)}
	p := Provenance{Owner: "serving", Version: Version, ArchiveSHA256: digest(archive), ArchiveBytes: len(archive), EmbeddedArchiveSHA256: digest(archive), BuildToolchain: "go1.26.9", ToolchainImageID: ToolchainImageID, ToolchainSourceSHA256: strings.Repeat("b", 64), BinarySHA256: report.BinarySHA256, SourceKind: "go-time-tzdata"}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return report, raw, archive
}
func TestObservedCatalogMeasuresPackagedArchive(t *testing.T) {
	report, raw, archive := catalogInputs(t)
	got, err := observe(context.Background(), report, raw, archive)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Catalog.Ready || got.Catalog.ArchiveBytes != len(archive) || got.Catalog.ArchiveSHA256 != digest(archive) || got.Catalog.ProvenanceSHA256 != digest(raw) {
		t.Fatal("catalog did not measure archive")
	}
	for _, tc := range []struct{ from, to string }{{`"owner":"serving"`, `"owner":"gateway"`}, {Version, "go1.26.9"}, {report.BinarySHA256, strings.Repeat("c", 64)}, {ToolchainImageID, "sha256:" + strings.Repeat("c", 64)}, {`"sourceKind":"go-time-tzdata"`, `"sourceKind":"warehouse-embedded"`}, {`"archiveBytes":`, `"unknown":1,"archiveBytes":`}} {
		if _, err := observe(context.Background(), report, []byte(strings.Replace(string(raw), tc.from, tc.to, 1)), archive); err == nil {
			t.Fatalf("bad provenance accepted: %s", tc.from)
		}
	}
	for _, bad := range [][]byte{nil, []byte("not zip"), append(append([]byte{}, archive...), 0)} {
		if _, err := observe(context.Background(), report, raw, bad); err == nil {
			t.Fatal("different archive accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := observe(ctx, report, raw, archive); err == nil {
		t.Fatal("cancelled observation accepted")
	}
}
func TestPackagedReaderRejectsWritableSymlinkAndOversize(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "catalog")
	if err := os.WriteFile(p, []byte("abc"), 0444); err != nil {
		t.Fatal(err)
	}
	if _, err := readPackaged(p, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := readPackaged(p, 2); err == nil {
		t.Fatal("oversize accepted")
	}
	if err := os.Symlink(p, filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	if _, err := readPackaged(filepath.Join(dir, "alias"), 3); err == nil {
		t.Fatal("symlink accepted")
	}
	if err := os.Chmod(p, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readPackaged(p, 3); err == nil {
		t.Fatal("writable provenance accepted")
	}
}
func TestObserveRejectsUnqualifiedEnvironment(t *testing.T) {
	t.Setenv("ZONEINFO", "/unqualified/archive")
	if _, err := Observe(context.Background(), nil); err == nil {
		t.Fatal("unqualified environment accepted")
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
