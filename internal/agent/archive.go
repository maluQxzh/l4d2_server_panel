package agent

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

const (
	backupLegacyFormatVersion = 1
	backupLaunchFormatVersion = 2
	backupFormatVersion       = 3
	maxBackupFiles            = 100000
	maxRestoreBytes           = int64(20 << 30)
)

var backupRoots = []string{"cfg", "addons", "left4dead2/cfg", "left4dead2/addons"}

type BackupManifest struct {
	Version    int                  `json:"version"`
	InstanceID string               `json:"instance_id"`
	OS         string               `json:"os"`
	Arch       string               `json:"arch"`
	CreatedAt  time.Time            `json:"created_at"`
	Files      map[string]string    `json:"files"`
	Launch     *BackupLaunchConfig  `json:"launch,omitempty"`
	Maps       []BackupMapReference `json:"maps,omitempty"`
}

type BackupMapReference struct {
	ID              string   `json:"id"`
	AggregateSHA256 string   `json:"aggregate_sha256"`
	Name            string   `json:"name"`
	BSPs            []string `json:"bsps"`
}

func ReadBackupManifest(archivePath string) (BackupManifest, error) {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return BackupManifest{}, err
	}
	defer zr.Close()
	return readBackupManifestEntries(zr.File)
}

func CreateBackup(instanceID, instancePath, destination string) (BackupManifest, error) {
	// Kept for writing v1 compatibility fixtures. Production backups use
	// CreateBackupWithMaps and always include the v3 portable launch snapshot.
	return createBackup(instanceID, instancePath, destination, backupLegacyFormatVersion, nil, nil, nil)
}

func CreateBackupWithLaunch(instanceID, instancePath, destination string, launch BackupLaunchConfig) (BackupManifest, error) {
	if err := validateBackupLaunchConfig(launch); err != nil {
		return BackupManifest{}, fmt.Errorf("backup launch configuration: %w", err)
	}
	// Retained for v2 compatibility fixtures and callers that intentionally do
	// not know about map references. The production Executor uses v3 below.
	return createBackup(instanceID, instancePath, destination, backupLaunchFormatVersion, &launch, nil, nil)
}

func CreateBackupWithMaps(instanceID, instancePath, destination string, launch BackupLaunchConfig, maps []BackupMapReference, excludedMapFiles map[string]bool) (BackupManifest, error) {
	if err := validateBackupLaunchConfig(launch); err != nil {
		return BackupManifest{}, fmt.Errorf("backup launch configuration: %w", err)
	}
	return createBackup(instanceID, instancePath, destination, backupFormatVersion, &launch, maps, excludedMapFiles)
}

func createBackup(instanceID, instancePath, destination string, version int, launch *BackupLaunchConfig, maps []BackupMapReference, excludedMapFiles map[string]bool) (BackupManifest, error) {
	if err := os.MkdirAll(filepath.Dir(destination), 0o750); err != nil {
		return BackupManifest{}, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(destination), ".backup-*.zip")
	if err != nil {
		return BackupManifest{}, err
	}
	name := tmp.Name()
	defer os.Remove(name)
	zw := zip.NewWriter(tmp)
	manifest := BackupManifest{Version: version, InstanceID: instanceID, OS: runtime.GOOS, Arch: runtime.GOARCH, CreatedAt: time.Now().UTC(), Files: make(map[string]string), Launch: launch, Maps: maps}
	count := 0
	var total int64
	for _, root := range backupRoots {
		base, err := ResolveWithin(instancePath, filepath.FromSlash(root))
		if err != nil {
			zw.Close()
			tmp.Close()
			return BackupManifest{}, err
		}
		if _, err := os.Stat(base); os.IsNotExist(err) {
			continue
		}
		err = filepath.WalkDir(base, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("backup refuses symlink %q", path)
			}
			if entry.IsDir() {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("backup refuses non-regular file %q", path)
			}
			if info.Size() < 0 || info.Size() > maxRestoreBytes-total {
				return errors.New("backup portable data exceeds 20 GiB limit")
			}
			total += info.Size()
			rel, err := filepath.Rel(instancePath, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if excludedMapFiles[strings.ToLower(rel)] {
				return nil
			}
			if isReservedMapAddonPath(rel) {
				return fmt.Errorf("backup found an unregistered reserved map file %q", rel)
			}
			if isGeneratedSecretConfig(rel) {
				return nil
			}
			count++
			if count > maxBackupFiles {
				return errors.New("backup file count exceeds limit")
			}
			in, err := os.Open(path)
			if err != nil {
				return err
			}
			header, err := zip.FileInfoHeader(info)
			if err != nil {
				_ = in.Close()
				return err
			}
			header.Name = "data/" + rel
			header.Method = zip.Deflate
			out, err := zw.CreateHeader(header)
			if err != nil {
				_ = in.Close()
				return err
			}
			hash := sha256.New()
			written, copyErr := io.Copy(io.MultiWriter(out, hash), io.LimitReader(in, info.Size()+1))
			closeErr := in.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
			if written != info.Size() {
				return fmt.Errorf("backup source file %q changed while reading", path)
			}
			manifest.Files[rel] = hex.EncodeToString(hash.Sum(nil))
			return nil
		})
		if err != nil {
			zw.Close()
			tmp.Close()
			return BackupManifest{}, err
		}
	}
	if err := validateBackupManifest(manifest); err != nil {
		_ = zw.Close()
		_ = tmp.Close()
		return BackupManifest{}, err
	}
	manifestRaw, marshalErr := json.MarshalIndent(manifest, "", "  ")
	if marshalErr != nil || len(manifestRaw) > maxBackupManifestBytes {
		_ = zw.Close()
		_ = tmp.Close()
		if marshalErr != nil {
			return BackupManifest{}, marshalErr
		}
		return BackupManifest{}, errors.New("backup manifest exceeds 4 MiB")
	}
	manifestWriter, err := zw.Create("manifest.json")
	if err == nil {
		_, err = manifestWriter.Write(manifestRaw)
	}
	if closeErr := zw.Close(); err == nil {
		err = closeErr
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return BackupManifest{}, err
	}
	if err := os.Rename(name, destination); err != nil {
		return BackupManifest{}, err
	}
	return manifest, nil
}

