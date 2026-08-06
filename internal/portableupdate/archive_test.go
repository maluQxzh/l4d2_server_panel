package portableupdate

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidateAndExtractPortablePackage(t *testing.T) {
	zipPath := buildTestPackage(t, "1.2.0", map[string]string{
		"l4d2panel-control.exe":     "control",
		"migrations/sqlite/001.sql": "CREATE TABLE test(id);",
		"PORTABLE-README.txt":       "read me",
	})
	destination := filepath.Join(t.TempDir(), "staging")
	validated, err := ValidateAndExtract(zipPath, destination, "1.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if validated.Manifest.Version != "1.2.0" || len(validated.SHA256) != 64 {
		t.Fatalf("unexpected validation result: %+v", validated)
	}
	if body, err := os.ReadFile(filepath.Join(destination, "l4d2panel-control.exe")); err != nil || string(body) != "control" {
		t.Fatalf("extracted Control mismatch: %q, %v", body, err)
	}
}

func TestRotateSnapshotsKeepsNewestThree(t *testing.T) {
	root := t.TempDir()
	manager := &Manager{Root: root}
	base := filepath.Join(root, "data", "backups", "upgrades")
	for index := 0; index < 5; index++ {
		directory := filepath.Join(base, string(rune('a'+index)))
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		at := time.Unix(int64(index+1), 0)
		if err := os.Chtimes(directory, at, at); err != nil {
			t.Fatal(err)
		}
	}
	if err := manager.rotateSnapshots(3); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 || entries[0].Name() != "c" || entries[2].Name() != "e" {
		t.Fatalf("unexpected retained snapshots: %v", entries)
	}
}

func TestValidateAndExtractRejectsDowngradeAndHashMismatch(t *testing.T) {
	zipPath := buildTestPackage(t, "1.1.9", map[string]string{
		"l4d2panel-control.exe":     "control",
		"migrations/sqlite/001.sql": "migration",
	})
	if _, err := ValidateAndExtract(zipPath, filepath.Join(t.TempDir(), "staging"), "1.2.0"); err == nil || !strings.Contains(err.Error(), "downgrade") {
		t.Fatalf("downgrade was not rejected: %v", err)
	}

	corrupt := filepath.Join(t.TempDir(), "corrupt.zip")
	writer, err := os.Create(corrupt)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(writer)
	manifest := Manifest{FormatVersion: 1, Product: portableProduct, Platform: portablePlatform, Architecture: portableArchitecture, Version: "1.2.0", Files: []ManifestFile{
		{Path: "l4d2panel-control.exe", Size: 7, SHA256: strings.Repeat("0", 64)},
		{Path: "migrations/sqlite/001.sql", Size: 1, SHA256: strings.Repeat("0", 64)},
	}}
	writeZipEntry(t, archive, ManifestName, mustJSON(t, manifest))
	writeZipEntry(t, archive, "l4d2panel-control.exe", []byte("control"))
	writeZipEntry(t, archive, "migrations/sqlite/001.sql", []byte("x"))
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateAndExtract(corrupt, filepath.Join(t.TempDir(), "bad"), "1.1.0"); err == nil || !strings.Contains(err.Error(), "integrity") {
		t.Fatalf("hash mismatch was not rejected: %v", err)
	}
}

func TestPortableUpdateVersionPolicy(t *testing.T) {
	for _, testCase := range []struct {
		target  string
		current string
		allowed bool
	}{
		{target: "1.0.0", current: "1.0.0", allowed: true},
		{target: "1.0.0-10", current: "1.0.0-2", allowed: true},
		{target: "1.0.0-alpha", current: "1.0.0-1", allowed: true},
		{target: "1.0.0-2", current: "1.0.0-10", allowed: false},
		{target: "dev", current: "dev", allowed: true},
		{target: "dev-b", current: "dev-a", allowed: true},
		{target: "1.1.0", current: "dev", allowed: true},
		{target: "dev", current: "1.0.0", allowed: false},
		{target: "dev build", current: "dev", allowed: false},
		{target: "1.0.0-alpha..1", current: "1.0.0-alpha", allowed: false},
		{target: "1.0.0-01", current: "1.0.0-1", allowed: false},
	} {
		t.Run(testCase.target+"_from_"+testCase.current, func(t *testing.T) {
			zipPath := buildTestPackage(t, testCase.target, map[string]string{
				"l4d2panel-control.exe":     "control",
				"migrations/sqlite/001.sql": "migration",
			})
			_, err := ValidateAndExtract(zipPath, filepath.Join(t.TempDir(), "out"), testCase.current)
			if testCase.allowed && err != nil {
				t.Fatalf("expected version to be allowed: %v", err)
			}
			if !testCase.allowed && err == nil {
				t.Fatal("expected version to be rejected")
			}
		})
	}
}

