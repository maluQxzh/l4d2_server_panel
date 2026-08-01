package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

var ErrPathEscape = errors.New("path escapes an approved root")

type PathGuard struct {
	roots []string
}

func NewPathGuard(roots []string) (*PathGuard, error) {
	if len(roots) == 0 {
		return nil, errors.New("at least one instance root is required")
	}
	g := &PathGuard{roots: make([]string, 0, len(roots))}
	for _, root := range roots {
		abs, err := filepath.Abs(filepath.Clean(root))
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(abs, 0o750); err != nil {
			return nil, fmt.Errorf("create root %q: %w", abs, err)
		}
		canonical, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return nil, fmt.Errorf("resolve root %q: %w", abs, err)
		}
		g.roots = append(g.roots, filepath.Clean(canonical))
	}
	return g, nil
}

func (g *PathGuard) Roots() []string { return append([]string(nil), g.roots...) }

// Resolve resolves a relative instance path below the selected configured root.
// Existing symlinks/reparse points are evaluated, including the closest existing
// ancestor for paths that are about to be created.
func (g *PathGuard) Resolve(rootIndex int, relative string) (string, error) {
	if rootIndex < 0 || rootIndex >= len(g.roots) {
		return "", errors.New("invalid root index")
	}
	if relative == "" || filepath.IsAbs(relative) || filepath.VolumeName(relative) != "" {
		return "", ErrPathEscape
	}
	return resolveBelow(g.roots[rootIndex], relative)
}

func (g *PathGuard) ResolveAbsolute(path string) (string, int, error) {
	if !filepath.IsAbs(path) {
		return "", -1, errors.New("absolute path required")
	}
	for i, root := range g.roots {
		resolved, err := resolveBelow(root, path)
		if err == nil {
			return resolved, i, nil
		}
	}
	return "", -1, ErrPathEscape
}

func ResolveWithin(base, relative string) (string, error) {
	if relative == "" {
		return filepath.Clean(base), nil
	}
	if filepath.IsAbs(relative) || filepath.VolumeName(relative) != "" {
		return "", ErrPathEscape
	}
	return resolveBelow(base, relative)
}

// ensureDataSubdirectory creates a fixed Agent-owned directory and then
// resolves it back through dataDir. The second step rejects a symlink or
// Windows reparse point that redirects the directory outside dataDir.
func ensureDataSubdirectory(dataDir, name string) (string, error) {
	if name == "" || filepath.Base(name) != name || strings.ContainsAny(name, "\x00/\\") {
		return "", ErrPathEscape
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", err
	}
	candidate := filepath.Join(dataDir, name)
	if err := os.MkdirAll(candidate, 0o700); err != nil {
		return "", err
	}
	return resolveBelow(dataDir, name)
}

func resolveBelow(base, value string) (string, error) {
	baseAbs, err := filepath.Abs(filepath.Clean(base))
	if err != nil {
		return "", err
	}
	candidate := value
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(baseAbs, candidate)
	}
	candidate, err = filepath.Abs(filepath.Clean(candidate))
	if err != nil {
		return "", err
	}
	if !isWithin(baseAbs, candidate) {
		return "", ErrPathEscape
	}

	// Resolve the closest existing ancestor, then append the still-missing tail.
	ancestor := candidate
	var tail []string
	for {
		_, statErr := os.Lstat(ancestor)
		if statErr == nil {
			break
		}
		if !os.IsNotExist(statErr) {
			return "", statErr
		}
		parent := filepath.Dir(ancestor)
		if samePath(parent, ancestor) {
			return "", statErr
		}
		tail = append(tail, filepath.Base(ancestor))
		ancestor = parent
	}
	resolvedAncestor, err := filepath.EvalSymlinks(ancestor)
	if err != nil {
		return "", err
	}
	resolved := resolvedAncestor
	for i := len(tail) - 1; i >= 0; i-- {
		resolved = filepath.Join(resolved, tail[i])
	}
	baseCanonical, err := filepath.EvalSymlinks(baseAbs)
	if err != nil {
		return "", err
	}
	if !isWithin(baseCanonical, resolved) {
		return "", ErrPathEscape
	}
	return filepath.Clean(resolved), nil
}

func isWithin(base, candidate string) bool {
	rel, err := filepath.Rel(base, candidate)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func samePath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
	}
	return filepath.Clean(a) == filepath.Clean(b)
}