func isGeneratedSecretConfig(relative string) bool {
	return strings.EqualFold(filepath.ToSlash(relative), "left4dead2/cfg/l4d2panel.cfg")
}

func RestoreBackup(instancePath, archivePath, stagingDir string) (BackupManifest, []string, error) {
	if !filepath.IsAbs(stagingDir) || !strings.HasPrefix(filepath.Base(stagingDir), "restore-") {
		return BackupManifest{}, nil, errors.New("restore staging path is invalid")
	}
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return BackupManifest{}, nil, err
	}
	defer zr.Close()
	if len(zr.File) > maxBackupFiles+1 {
		return BackupManifest{}, nil, errors.New("backup contains too many entries")
	}
	manifest, err := readBackupManifestEntries(zr.File)
	if err != nil {
		return BackupManifest{}, nil, err
	}
	if err := os.RemoveAll(stagingDir); err != nil {
		return manifest, nil, err
	}
	if err := os.MkdirAll(stagingDir, 0o700); err != nil {
		return manifest, nil, err
	}
	defer os.RemoveAll(stagingDir)
	seen := make(map[string]bool)
	restored := make(map[string]bool)
	skippedBinaries := make([]string, 0)
	var declaredTotal, actualTotal int64
	for _, file := range zr.File {
		if file.Name == "manifest.json" {
			continue
		}
		if file.Mode()&os.ModeSymlink != 0 {
			return manifest, nil, errors.New("backup contains a symlink")
		}
		if strings.HasSuffix(file.Name, "/") {
			if !file.Mode().IsDir() {
				return manifest, nil, fmt.Errorf("archive entry %q has an invalid directory type", file.Name)
			}
			if file.Name == "data/" {
				continue
			}
			if !strings.HasPrefix(file.Name, "data/") {
				return manifest, nil, fmt.Errorf("unexpected archive directory %q", file.Name)
			}
			relativeDirectory := strings.TrimSuffix(strings.TrimPrefix(file.Name, "data/"), "/")
			if cleaned, cleanErr := cleanBackupRelative(relativeDirectory); cleanErr != nil || cleaned != relativeDirectory {
				return manifest, nil, fmt.Errorf("invalid archive directory %q", file.Name)
			}
			continue
		}
		if !file.Mode().IsRegular() {
			return manifest, nil, fmt.Errorf("archive entry %q is not a regular file", file.Name)
		}
		if !strings.HasPrefix(file.Name, "data/") {
			return manifest, nil, fmt.Errorf("unexpected archive entry %q", file.Name)
		}
		rel := strings.TrimPrefix(file.Name, "data/")
		cleaned, err := cleanBackupRelative(rel)
		if err != nil {
			return manifest, nil, err
		}
		expected, ok := manifest.Files[cleaned]
		if !ok {
			return manifest, nil, fmt.Errorf("file %q missing from manifest", cleaned)
		}
		if seen[cleaned] {
			return manifest, nil, fmt.Errorf("duplicate archive file %q", cleaned)
		}
		if file.UncompressedSize64 > uint64(maxRestoreBytes) {
			return manifest, nil, errors.New("expanded backup exceeds 20 GiB limit")
		}
		declaredTotal += int64(file.UncompressedSize64)
		if declaredTotal > maxRestoreBytes {
			return manifest, nil, errors.New("expanded backup exceeds 20 GiB limit")
		}
		if manifest.OS != runtime.GOOS && isNativePackageFile(cleaned) {
			in, openErr := file.Open()
			if openErr != nil {
				return manifest, nil, openErr
			}
			hash := sha256.New()
			written, copyErr := io.Copy(hash, io.LimitReader(in, maxRestoreBytes-actualTotal+1))
			in.Close()
			if copyErr != nil {
				return manifest, nil, copyErr
			}
			actualTotal += written
			if actualTotal > maxRestoreBytes || uint64(written) != file.UncompressedSize64 {
				return manifest, nil, errors.New("backup entry size does not match its ZIP header or exceeds the expanded limit")
			}
			if !strings.EqualFold(expected, hex.EncodeToString(hash.Sum(nil))) {
				return manifest, nil, fmt.Errorf("checksum mismatch for %q", cleaned)
			}
			seen[cleaned] = true
			skippedBinaries = append(skippedBinaries, cleaned)
			continue
		}
		in, err := file.Open()
		if err != nil {
			return manifest, nil, err
		}
		written, writeErr := writeFileWithinNoFollow(stagingDir, cleaned, in, secureWriteOptions{
			Mode: 0o640, ExpectedSize: int64(file.UncompressedSize64), ExpectedSHA256: expected, Replace: false,
		})
		closeErr := in.Close()
		if writeErr != nil {
			return manifest, nil, writeErr
		}
		if closeErr != nil {
			return manifest, nil, closeErr
		}
		actualTotal += written
		if actualTotal > maxRestoreBytes || uint64(written) != file.UncompressedSize64 {
			return manifest, nil, errors.New("backup entry size does not match its ZIP header or exceeds the expanded limit")
		}
		seen[cleaned] = true
		restored[cleaned] = true
	}
	if len(seen) != len(manifest.Files) {
		return manifest, nil, errors.New("backup is incomplete")
	}
	paths := make([]string, 0, len(restored))
	for path := range restored {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	if err := removeRestoreExtras(instancePath, restored, manifest.OS != runtime.GOOS); err != nil {
		return manifest, nil, err
	}
	for _, rel := range paths {
		source := filepath.Join(stagingDir, filepath.FromSlash(rel))
		if err := copyRegularFileToRootNoFollow(source, instancePath, rel, secureWriteOptions{
			Mode: 0o640, ExpectedSHA256: manifest.Files[rel], Replace: true, RejectExistingLink: true,
		}); err != nil {
			return manifest, nil, err
		}
	}
	return manifest, skippedBinaries, nil
}

