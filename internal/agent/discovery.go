package agent

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	processinfo "github.com/shirou/gopsutil/v4/process"
)

const (
	defaultDiscoveryDepth      = 6
	maximumDiscoveryDepth      = 8
	defaultDiscoveryEntries    = 5000
	maximumDiscoveryEntries    = 20000
	maximumDiscoveryCandidates = 200
)

// InstallationCandidate is deliberately limited to paths below a configured
// instance root. Relative paths are returned to the control plane so a scan
// can never be used to enumerate arbitrary host paths.
type InstallationCandidate struct {
	RootIndex      int      `json:"root_index"`
	Path           string   `json:"path"`
	Name           string   `json:"name"`
	Executable     string   `json:"executable"`
	HasGameInfo    bool     `json:"has_gameinfo"`
	HasAppManifest bool     `json:"has_appmanifest"`
	Managed        bool     `json:"managed"`
	LikelyRunning  bool     `json:"likely_running"`
	PID            int32    `json:"pid,omitempty"`
	Evidence       []string `json:"evidence"`
}

type DiscoveryResult struct {
	Candidates         []InstallationCandidate `json:"candidates"`
	ScannedDirectories int                     `json:"scanned_directories"`
	SkippedLinks       int                     `json:"skipped_links"`
	Truncated          bool                    `json:"truncated"`
}

type discoveredProcess struct {
	PID   int32
	Paths []string
}

type discoveryDirectory struct {
	rootIndex int
	relative  string
	depth     int
}

// DiscoverInstallations performs a bounded, read-only breadth-first walk. It
// does not follow symbolic links or Windows reparse points, and re-validates
// every visited directory through PathGuard before reading it.
func DiscoverInstallations(ctx context.Context, guard *PathGuard, registry *Registry, maxDepth, maxEntries int) (DiscoveryResult, error) {
	if guard == nil {
		return DiscoveryResult{}, errors.New("path guard is required")
	}
	if maxDepth <= 0 {
		maxDepth = defaultDiscoveryDepth
	}
	if maxDepth > maximumDiscoveryDepth {
		maxDepth = maximumDiscoveryDepth
	}
	if maxEntries <= 0 {
		maxEntries = defaultDiscoveryEntries
	}
	if maxEntries > maximumDiscoveryEntries {
		maxEntries = maximumDiscoveryEntries
	}

	managed := make(map[string]struct{})
	if registry != nil {
		for _, record := range registry.List() {
			path, err := guard.Resolve(record.RootIndex, record.RelativePath)
			if err == nil {
				managed[canonicalProcessPath(path)] = struct{}{}
			}
		}
	}
	processes := activeSrcdsProcesses(ctx)
	queue := make([]discoveryDirectory, 0, len(guard.Roots()))
	for rootIndex := range guard.Roots() {
		queue = append(queue, discoveryDirectory{rootIndex: rootIndex, relative: "."})
	}
	result := DiscoveryResult{Candidates: make([]InstallationCandidate, 0)}
	examinedEntries := 0

	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if len(result.Candidates) >= maximumDiscoveryCandidates {
			result.Truncated = true
			break
		}
		current := queue[0]
		queue = queue[1:]
		directory, err := guard.Resolve(current.rootIndex, current.relative)
		if err != nil {
			continue
		}
		info, err := os.Lstat(directory)
		if err != nil || !info.IsDir() || isReparseOrSymlink(info) {
			if err == nil && isReparseOrSymlink(info) {
				result.SkippedLinks++
			}
			continue
		}
		result.ScannedDirectories++
		candidate, found := inspectInstallationCandidate(guard.Roots()[current.rootIndex], current.rootIndex, current.relative, directory, managed, processes)
		if found {
			result.Candidates = append(result.Candidates, candidate)
			// A game directory contains tens of thousands of files but cannot
			// contain another independent installation in this layout.
			continue
		}
		if current.depth >= maxDepth {
			continue
		}
		remaining := maxEntries - examinedEntries
		if remaining <= 0 {
			result.Truncated = true
			continue
		}
		entries, more, err := readDiscoveryEntries(directory, remaining)
		if err != nil {
			continue
		}
		examinedEntries += len(entries)
		if more {
			result.Truncated = true
		}
		for _, entry := range entries {
			if discoveryExcludedDirectory(entry.Name()) {
				continue
			}
			entryPath := filepath.Join(directory, entry.Name())
			entryInfo, statErr := os.Lstat(entryPath)
			if statErr != nil {
				continue
			}
			if isReparseOrSymlink(entryInfo) {
				result.SkippedLinks++
				continue
			}
			if !entryInfo.IsDir() {
				continue
			}
			relative := filepath.Join(current.relative, entry.Name())
			if _, resolveErr := guard.Resolve(current.rootIndex, relative); resolveErr != nil {
				continue
			}
			queue = append(queue, discoveryDirectory{rootIndex: current.rootIndex, relative: relative, depth: current.depth + 1})
		}
	}
	sort.Slice(result.Candidates, func(i, j int) bool {
		if result.Candidates[i].RootIndex != result.Candidates[j].RootIndex {
			return result.Candidates[i].RootIndex < result.Candidates[j].RootIndex
		}
		return result.Candidates[i].Path < result.Candidates[j].Path
	})
	return result, nil
}

