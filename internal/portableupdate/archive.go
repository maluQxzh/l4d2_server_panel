package portableupdate

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

const (
	ManifestName         = "portable-manifest.json"
	MaxPackageBytes      = 512 << 20
	MaxExpandedBytes     = 1 << 30
	MaxArchiveEntries    = 4096
	maxManifestBytes     = 1 << 20
	portableFormat       = 1
	portableProduct      = "l4d2panel-center"
	portablePlatform     = "windows"
	portableArchitecture = "amd64"
)

type Manifest struct {
	FormatVersion int            `json:"format_version"`
	Product       string         `json:"product"`
	Platform      string         `json:"platform"`
	Architecture  string         `json:"architecture"`
	Version       string         `json:"version"`
	Files         []ManifestFile `json:"files"`
}

type ManifestFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type ValidatedPackage struct {
	Manifest Manifest
	SHA256   string
	Files    map[string]ManifestFile
}

var releaseVersionPattern = regexp.MustCompile(`^v?([0-9]+)\.([0-9]+)\.([0-9]+)(?:-([0-9A-Za-z.-]+))?$`)

func ValidateAndExtract(zipPath, destination, currentVersion string) (ValidatedPackage, error) {
	info, err := os.Stat(zipPath)
	if err != nil {
		return ValidatedPackage{}, fmt.Errorf("open update package: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > MaxPackageBytes {
		return ValidatedPackage{}, fmt.Errorf("update package must be a regular ZIP no larger than %d bytes", MaxPackageBytes)
	}
	packageHash, err := hashFile(zipPath)
	if err != nil {
		return ValidatedPackage{}, err
	}
	reader, err := zip.OpenReader(zipPath)
	if err != nil {
		return ValidatedPackage{}, fmt.Errorf("open update ZIP: %w", err)
	}
	defer reader.Close()
	if len(reader.File) == 0 || len(reader.File) > MaxArchiveEntries {
		return ValidatedPackage{}, fmt.Errorf("update ZIP entry count must be between 1 and %d", MaxArchiveEntries)
	}

	entries := make(map[string]*zip.File, len(reader.File))
	caseFolded := make(map[string]string, len(reader.File))
	var expanded uint64
	for _, entry := range reader.File {
		name, err := safeArchivePath(entry.Name)
		if err != nil {
			return ValidatedPackage{}, err
		}
		if _, exists := entries[name]; exists {
			return ValidatedPackage{}, fmt.Errorf("duplicate update ZIP path %q", name)
		}
		folded := strings.ToLower(name)
		if previous, exists := caseFolded[folded]; exists {
			return ValidatedPackage{}, fmt.Errorf("case-insensitive duplicate update ZIP paths %q and %q", previous, name)
		}
		if entry.Mode()&os.ModeType != 0 && !entry.FileInfo().IsDir() {
			return ValidatedPackage{}, fmt.Errorf("update ZIP contains a link or special file %q", name)
		}
		// Some Windows archivers preserve FILE_ATTRIBUTE_REPARSE_POINT in
		// either half of ExternalAttrs. Reject both encodings before extraction.
		if entry.ExternalAttrs&0x00000400 != 0 || entry.ExternalAttrs&0x04000000 != 0 {
			return ValidatedPackage{}, fmt.Errorf("update ZIP contains a reparse point %q", name)
		}
		if entry.UncompressedSize64 > MaxExpandedBytes || expanded+entry.UncompressedSize64 > MaxExpandedBytes {
			return ValidatedPackage{}, fmt.Errorf("expanded update ZIP exceeds %d bytes", MaxExpandedBytes)
		}
		if entry.UncompressedSize64 > 1<<20 && entry.CompressedSize64*200 < entry.UncompressedSize64 {
			return ValidatedPackage{}, fmt.Errorf("update ZIP entry %q has an unsafe compression ratio", name)
		}
		expanded += entry.UncompressedSize64
		entries[name] = entry
		caseFolded[folded] = name
	}
	manifestEntry := entries[ManifestName]
	if manifestEntry == nil || manifestEntry.FileInfo().IsDir() || manifestEntry.UncompressedSize64 > maxManifestBytes {
		return ValidatedPackage{}, errors.New("update ZIP has no valid portable-manifest.json")
	}
	manifest, err := readManifest(manifestEntry)
	if err != nil {
		return ValidatedPackage{}, err
	}
	files, err := validateManifest(manifest, entries, currentVersion)
	if err != nil {
		return ValidatedPackage{}, err
	}
	if err := os.RemoveAll(destination); err != nil {
		return ValidatedPackage{}, fmt.Errorf("clear update staging: %w", err)
	}
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return ValidatedPackage{}, fmt.Errorf("create update staging: %w", err)
	}
	for name, expected := range files {
		entry := entries[name]
		target := filepath.Join(destination, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return ValidatedPackage{}, err
		}
		if err := extractVerified(entry, target, expected); err != nil {
			return ValidatedPackage{}, err
		}
	}
	manifestTarget := filepath.Join(destination, ManifestName)
	if err := extractBounded(manifestEntry, manifestTarget, maxManifestBytes); err != nil {
		return ValidatedPackage{}, err
	}
	return ValidatedPackage{Manifest: manifest, SHA256: packageHash, Files: files}, nil
}