func removeRestoreExtras(instancePath string, desired map[string]bool, preserveTargetNative bool) error {
	for _, root := range backupRoots {
		base, err := ResolveWithin(instancePath, filepath.FromSlash(root))
		if err != nil {
			return err
		}
		if _, err := statManagedDirectoryNoFollow(instancePath, root); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		directories := make([]string, 0)
		err = filepath.WalkDir(base, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			info, infoErr := entry.Info()
			if infoErr != nil {
				return infoErr
			}
			if isReparseOrSymlink(info) {
				return fmt.Errorf("restore refuses symlink %q in portable directories", path)
			}
			relative, err := filepath.Rel(instancePath, path)
			if err != nil {
				return err
			}
			relative = filepath.ToSlash(relative)
			if entry.IsDir() {
				if path != base {
					directories = append(directories, relative)
				}
				return nil
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("restore refuses non-regular file %q", path)
			}
			if desired[relative] || isGeneratedSecretConfig(relative) || isReservedMapAddonPath(relative) || (preserveTargetNative && isTargetPlatformNativeFile(relative)) {
				return nil
			}
			if err := deleteManagedFileNoFollow(instancePath, relative); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			return nil
		})
		if err != nil {
			return err
		}
		sort.Slice(directories, func(i, j int) bool { return len(directories[i]) > len(directories[j]) })
		for _, directory := range directories {
			if err := removeDirectoryWithinNoFollow(instancePath, directory); err != nil && !errors.Is(err, os.ErrNotExist) && !isDirectoryNotEmpty(err) {
				return err
			}
		}
	}
	return nil
}

