package catalog

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"runtime"
	"strings"
	"time"
)

const Amendment = "umir-owner-profiles/0.1.0-unpublished"
const Path = "/owner-profile/v1"
const RequestPath = "/run/umir-fixtures/profile-request.json"
const ArchivePath = "/usr/local/lib/umir/zoneinfo.zip"
const ProvenancePath = "/usr/local/lib/umir/catalog-provenance.json"
const Version = "go1.26.9-embedded"
const ToolchainImageID = "sha256:3082400e369fa24d5fc60bca20edab3f6d604e0c5a690ec66b295eff4dd87ade"

type Request struct {
	Amendment          string `json:"amendment"`
	Project            string `json:"project"`
	RequestID          string `json:"requestID"`
	ExpectedInstanceID string `json:"expectedInstanceID"`
	ExpectedGeneration string `json:"expectedGeneration"`
	Action             string `json:"action"`
}
type OwnerClock struct {
	Instant    string `json:"instant"`
	Generation string `json:"generation"`
	InstanceID string `json:"instanceID"`
	Source     string `json:"source"`
	Healthy    bool   `json:"healthy"`
}
type Fact struct {
	Owner            string `json:"owner"`
	Version          string `json:"version"`
	ArchiveSHA256    string `json:"archiveSHA256"`
	ArchiveBytes     int    `json:"archiveBytes"`
	BuildToolchain   string `json:"buildToolchain"`
	Ready            bool   `json:"ready"`
	ProvenanceSHA256 string `json:"provenanceSHA256"`
}
type CatalogReport struct {
	Amendment    string     `json:"amendment"`
	RequestID    string     `json:"requestID"`
	Project      string     `json:"project"`
	InstanceID   string     `json:"instanceID"`
	Clock        OwnerClock `json:"clock"`
	BinarySHA256 string     `json:"binarySHA256"`
	ConfigSHA256 string     `json:"configSHA256"`
	Catalog      Fact       `json:"catalog"`
}
type Provenance struct {
	Owner                 string `json:"owner"`
	Version               string `json:"version"`
	ArchiveSHA256         string `json:"archiveSHA256"`
	ArchiveBytes          int    `json:"archiveBytes"`
	EmbeddedArchiveSHA256 string `json:"embeddedArchiveSHA256"`
	BuildToolchain        string `json:"buildToolchain"`
	ToolchainImageID      string `json:"toolchainImageID"`
	ToolchainSourceSHA256 string `json:"toolchainSourceSHA256"`
	BinarySHA256          string `json:"binarySHA256"`
	SourceKind            string `json:"sourceKind"`
}
type Clock interface{ CatalogSnapshot() CatalogReport }

func digest(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }
func validDigest(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 32 && s == strings.ToLower(s)
}

func readPackaged(path string, limit int64) (raw []byte, err error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > limit || info.Mode().Perm()&0222 != 0 {
		return nil, errors.New("invalid packaged catalog file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	raw, readErr := io.ReadAll(io.LimitReader(f, limit+1))
	err = errors.Join(readErr, f.Close())
	if err != nil || int64(len(raw)) > limit {
		return nil, errors.New("catalog read failed")
	}
	return raw, nil
}

func Observe(ctx context.Context, clock Clock) (CatalogReport, error) {
	if clock == nil || ctx.Err() != nil || os.Getenv("ZONEINFO") != "" || runtime.Version() != "go1.26.9" {
		return CatalogReport{}, errors.New("catalog environment not qualified")
	}
	report := clock.CatalogSnapshot()
	provenance, err := readPackaged(ProvenancePath, 4096)
	if err != nil {
		return CatalogReport{}, err
	}
	archive, err := readPackaged(ArchivePath, 16777216)
	if err != nil {
		return CatalogReport{}, err
	}
	return observe(ctx, report, provenance, archive)
}

func observe(ctx context.Context, report CatalogReport, raw, archive []byte) (CatalogReport, error) {
	var p Provenance
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&p) != nil || d.Decode(new(any)) != io.EOF || p.Owner != "serving" || p.Version != Version || p.BuildToolchain != "go1.26.9" || p.ToolchainImageID != ToolchainImageID || p.SourceKind != "go-time-tzdata" || !validDigest(p.ToolchainSourceSHA256) || p.BinarySHA256 != report.BinarySHA256 || !validDigest(p.BinarySHA256) || len(archive) < 1 || len(archive) > 16777216 || p.ArchiveBytes != len(archive) || p.ArchiveSHA256 != digest(archive) || p.EmbeddedArchiveSHA256 != p.ArchiveSHA256 {
		return CatalogReport{}, errors.New("catalog provenance mismatch")
	}
	z, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return CatalogReport{}, errors.New("catalog archive invalid")
	}
	found := false
	for _, f := range z.File {
		if f.Name != "Europe/Moscow" {
			continue
		}
		if found || f.UncompressedSize64 > 1048576 {
			return CatalogReport{}, errors.New("catalog Moscow entry invalid")
		}
		r, err := f.Open()
		if err != nil {
			return CatalogReport{}, err
		}
		b, readErr := io.ReadAll(io.LimitReader(r, 1048577))
		if err := errors.Join(readErr, r.Close()); err != nil || len(b) > 1048576 {
			return CatalogReport{}, errors.New("catalog Moscow read failed")
		}
		if _, err := time.LoadLocationFromTZData("Europe/Moscow", b); err != nil {
			return CatalogReport{}, errors.New("catalog Moscow invalid")
		}
		found = true
	}
	if !found || ctx.Err() != nil {
		return CatalogReport{}, errors.New("catalog unavailable")
	}
	report.Catalog = Fact{Owner: p.Owner, Version: p.Version, ArchiveSHA256: p.ArchiveSHA256, ArchiveBytes: p.ArchiveBytes, BuildToolchain: p.BuildToolchain, Ready: true, ProvenanceSHA256: digest(raw)}
	return report, nil
}
