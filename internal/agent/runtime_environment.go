package agent

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const maxSteamCMDArchiveExpanded = int64(256 << 20)

type runtimeEnvironmentState struct {
	SteamCMDPath   string                       `json:"steamcmd_path,omitempty"`
	SteamCMDSource string                       `json:"steamcmd_source,omitempty"`
	Installations  map[string]runtimeGameRecord `json:"installations,omitempty"`
}

type runtimeGameRecord struct {
	Path string `json:"path"`
}

type runtimeEnvironmentStore struct {
	mu    sync.Mutex
	path  string
	state runtimeEnvironmentState
}

func newRuntimeEnvironmentStore(config *Config) (*runtimeEnvironmentStore, error) {
	if err := os.MkdirAll(config.DataDir, 0o700); err != nil {
		return nil, err
	}
	store := &runtimeEnvironmentStore{path: filepath.Join(config.DataDir, "runtime-environment.json")}
	raw, err := os.ReadFile(store.path)
	if err == nil {
		if err := json.Unmarshal(raw, &store.state); err != nil {
			return nil, fmt.Errorf("read runtime environment state: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	} else if strings.TrimSpace(config.SteamCMDPath) != "" {
		// Preserve the configured launcher for upgraded Agents. It is only
		// projected to Control after an explicit environment inspection.
		store.state.SteamCMDPath = config.SteamCMDPath
		store.state.SteamCMDSource = "custom"
	}
	if store.state.Installations == nil {
		store.state.Installations = make(map[string]runtimeGameRecord)
	}
	prunedLegacyPaths := false
	for id, record := range store.state.Installations {
		if !filepath.IsAbs(record.Path) {
			delete(store.state.Installations, id)
			prunedLegacyPaths = true
		}
	}
	if prunedLegacyPaths {
		if err := store.update(func(*runtimeEnvironmentState) {}); err != nil {
			return nil, fmt.Errorf("remove legacy relative game installation paths: %w", err)
		}
	}
	return store, nil
}

func (s *runtimeEnvironmentStore) snapshot() runtimeEnvironmentState {
	s.mu.Lock()
	defer s.mu.Unlock()
	copyState := s.state
	copyState.Installations = make(map[string]runtimeGameRecord, len(s.state.Installations))
	for id, record := range s.state.Installations {
		copyState.Installations[id] = record
	}
	return copyState
}

func (s *runtimeEnvironmentStore) update(change func(*runtimeEnvironmentState)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := s.state
	previous.Installations = make(map[string]runtimeGameRecord, len(s.state.Installations))
	for id, record := range s.state.Installations {
		previous.Installations[id] = record
	}
	change(&s.state)
	raw, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		s.state = previous
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".runtime-environment-*")
	if err != nil {
		s.state = previous
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err == nil {
		_, err = tmp.Write(raw)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		s.state = previous
		return err
	}
	if err := replaceJournalFile(name, s.path); err != nil {
		s.state = previous
		return err
	}
	return nil
}

func validateSteamCMDPath(ctx context.Context, path string, roots []string) (string, error) {
	path = filepath.Clean(strings.TrimSpace(path))
	if !filepath.IsAbs(path) {
		return "", errors.New("SteamCMD path must be absolute")
	}
	base := strings.ToLower(filepath.Base(path))
	if runtime.GOOS == "windows" {
		if base != "steamcmd.exe" {
			return "", errors.New("Windows SteamCMD path must end with steamcmd.exe")
		}
	} else if base != "steamcmd.sh" && base != "steamcmd" {
		return "", errors.New("Linux SteamCMD path must end with steamcmd.sh or steamcmd")
	}
	for _, root := range roots {
		if isWithin(root, path) {
			return "", errors.New("SteamCMD path must not be inside a configured game root")
		}
	}
	if err := rejectLinkedPath(path); err != nil {
		return "", err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("SteamCMD not found: %w", err)
	}
	if !info.Mode().IsRegular() || isReparseOrSymlink(info) {
		return "", errors.New("SteamCMD path is not a plain file")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		return "", errors.New("SteamCMD launcher is not executable by the Agent account")
	}
	probeCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, path, "+quit")
	cmd.Dir = filepath.Dir(path)
	cmd.Stdin = nil
	cmd.Env = restrictedSteamEnvironment(filepath.Dir(path))
	output, err := cmd.CombinedOutput()
	if len(output) > 2<<20 {
		output = output[len(output)-(2<<20):]
	}
	if err != nil {
		return "", fmt.Errorf("SteamCMD self-test failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return "available", nil
}

func rejectLinkedPath(path string) error {
	volume := filepath.VolumeName(path)
	current := volume + string(filepath.Separator)
	remainder := strings.TrimPrefix(path, current)
	for _, component := range strings.Split(remainder, string(filepath.Separator)) {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if isReparseOrSymlink(info) {
			return errors.New("SteamCMD path contains a link or reparse point")
		}
	}
	return nil
}

// rejectLinkedAncestors also works for a not-yet-created installation path:
// every existing component up to the configured root must still be a plain
// directory. This is re-run immediately before every environment task.
func rejectLinkedAncestors(path string) error {
	current := filepath.Clean(path)
	for {
		if _, err := os.Lstat(current); err == nil {
			return rejectLinkedPath(current)
		} else if !os.IsNotExist(err) {
			return err
		}
		parent := filepath.Dir(current)
		if samePath(parent, current) {
			return errors.New("game installation has no existing safe ancestor")
		}
		current = parent
	}
}

func restrictedSteamEnvironment(home string) []string {
	allowed := []string{"PATH", "SystemRoot", "WINDIR", "TEMP", "TMP", "LANG", "LC_ALL"}
	result := make([]string, 0, len(allowed))
	for _, name := range allowed {
		if value, ok := os.LookupEnv(name); ok {
			result = append(result, name+"="+value)
		}
	}
	if home != "" {
		result = append(result, "HOME="+home, "USERPROFILE="+home)
	}
	return result
}

func systemDependencyStatus() map[string]any {
	if runtime.GOOS == "windows" {
		paths := []string{filepath.Join(os.Getenv("WINDIR"), "SysWOW64", "vcruntime140.dll"), filepath.Join(os.Getenv("WINDIR"), "SysWOW64", "msvcp140.dll")}
		ready := true
		for _, path := range paths {
			if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
				ready = false
			}
		}
		return map[string]any{"status": map[bool]string{true: "ready", false: "missing"}[ready], "vc_runtime_x86": ready}
	}
	paths := []string{"/lib/i386-linux-gnu/libgcc_s.so.1", "/usr/lib/i386-linux-gnu/libstdc++.so.6"}
	ready := true
	for _, path := range paths {
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			ready = false
		}
	}
	return map[string]any{"status": map[bool]string{true: "ready", false: "missing"}[ready], "lib32_runtime": ready}
}

func installManagedSteamCMD(ctx context.Context, config *Config, roots []string) (string, string, error) {
	archiveURL := "https://steamcdn-a.akamaihd.net/client/installer/steamcmd_linux.tar.gz"
	archiveName := "steamcmd.tar.gz"
	launcher := "steamcmd.sh"
	if runtime.GOOS == "windows" {
		archiveURL, archiveName, launcher = "https://steamcdn-a.akamaihd.net/client/installer/steamcmd.zip", "steamcmd.zip", "steamcmd.exe"
	}
	staging, err := os.MkdirTemp(config.DataDir, ".steamcmd-stage-")
	if err != nil {
		return "", "", err
	}
	defer os.RemoveAll(staging)
	archivePath := filepath.Join(staging, archiveName)
	if _, _, err := DownloadOfficialHTTPS(ctx, archiveURL, archivePath); err != nil {
		return "", "", err
	}
	extractDir := filepath.Join(staging, "content")
	if err := os.Mkdir(extractDir, 0o700); err != nil {
		return "", "", err
	}
	if runtime.GOOS == "windows" {
		err = extractSteamZIP(archivePath, extractDir)
	} else {
		err = extractSteamTarGZ(archivePath, extractDir)
	}
	if err != nil {
		return "", "", err
	}
	stagedLauncher := filepath.Join(extractDir, launcher)
	if runtime.GOOS != "windows" {
		if err := os.Chmod(stagedLauncher, 0o700); err != nil {
			return "", "", err
		}
	}
	if _, err := validateSteamCMDPath(ctx, stagedLauncher, roots); err != nil {
		return "", "", err
	}
	destination := filepath.Join(config.DataDir, "steamcmd")
	backup := filepath.Join(config.DataDir, ".steamcmd-backup")
	_ = os.RemoveAll(backup)
	if _, statErr := os.Lstat(destination); statErr == nil {
		if err := os.Rename(destination, backup); err != nil {
			return "", "", err
		}
	} else if !os.IsNotExist(statErr) {
		return "", "", statErr
	}
	if err := os.Rename(extractDir, destination); err != nil {
		_ = os.Rename(backup, destination)
		return "", "", err
	}
	finalLauncher := filepath.Join(destination, launcher)
	version, err := validateSteamCMDPath(ctx, finalLauncher, roots)
	if err != nil {
		_ = os.RemoveAll(destination)
		_ = os.Rename(backup, destination)
		return "", "", err
	}
	_ = os.RemoveAll(backup)
	return finalLauncher, version, nil
}

func extractSteamZIP(path, destination string) error {
	reader, err := zip.OpenReader(path)
	if err != nil {
		return err
	}
	defer reader.Close()
	if len(reader.File) > 1024 {
		return errors.New("SteamCMD archive contains too many entries")
	}
	var expanded int64
	for _, entry := range reader.File {
		name := filepath.FromSlash(entry.Name)
		target, err := ResolveWithin(destination, name)
		if err != nil || entry.Mode()&os.ModeSymlink != 0 {
			return errors.New("SteamCMD archive contains an unsafe path")
		}
		if entry.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
			continue
		}
		expanded += int64(entry.UncompressedSize64)
		if expanded > maxSteamCMDArchiveExpanded {
			return errors.New("SteamCMD archive is too large")
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		source, err := entry.Open()
		if err != nil {
			return err
		}
		file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, err = io.Copy(file, io.LimitReader(source, maxSteamCMDArchiveExpanded+1))
		}
		if closeErr := source.Close(); err == nil {
			err = closeErr
		}
		if file != nil {
			if closeErr := file.Close(); err == nil {
				err = closeErr
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func extractSteamTarGZ(path, destination string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	var expanded int64
	for entries := 0; ; entries++ {
		if entries > 1024 {
			return errors.New("SteamCMD archive contains too many entries")
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		target, resolveErr := ResolveWithin(destination, filepath.FromSlash(header.Name))
		if resolveErr != nil || (header.Typeflag != tar.TypeDir && header.Typeflag != tar.TypeReg) {
			return errors.New("SteamCMD archive contains an unsafe path")
		}
		if header.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
			continue
		}
		expanded += header.Size
		if expanded > maxSteamCMDArchiveExpanded {
			return errors.New("SteamCMD archive is too large")
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, io.LimitReader(reader, header.Size))
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}