func isTargetPlatformNativeFile(relative string) bool {
	return isNativeFileForOS(relative, runtime.GOOS)
}

func isNativeFileForOS(relative, goos string) bool {
	extension := strings.ToLower(filepath.Ext(relative))
	if goos == "windows" {
		return extension == ".dll"
	}
	if goos == "linux" {
		return extension == ".so"
	}
	return false
}

func isDirectoryNotEmpty(err error) bool {
	// Go does not expose a portable ENOTEMPTY sentinel on Windows. A failed
	// best-effort empty-directory cleanup is harmless; all file deletions above
	// are still exact. Treat common platform messages as the non-empty case.
	value := strings.ToLower(err.Error())
	return strings.Contains(value, "directory not empty") || strings.Contains(value, "directory is not empty") || strings.Contains(value, "not empty")
}

func cleanBackupRelative(rel string) (string, error) {
	cleaned := filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel)))
	if cleaned == "." || strings.HasPrefix(cleaned, "../") || cleaned == ".." || strings.HasPrefix(cleaned, "/") {
		return "", ErrPathEscape
	}
	for _, root := range backupRoots {
		if cleaned == root || strings.HasPrefix(cleaned, root+"/") {
			return cleaned, nil
		}
	}
	return "", errors.New("backup entry is outside portable directories")
}

func copyRegularFile(source, destination string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(destination), ".restore-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	_ = tmp.Chmod(0o640)
	if _, err = io.Copy(tmp, in); err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, destination)
}

func ApplyBackupRetention(dir, currentPath, kind string, dailyKeep, weeklyKeep int) ([]string, string, error) {
	dir = filepath.Clean(dir)
	currentPath = filepath.Clean(currentPath)
	if !isWithin(dir, currentPath) || filepath.Dir(currentPath) != dir {
		return nil, "", ErrPathEscape
	}
	weeklyPath := ""
	if kind == "daily" {
		year, week := time.Now().UTC().ISOWeek()
		weeklyPrefix := fmt.Sprintf("weekly-%04d-W%02d-", year, week)
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, "", err
		}
		exists := false
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasPrefix(entry.Name(), weeklyPrefix) && strings.HasSuffix(entry.Name(), ".zip") {
				exists = true
				break
			}
		}
		if !exists {
			weeklyName := weeklyPrefix + strings.TrimPrefix(filepath.Base(currentPath), "daily-")
			weeklyPath = filepath.Join(dir, weeklyName)
			if err := copyRegularFile(currentPath, weeklyPath); err != nil {
				return nil, "", err
			}
		}
	}
	pruned := make([]string, 0)
	if kind == "daily" || kind == "weekly" {
		removed, err := pruneBackupPrefix(dir, "daily-", dailyKeep)
		if err != nil {
			return pruned, weeklyPath, err
		}
		pruned = append(pruned, removed...)
		removed, err = pruneBackupPrefix(dir, "weekly-", weeklyKeep)
		if err != nil {
			return pruned, weeklyPath, err
		}
		pruned = append(pruned, removed...)
	} else if kind == "checkpoint" {
		removed, err := pruneBackupPrefix(dir, "checkpoint-", 3)
		if err != nil {
			return pruned, weeklyPath, err
		}
		pruned = append(pruned, removed...)
	}
	return pruned, weeklyPath, nil
}

func pruneBackupPrefix(dir, prefix string, keep int) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	type candidate struct {
		name     string
		modified time.Time
	}
	items := make([]candidate, 0)
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !strings.HasPrefix(entry.Name(), prefix) || !strings.HasSuffix(entry.Name(), ".zip") {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		items = append(items, candidate{name: entry.Name(), modified: info.ModTime()})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].modified.Equal(items[j].modified) {
			return items[i].name > items[j].name
		}
		return items[i].modified.After(items[j].modified)
	})
	if keep < 0 {
		keep = 0
	}
	if keep >= len(items) {
		return nil, nil
	}
	removed := make([]string, 0, len(items)-keep)
	for _, item := range items[keep:] {
		path, err := ResolveWithin(dir, item.name)
		if err != nil || filepath.Dir(path) != filepath.Clean(dir) {
			if err == nil {
				err = ErrPathEscape
			}
			return removed, err
		}
		if err := os.Remove(path); err != nil {
			return removed, err
		}
		removed = append(removed, path)
	}
	return removed, nil
}