func readManifest(entry *zip.File) (Manifest, error) {
	r, err := entry.Open()
	if err != nil {
		return Manifest{}, err
	}
	defer r.Close()
	decoder := json.NewDecoder(io.LimitReader(r, maxManifestBytes+1))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode portable manifest: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return Manifest{}, errors.New("portable manifest must contain one JSON value")
	}
	return manifest, nil
}

func validateManifest(manifest Manifest, entries map[string]*zip.File, currentVersion string) (map[string]ManifestFile, error) {
	if manifest.FormatVersion != portableFormat || manifest.Product != portableProduct || manifest.Platform != portablePlatform || manifest.Architecture != portableArchitecture {
		return nil, errors.New("update package is not an L4D2 Panel Windows x64 portable package")
	}
	if _, ok := parseVersion(manifest.Version); !ok {
		return nil, errors.New("portable manifest version is not a release semantic version")
	}
	if current, ok := parseVersion(currentVersion); ok {
		target, _ := parseVersion(manifest.Version)
		if compareVersion(target, current) < 0 {
			return nil, fmt.Errorf("downgrade from %s to %s is not allowed", currentVersion, manifest.Version)
		}
	}
	if len(manifest.Files) == 0 || len(manifest.Files) > MaxArchiveEntries-1 {
		return nil, errors.New("portable manifest has an invalid file count")
	}
	files := make(map[string]ManifestFile, len(manifest.Files))
	caseFolded := make(map[string]string, len(manifest.Files))
	hasControl, hasMigration := false, false
	for _, item := range manifest.Files {
		name, err := safeArchivePath(item.Path)
		if err != nil || name != item.Path || name == ManifestName {
			return nil, fmt.Errorf("portable manifest contains an unsafe path %q", item.Path)
		}
		if item.Size < 0 || !validSHA256(item.SHA256) {
			return nil, fmt.Errorf("portable manifest has invalid metadata for %q", name)
		}
		if _, exists := files[name]; exists {
			return nil, fmt.Errorf("portable manifest repeats %q", name)
		}
		folded := strings.ToLower(name)
		if previous, exists := caseFolded[folded]; exists {
			return nil, fmt.Errorf("portable manifest has case-insensitive duplicate paths %q and %q", previous, name)
		}
		entry := entries[name]
		if entry == nil || entry.FileInfo().IsDir() || int64(entry.UncompressedSize64) != item.Size {
			return nil, fmt.Errorf("update ZIP does not match manifest entry %q", name)
		}
		item.SHA256 = strings.ToLower(item.SHA256)
		files[name] = item
		caseFolded[folded] = name
		hasControl = hasControl || strings.EqualFold(name, "l4d2panel-control.exe")
		hasMigration = hasMigration || strings.HasPrefix(name, "migrations/sqlite/") && strings.HasSuffix(strings.ToLower(name), ".sql")
	}
	if !hasControl || !hasMigration {
		return nil, errors.New("portable package must contain Control and SQLite migrations")
	}
	for name, entry := range entries {
		if entry.FileInfo().IsDir() || name == ManifestName {
			continue
		}
		if _, listed := files[name]; !listed {
			return nil, fmt.Errorf("update ZIP contains unlisted file %q", name)
		}
	}
	return files, nil
}

func safeArchivePath(name string) (string, error) {
	if name == "" || strings.Contains(name, "\\") || strings.ContainsRune(name, 0) || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("unsafe update ZIP path %q", name)
	}
	clean := path.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || clean != strings.TrimSuffix(name, "/") {
		return "", fmt.Errorf("unsafe update ZIP path %q", name)
	}
	for _, component := range strings.Split(clean, "/") {
		trimmed := strings.TrimRight(component, ". ")
		if trimmed == "" || trimmed != component || strings.Contains(trimmed, ":") || windowsDeviceName(trimmed) {
			return "", fmt.Errorf("unsafe Windows update path component %q", component)
		}
	}
	return clean, nil
}