func readDiscoveryEntries(directory string, limit int) ([]os.DirEntry, bool, error) {
	file, err := os.Open(directory)
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	entries, err := file.ReadDir(limit + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, err
	}
	more := len(entries) > limit
	if more {
		entries = entries[:limit]
	}
	return entries, more, nil
}

func discoveryExcludedDirectory(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case ".quarantine", ".staging", "$recycle.bin", "system volume information":
		return true
	default:
		return false
	}
}

func inspectInstallationCandidate(root string, rootIndex int, relative, directory string, managed map[string]struct{}, processes []discoveredProcess) (InstallationCandidate, bool) {
	executable := ""
	for _, name := range []string{"srcds.exe", "srcds_run", "srcds_linux"} {
		path := filepath.Join(directory, name)
		if info, err := os.Lstat(path); err == nil && !info.IsDir() && info.Mode().IsRegular() && !isReparseOrSymlink(info) {
			executable = name
			break
		}
	}
	if executable == "" || !safeDiscoveryDirectory(filepath.Join(directory, "left4dead2")) {
		return InstallationCandidate{}, false
	}
	gameInfo := safeDiscoveryFile(filepath.Join(directory, "left4dead2", "gameinfo.txt"))
	manifest := installationManifestExists(root, directory)
	cleanRelative := filepath.Clean(relative)
	if cleanRelative == "" {
		cleanRelative = "."
	}
	name := filepath.Base(directory)
	if name == "." || name == string(filepath.Separator) {
		name = "L4D2 Dedicated Server"
	}
	candidate := InstallationCandidate{
		RootIndex: rootIndex, Path: filepath.ToSlash(cleanRelative), Name: name, Executable: executable,
		HasGameInfo: gameInfo, HasAppManifest: manifest,
		Evidence: []string{executable, "left4dead2/"},
	}
	if gameInfo {
		candidate.Evidence = append(candidate.Evidence, "left4dead2/gameinfo.txt")
	}
	if manifest {
		candidate.Evidence = append(candidate.Evidence, "appmanifest_222860.acf")
	}
	_, candidate.Managed = managed[canonicalProcessPath(directory)]
	if process, ok := installationProcessForDirectory(directory, processes); ok {
		candidate.LikelyRunning, candidate.PID = true, process.PID
	}
	return candidate, true
}

func installationManifestExists(root, directory string) bool {
	paths := []string{
		filepath.Join(directory, "appmanifest_222860.acf"),
		filepath.Join(directory, "steamapps", "appmanifest_222860.acf"),
	}
	parent := filepath.Dir(directory)
	if strings.EqualFold(filepath.Base(parent), "common") {
		paths = append(paths, filepath.Join(filepath.Dir(parent), "appmanifest_222860.acf"))
	}
	for _, path := range paths {
		if isWithin(root, path) && safeDiscoveryFile(path) {
			return true
		}
	}
	return false
}

func safeDiscoveryDirectory(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.IsDir() && !isReparseOrSymlink(info)
}

