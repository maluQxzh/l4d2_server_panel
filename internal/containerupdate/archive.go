package containerupdate

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strings"
)

const (
	MaxPackageBytes         int64 = 1 << 30
	maxArchiveExpandedBytes int64 = 4 << 30
	maxArchiveEntries             = 2048
	maxManifestBytes        int64 = 1 << 20
	maxImageConfigBytes     int64 = 2 << 20
	offlineImageReference         = "l4d2panel/control:offline"
)

var digestPathPattern = regexp.MustCompile(`^blobs/sha256/([0-9a-f]{64})$`)

type validatedPackage struct {
	Version      string
	Commit       string
	Architecture string
	OS           string
	ImageRef     string
	ConfigDigest string
}

type dockerSaveManifest struct {
	Config   string   `json:"Config"`
	RepoTags []string `json:"RepoTags"`
	Layers   []string `json:"Layers"`
}

type dockerImageConfig struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Config       struct {
		Labels map[string]string `json:"Labels"`
	} `json:"config"`
}

func validatePackage(pathName string) (validatedPackage, error) {
	manifestBody, err := scanArchive(pathName, "manifest.json", maxManifestBytes)
	if err != nil {
		return validatedPackage{}, err
	}
	var manifests []dockerSaveManifest
	if err := json.Unmarshal(manifestBody, &manifests); err != nil || len(manifests) != 1 {
		return validatedPackage{}, errors.New("container update archive must contain exactly one Docker image manifest")
	}
	manifest := manifests[0]
	if len(manifest.RepoTags) != 1 || manifest.RepoTags[0] != offlineImageReference {
		return validatedPackage{}, fmt.Errorf("container update image must use the fixed %s tag", offlineImageReference)
	}
	match := digestPathPattern.FindStringSubmatch(manifest.Config)
	if len(match) != 2 {
		return validatedPackage{}, errors.New("container update image has an invalid config digest path")
	}
	if len(manifest.Layers) == 0 || len(manifest.Layers) > 256 {
		return validatedPackage{}, errors.New("container update image has an invalid layer list")
	}
	for _, layer := range manifest.Layers {
		if !digestPathPattern.MatchString(layer) {
			return validatedPackage{}, errors.New("container update image has an invalid layer digest path")
		}
	}

	configBody, err := scanArchive(pathName, manifest.Config, maxImageConfigBytes)
	if err != nil {
		return validatedPackage{}, err
	}
	configSum := sha256.Sum256(configBody)
	if hex.EncodeToString(configSum[:]) != match[1] {
		return validatedPackage{}, errors.New("container update image config digest does not match its path")
	}
	var config dockerImageConfig
	if err := json.Unmarshal(configBody, &config); err != nil {
		return validatedPackage{}, fmt.Errorf("decode container image config: %w", err)
	}
	if config.OS != "linux" || (config.Architecture != "amd64" && config.Architecture != "arm64") {
		return validatedPackage{}, fmt.Errorf("unsupported container image platform %s/%s", config.OS, config.Architecture)
	}
	if config.Config.Labels["org.opencontainers.image.title"] != "L4D2 Panel Control" {
		return validatedPackage{}, errors.New("container update image product label is invalid")
	}
	version := strings.TrimSpace(config.Config.Labels["org.opencontainers.image.version"])
	commit := strings.TrimSpace(config.Config.Labels["org.opencontainers.image.revision"])
	if version == "" || len(version) > 128 || commit == "" || len(commit) > 128 {
		return validatedPackage{}, errors.New("container update image version metadata is invalid")
	}
	return validatedPackage{Version: version, Commit: commit, Architecture: config.Architecture, OS: config.OS,
		ImageRef: offlineImageReference, ConfigDigest: match[1]}, nil
}

func scanArchive(pathName, captureName string, captureLimit int64) ([]byte, error) {
	file, err := os.Open(pathName)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return nil, errors.New("container update package must be a gzip-compressed Docker archive")
	}
	defer gzipReader.Close()
	gzipReader.Multistream(false)
	tape := tar.NewReader(gzipReader)
	seen := make(map[string]struct{})
	var captured []byte
	var expanded int64
	entries := 0
	for {
		header, err := tape.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read container update archive: %w", err)
		}
		entries++
		if entries > maxArchiveEntries {
			return nil, errors.New("container update archive contains too many entries")
		}
		name, err := safeTarPath(header.Name)
		if err != nil {
			return nil, err
		}
		if _, exists := seen[name]; exists {
			return nil, fmt.Errorf("container update archive repeats %q", name)
		}
		seen[name] = struct{}{}
		if header.Size < 0 || header.Size > maxArchiveExpandedBytes-expanded {
			return nil, errors.New("container update archive is too large when expanded")
		}
		expanded += header.Size
		directoryEntry := name == "blobs" || name == "blobs/sha256"
		if header.Typeflag == tar.TypeDir {
			if !directoryEntry {
				return nil, fmt.Errorf("container update archive contains unexpected directory %q", name)
			}
			continue
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return nil, fmt.Errorf("container update archive contains unsupported entry %q", name)
		}
		if directoryEntry {
			return nil, fmt.Errorf("container update archive entry %q must be a directory", name)
		}
		var writer io.Writer = io.Discard
		var capture strings.Builder
		if name == captureName {
			if header.Size > captureLimit {
				return nil, fmt.Errorf("container update metadata %q is too large", name)
			}
			capture.Grow(int(header.Size))
			writer = &capture
		}
		if match := digestPathPattern.FindStringSubmatch(name); len(match) == 2 {
			hash := sha256.New()
			writer = io.MultiWriter(writer, hash)
			written, err := io.CopyN(writer, tape, header.Size)
			if err != nil || written != header.Size {
				return nil, fmt.Errorf("read container update blob %q: %w", name, err)
			}
			if hex.EncodeToString(hash.Sum(nil)) != match[1] {
				return nil, fmt.Errorf("container update blob digest mismatch for %q", name)
			}
		} else {
			written, err := io.CopyN(writer, tape, header.Size)
			if err != nil || written != header.Size {
				return nil, fmt.Errorf("read container update entry %q: %w", name, err)
			}
		}
		if name == captureName {
			captured = []byte(capture.String())
		}
	}
	if _, exists := seen["manifest.json"]; !exists {
		return nil, errors.New("container update archive is missing manifest.json")
	}
	if captured == nil {
		return nil, fmt.Errorf("container update archive is missing %q", captureName)
	}
	return captured, nil
}

func safeTarPath(name string) (string, error) {
	if name == "" || strings.Contains(name, "\\") || strings.ContainsRune(name, 0) || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("unsafe container update archive path %q", name)
	}
	trimmed := strings.TrimSuffix(name, "/")
	clean := path.Clean(trimmed)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || clean != trimmed {
		return "", fmt.Errorf("unsafe container update archive path %q", name)
	}
	if clean == "manifest.json" || clean == "index.json" || clean == "oci-layout" || clean == "blobs" || clean == "blobs/sha256" || digestPathPattern.MatchString(clean) {
		return clean, nil
	}
	return "", fmt.Errorf("unexpected container update archive path %q", name)
}