func windowsDeviceName(component string) bool {
	base := strings.ToUpper(strings.SplitN(component, ".", 2)[0])
	if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" {
		return true
	}
	if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9' {
		return true
	}
	return false
}

func extractVerified(entry *zip.File, target string, expected ManifestFile) error {
	r, err := entry.Open()
	if err != nil {
		return err
	}
	defer r.Close()
	temp := target + ".tmp"
	f, err := os.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(f, hash), io.LimitReader(r, expected.Size+1))
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil || written != expected.Size || hex.EncodeToString(hash.Sum(nil)) != expected.SHA256 {
		_ = os.Remove(temp)
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		return fmt.Errorf("update file integrity check failed for %q", expected.Path)
	}
	if err := os.Rename(temp, target); err != nil {
		_ = os.Remove(temp)
		return err
	}
	return nil
}

func extractBounded(entry *zip.File, target string, limit int64) error {
	r, err := entry.Open()
	if err != nil {
		return err
	}
	defer r.Close()
	f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	written, copyErr := io.Copy(f, io.LimitReader(r, limit+1))
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil || written > limit {
		_ = os.Remove(target)
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		return errors.New("portable manifest exceeds size limit")
	}
	return nil
}

func hashFile(filename string) (string, error) {
	f, err := os.Open(filename)
	if err != nil {
		return "", err
	}
	defer f.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

type releaseVersion struct {
	major, minor, patch int
	prerelease          string
}

func parseVersion(value string) (releaseVersion, bool) {
	match := releaseVersionPattern.FindStringSubmatch(strings.TrimSpace(value))
	if match == nil {
		return releaseVersion{}, false
	}
	major, e1 := strconv.Atoi(match[1])
	minor, e2 := strconv.Atoi(match[2])
	patch, e3 := strconv.Atoi(match[3])
	if match[4] != "" {
		for _, identifier := range strings.Split(match[4], ".") {
			if identifier == "" || (numericIdentifier(identifier) && len(identifier) > 1 && identifier[0] == '0') {
				return releaseVersion{}, false
			}
		}
	}
	return releaseVersion{major: major, minor: minor, patch: patch, prerelease: match[4]}, e1 == nil && e2 == nil && e3 == nil
}

func compareVersion(left, right releaseVersion) int {
	for _, pair := range [][2]int{{left.major, right.major}, {left.minor, right.minor}, {left.patch, right.patch}} {
		if pair[0] < pair[1] {
			return -1
		}
		if pair[0] > pair[1] {
			return 1
		}
	}
	if left.prerelease == right.prerelease {
		return 0
	}
	if left.prerelease == "" {
		return 1
	}
	if right.prerelease == "" {
		return -1
	}
	leftIdentifiers, rightIdentifiers := strings.Split(left.prerelease, "."), strings.Split(right.prerelease, ".")
	for index := 0; index < len(leftIdentifiers) && index < len(rightIdentifiers); index++ {
		leftID, rightID := leftIdentifiers[index], rightIdentifiers[index]
		leftNumeric, rightNumeric := numericIdentifier(leftID), numericIdentifier(rightID)
		switch {
		case leftNumeric && rightNumeric:
			if len(leftID) < len(rightID) {
				return -1
			}
			if len(leftID) > len(rightID) {
				return 1
			}
			if comparison := strings.Compare(leftID, rightID); comparison != 0 {
				return comparison
			}
		case leftNumeric:
			return -1
		case rightNumeric:
			return 1
		default:
			if comparison := strings.Compare(leftID, rightID); comparison != 0 {
				return comparison
			}
		}
	}
	if len(leftIdentifiers) < len(rightIdentifiers) {
		return -1
	}
	if len(leftIdentifiers) > len(rightIdentifiers) {
		return 1
	}
	return 0
}

func numericIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func RuntimeSupported() bool { return runtime.GOOS == "windows" && runtime.GOARCH == "amd64" }

func SortedManifestPaths(manifest Manifest) []string {
	paths := make([]string, 0, len(manifest.Files)+1)
	for _, item := range manifest.Files {
		paths = append(paths, item.Path)
	}
	paths = append(paths, ManifestName)
	sort.Strings(paths)
	return paths
}