func safeDiscoveryFile(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular() && !isReparseOrSymlink(info)
}

func isSrcdsExecutableName(name string) bool {
	switch strings.ToLower(filepath.Base(strings.TrimSpace(name))) {
	case "srcds.exe", "srcds_run", "srcds_linux":
		return true
	default:
		return false
	}
}

func canonicalProcessPath(path string) string {
	path = strings.Trim(strings.TrimSpace(path), `"'`)
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	if filepath.Separator == '\\' {
		abs = strings.ToLower(abs)
	}
	return filepath.Clean(abs)
}

func activeSrcdsProcesses(ctx context.Context) []discoveredProcess {
	processes, err := processinfo.ProcessesWithContext(ctx)
	if err != nil {
		return nil
	}
	result := make([]discoveredProcess, 0)
	for _, process := range processes {
		paths := processExecutablePaths(ctx, process)
		if len(paths) > 0 {
			result = append(result, discoveredProcess{PID: process.Pid, Paths: paths})
		}
	}
	return result
}

func processExecutablePaths(ctx context.Context, process *processinfo.Process) []string {
	seen := make(map[string]struct{})
	result := make([]string, 0, 2)
	add := func(path string) {
		if !isSrcdsExecutableName(path) {
			return
		}
		path = canonicalProcessPath(path)
		if path == "" {
			return
		}
		if _, ok := seen[path]; !ok {
			seen[path] = struct{}{}
			result = append(result, path)
		}
	}
	if executable, err := process.ExeWithContext(ctx); err == nil {
		add(executable)
	}
	cwd, _ := process.CwdWithContext(ctx)
	if args, err := process.CmdlineSliceWithContext(ctx); err == nil {
		for _, argument := range args {
			argument = strings.Trim(strings.TrimSpace(argument), `"'`)
			if !isSrcdsExecutableName(argument) {
				continue
			}
			if !filepath.IsAbs(argument) && cwd != "" {
				argument = filepath.Join(cwd, argument)
			}
			add(argument)
		}
	}
	return result
}

func installationProcessForDirectory(directory string, processes []discoveredProcess) (discoveredProcess, bool) {
	directory = canonicalProcessPath(directory)
	for _, preferredName := range []string{"srcds.exe", "srcds_linux", "srcds_run"} {
		for _, process := range processes {
			for _, path := range process.Paths {
				if strings.EqualFold(filepath.Base(path), preferredName) && isWithin(directory, path) {
					return process, true
				}
			}
		}
	}
	return discoveredProcess{}, false
}

func externalProcessMatches(ctx context.Context, pid int32, directory string) bool {
	if pid <= 0 {
		return false
	}
	process, err := processinfo.NewProcess(pid)
	if err != nil {
		return false
	}
	_, matches := installationProcessForDirectory(directory, []discoveredProcess{{PID: pid, Paths: processExecutablePaths(ctx, process)}})
	return matches
}

func externalPIDStillRunning(ctx context.Context, pid int32) bool {
	if pid <= 0 {
		return false
	}
	process, err := processinfo.NewProcess(pid)
	if err != nil {
		return false
	}
	running, err := process.IsRunningWithContext(ctx)
	if err != nil {
		// Failure to inspect a privileged process is not proof it exited.
		return true
	}
	return running
}

func terminateExternalProcessTree(ctx context.Context, pid int32, directory string, timeout time.Duration) error {
	if !externalProcessMatches(ctx, pid, directory) {
		return nil
	}
	root, err := processinfo.NewProcess(pid)
	if err != nil {
		return nil
	}
	children, _ := root.ChildrenWithContext(ctx)
	processes := append([]*processinfo.Process{root}, children...)
	for _, process := range processes {
		_ = process.TerminateWithContext(ctx)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !externalProcessMatches(ctx, pid, directory) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	// Re-check the PID/path pair immediately before forcing termination to
	// avoid acting on a recycled PID.
	if !externalProcessMatches(ctx, pid, directory) {
		return nil
	}
	for index := len(processes) - 1; index >= 0; index-- {
		_ = processes[index].KillWithContext(ctx)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !externalProcessMatches(ctx, pid, directory) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return errors.New("external server process did not exit")
}
