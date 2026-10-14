package main

import (
	"bytes"
	"crypto/sha256"
	"debug/buildinfo"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"

	"github.com/JustIkra/umir-serving/tests/fixtures/catalog"
)

func digest(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }
func embeddedArchive(source []byte) ([]byte, error) {
	f, err := parser.ParseFile(token.NewFileSet(), "zzipdata.go", source, 0)
	if err != nil || f.Name.Name != "tzdata" {
		return nil, errors.New("unexpected tzdata source")
	}
	var archive []byte
	for _, decl := range f.Decls {
		g, ok := decl.(*ast.GenDecl)
		if !ok || g.Tok != token.CONST {
			continue
		}
		for _, spec := range g.Specs {
			v, ok := spec.(*ast.ValueSpec)
			if !ok || len(v.Names) != 1 || v.Names[0].Name != "zipdata" {
				continue
			}
			if archive != nil || len(v.Values) != 1 {
				return nil, errors.New("unexpected zipdata layout")
			}
			literal, ok := v.Values[0].(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return nil, errors.New("zipdata must be a literal")
			}
			data, err := strconv.Unquote(literal.Value)
			if err != nil || len(data) == 0 {
				return nil, errors.New("empty zipdata")
			}
			archive = []byte(data)
		}
	}
	if archive == nil {
		return nil, errors.New("zipdata missing")
	}
	return archive, nil
}
func verifyBinary(path string) error {
	info, err := buildinfo.ReadFile(path)
	if err != nil || info.GoVersion != "go1.26.9" || info.Path != "github.com/JustIkra/umir-serving/tests/fixtures/server" {
		return errors.New("fixture executable build identity mismatch")
	}
	f, err := elf.Open(path)
	if err != nil {
		return err
	}
	symbols, readErr := f.Symbols()
	if err := errors.Join(readErr, f.Close()); err != nil {
		return err
	}
	for _, symbol := range symbols {
		if symbol.Name == "time/tzdata.init.0" {
			return nil
		}
	}
	return errors.New("fixture executable lacks time/tzdata")
}
func run() error {
	if runtime.Version() != "go1.26.9" {
		return errors.New("wrong provenance toolchain")
	}
	source, err := os.ReadFile(filepath.Join("/usr/local/go", "src/time/tzdata/zzipdata.go"))
	if err != nil {
		return err
	}
	embedded, err := embeddedArchive(source)
	if err != nil {
		return err
	}
	archive, err := os.ReadFile(filepath.Join("/usr/local/go", "lib/time/zoneinfo.zip"))
	if err != nil {
		return err
	}
	if !bytes.Equal(embedded, archive) || len(archive) > 16777216 {
		return errors.New("embedded and toolchain archives differ")
	}
	if err := verifyBinary("/out/fixture-server"); err != nil {
		return err
	}
	binary, err := os.ReadFile("/out/fixture-server")
	if err != nil {
		return err
	}
	p := catalog.Provenance{Owner: "serving", Version: catalog.Version, ArchiveSHA256: digest(archive), ArchiveBytes: len(archive), EmbeddedArchiveSHA256: digest(embedded), BuildToolchain: runtime.Version(), ToolchainImageID: catalog.ToolchainImageID, ToolchainSourceSHA256: digest(source), BinarySHA256: digest(binary), SourceKind: "go-time-tzdata"}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if err := os.WriteFile("/out/catalog-provenance.json", raw, 0444); err != nil {
		return err
	}
	return os.WriteFile("/out/zoneinfo.zip", archive, 0444)
}
func main() {
	if len(os.Args) != 1 {
		os.Exit(1)
	}
	if err := run(); err != nil {
		if _, writeErr := fmt.Fprintln(os.Stderr, err); writeErr != nil {
			os.Exit(1)
		}
		os.Exit(1)
	}
}