func TestValidateAndExtractRejectsTraversalAndUnlistedFiles(t *testing.T) {
	for _, unsafeName := range []string{"../escape.exe", "folder\\escape.exe", "CON.txt", "folder/file:stream", "folder/trailing. "} {
		t.Run(unsafeName, func(t *testing.T) {
			zipPath := filepath.Join(t.TempDir(), "unsafe.zip")
			file, err := os.Create(zipPath)
			if err != nil {
				t.Fatal(err)
			}
			archive := zip.NewWriter(file)
			writeZipEntry(t, archive, unsafeName, []byte("unsafe"))
			archive.Close()
			file.Close()
			if _, err := ValidateAndExtract(zipPath, filepath.Join(t.TempDir(), "out"), "1.0.0"); err == nil {
				t.Fatalf("unsafe path %q was accepted", unsafeName)
			}
		})
	}

	zipPath := filepath.Join(t.TempDir(), "unlisted.zip")
	file, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	files := map[string]string{"l4d2panel-control.exe": "control", "migrations/sqlite/001.sql": "migration"}
	manifest := Manifest{FormatVersion: 1, Product: portableProduct, Platform: portablePlatform, Architecture: portableArchitecture, Version: "1.2.0"}
	for name, body := range files {
		digest := sha256.Sum256([]byte(body))
		manifest.Files = append(manifest.Files, ManifestFile{Path: name, Size: int64(len(body)), SHA256: hex.EncodeToString(digest[:])})
		writeZipEntry(t, archive, name, []byte(body))
	}
	writeZipEntry(t, archive, "unlisted.txt", []byte("unexpected"))
	writeZipEntry(t, archive, ManifestName, mustJSON(t, manifest))
	archive.Close()
	file.Close()
	if _, err := ValidateAndExtract(zipPath, filepath.Join(t.TempDir(), "out"), "1.0.0"); err == nil || !strings.Contains(err.Error(), "unlisted") {
		t.Fatalf("unlisted file was not rejected: %v", err)
	}
}

func TestValidateAndExtractRejectsWindowsReparsePoint(t *testing.T) {
	for name, attributes := range map[string]uint32{"low-word": 0x00000400, "high-word": 0x04000000} {
		t.Run(name, func(t *testing.T) {
			zipPath := filepath.Join(t.TempDir(), "reparse.zip")
			file, err := os.Create(zipPath)
			if err != nil {
				t.Fatal(err)
			}
			archive := zip.NewWriter(file)
			files := map[string]string{"l4d2panel-control.exe": "control", "migrations/sqlite/001.sql": "migration"}
			manifest := Manifest{FormatVersion: 1, Product: portableProduct, Platform: portablePlatform, Architecture: portableArchitecture, Version: "1.2.0"}
			for path, body := range files {
				digest := sha256.Sum256([]byte(body))
				manifest.Files = append(manifest.Files, ManifestFile{Path: path, Size: int64(len(body)), SHA256: hex.EncodeToString(digest[:])})
				header := &zip.FileHeader{Name: path, Method: zip.Store}
				if path == "l4d2panel-control.exe" {
					header.ExternalAttrs = attributes
				}
				entry, err := archive.CreateHeader(header)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := entry.Write([]byte(body)); err != nil {
					t.Fatal(err)
				}
			}
			writeZipEntry(t, archive, ManifestName, mustJSON(t, manifest))
			if err := archive.Close(); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := ValidateAndExtract(zipPath, filepath.Join(t.TempDir(), "out"), "1.1.0"); err == nil || !strings.Contains(err.Error(), "reparse point") {
				t.Fatalf("expected reparse point rejection, got %v", err)
			}
		})
	}
}

func buildTestPackage(t *testing.T, version string, files map[string]string) string {
	t.Helper()
	zipPath := filepath.Join(t.TempDir(), "package.zip")
	file, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	manifest := Manifest{FormatVersion: 1, Product: portableProduct, Platform: portablePlatform, Architecture: portableArchitecture, Version: version}
	for name, body := range files {
		digest := sha256.Sum256([]byte(body))
		manifest.Files = append(manifest.Files, ManifestFile{Path: name, Size: int64(len(body)), SHA256: hex.EncodeToString(digest[:])})
		writeZipEntry(t, archive, name, []byte(body))
	}
	writeZipEntry(t, archive, ManifestName, mustJSON(t, manifest))
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return zipPath
}

func writeZipEntry(t *testing.T, archive *zip.Writer, name string, body []byte) {
	t.Helper()
	entry, err := archive.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write(body); err != nil {
		t.Fatal(err)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return body
}
