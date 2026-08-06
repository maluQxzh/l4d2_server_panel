package containerupdate

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateContainerUpdatePackage(t *testing.T) {
	archive := writeTestArchive(t, "amd64", offlineImageReference, false)
	validated, err := validatePackage(archive)
	if err != nil {
		t.Fatal(err)
	}
	if validated.OS != "linux" || validated.Architecture != "amd64" || validated.Version != "1.2.3" {
		t.Fatalf("unexpected validated package: %+v", validated)
	}
}

func TestValidateContainerUpdatePackageRejectsWrongTagAndDigest(t *testing.T) {
	if _, err := validatePackage(writeTestArchive(t, "amd64", "example/control:latest", false)); err == nil || !strings.Contains(err.Error(), "fixed") {
		t.Fatalf("wrong image tag was accepted: %v", err)
	}
	if _, err := validatePackage(writeTestArchive(t, "arm64", offlineImageReference, true)); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("corrupt blob was accepted: %v", err)
	}
}

func TestSafeTarPathRejectsTraversalAndLinks(t *testing.T) {
	for _, name := range []string{"../manifest.json", "/manifest.json", `blobs\sha256\value`, "unexpected"} {
		if _, err := safeTarPath(name); err == nil {
			t.Fatalf("unsafe archive path %q was accepted", name)
		}
	}
	for _, name := range []string{"manifest.json", "index.json", "oci-layout", "blobs", "blobs/sha256"} {
		if _, err := safeTarPath(name); err != nil {
			t.Fatalf("valid archive path %q was rejected: %v", name, err)
		}
	}
}

func TestValidateExternalContainerUpdatePackage(t *testing.T) {
	path := os.Getenv("CONTAINER_UPDATE_TEST_PACKAGE")
	if path == "" {
		t.Skip("CONTAINER_UPDATE_TEST_PACKAGE is not set")
	}
	if _, err := validatePackage(path); err != nil {
		t.Fatal(err)
	}
}

func writeTestArchive(t *testing.T, architecture, reference string, corruptLayer bool) string {
	t.Helper()
	config := map[string]any{
		"architecture": architecture,
		"os":           "linux",
		"config": map[string]any{"Labels": map[string]string{
			"org.opencontainers.image.title":    "L4D2 Panel Control",
			"org.opencontainers.image.version":  "1.2.3",
			"org.opencontainers.image.revision": "abc123",
		}},
	}
	configBody, _ := json.Marshal(config)
	configSum := sha256.Sum256(configBody)
	configPath := "blobs/sha256/" + hex.EncodeToString(configSum[:])
	layerBody := []byte("layer-body")
	layerSum := sha256.Sum256(layerBody)
	layerPath := "blobs/sha256/" + hex.EncodeToString(layerSum[:])
	manifestBody, _ := json.Marshal([]dockerSaveManifest{{Config: configPath, RepoTags: []string{reference}, Layers: []string{layerPath}}})
	if corruptLayer {
		layerBody = []byte("changed-layer-body")
	}

	archivePath := filepath.Join(t.TempDir(), "update.tar.gz")
	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	gzipWriter := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gzipWriter)
	for name, body := range map[string][]byte{configPath: configBody, layerPath: layerBody, "manifest.json": manifestBody, "index.json": []byte(`{}`), "oci-layout": []byte(`{"imageLayoutVersion":"1.0.0"}`)} {
		if err := tarWriter.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return archivePath
}
