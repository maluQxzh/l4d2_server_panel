package agent

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	maxPackageFiles    = 50000
	maxPackageExpanded = int64(2 << 30)
)

func InstallPackageArchive(instancePath, archivePath, stagingParent string, allowNative bool) ([]string, error) {
	if err := os.MkdirAll(stagingParent, 0o700); err != nil {
		return nil, err
	}
	staging, err := os.MkdirTemp(stagingParent, "package-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(staging)
	f, err := os.Open(archivePath)
	if err != nil {
		return nil, err
	}
	reader := bufio.NewReader(f)
	magic, _ := reader.Peek(4)
	f.Close()
	var files []string
	if len(magic) >= 2 && magic[0] == 'P' && magic[1] == 'K' {
		files, err = stageZIPArchive(archivePath, staging, allowNative)
	} else if len(magic) >= 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		files, err = stageTarGZArchive(archivePath, staging, allowNative)
	} else {
		return nil, errors.New("package must be a ZIP or tar.gz archive")
	}
	if err != nil {
		return nil, err
	}
	for _, rel := range files {
		source := filepath.Join(staging, filepath.FromSlash(rel))
		if err := copyRegularFileToRootNoFollow(source, instancePath, rel, secureWriteOptions{
			Mode: 0o640, Replace: true, RejectExistingLink: true,
		}); err != nil {
			return nil, err
		}
	}
	return files, nil
}

func stageZIPArchive(path, staging string, allowNative bool) ([]string, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	if len(zr.File) > maxPackageFiles {
		return nil, errors.New("package contains too many files")
	}
	files := make([]string, 0, len(zr.File))
	var expanded int64
	for _, entry := range zr.File {
		if entry.FileInfo().Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("package contains a symlink")
		}
		if entry.FileInfo().IsDir() {
			continue
		}
		rel, err := packageDestination(entry.Name, allowNative)
		if err != nil {
			return nil, err
		}
		expanded += int64(entry.UncompressedSize64)
		if expanded > maxPackageExpanded {
			return nil, errors.New("expanded package exceeds 2 GiB")
		}
		in, err := entry.Open()
		if err != nil {
			return nil, err
		}
		if err := stagePackageFile(staging, rel, in, int64(entry.UncompressedSize64)); err != nil {
			in.Close()
			return nil, err
		}
		in.Close()
		files = append(files, rel)
	}
	if len(files) == 0 {
		return nil, errors.New("package contains no installable files")
	}
	return files, nil
}

func stageTarGZArchive(path, staging string, allowNative bool) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(io.LimitReader(gz, maxPackageExpanded+1))
	files := make([]string, 0)
	var expanded int64
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if header.Typeflag == tar.TypeDir {
			continue
		}
		if header.Typeflag != tar.TypeReg {
			return nil, errors.New("package contains a link or non-regular entry")
		}
		if len(files) >= maxPackageFiles {
			return nil, errors.New("package contains too many files")
		}
		rel, err := packageDestination(header.Name, allowNative)
		if err != nil {
			return nil, err
		}
		if header.Size < 0 {
			return nil, errors.New("package contains an invalid file size")
		}
		expanded += header.Size
		if expanded > maxPackageExpanded {
			return nil, errors.New("expanded package exceeds 2 GiB")
		}
		if err := stagePackageFile(staging, rel, tr, header.Size); err != nil {
			return nil, err
		}
		files = append(files, rel)
	}
	if len(files) == 0 {
		return nil, errors.New("package contains no installable files")
	}
	return files, nil
}

func packageDestination(name string, allowNative bool) (string, error) {
	name = strings.TrimPrefix(filepath.ToSlash(filepath.Clean(filepath.FromSlash(name))), "./")
	if name == "." || strings.HasPrefix(name, "../") || strings.HasPrefix(name, "/") {
		return "", ErrPathEscape
	}
	var rel string
	switch {
	case strings.HasPrefix(name, "left4dead2/addons/") || strings.HasPrefix(name, "left4dead2/cfg/"):
		rel = name
	case strings.HasPrefix(name, "addons/") || strings.HasPrefix(name, "cfg/"):
		rel = "left4dead2/" + name
	default:
		return "", fmt.Errorf("package entry %q is outside addons/cfg", name)
	}
	if isNativePackageFile(rel) && !allowNative {
		return "", fmt.Errorf("native package file %q requires acknowledge_native_risk", name)
	}
	return rel, nil
}

func isNativePackageFile(path string) bool {
	lower := strings.ToLower(path)
	return strings.HasSuffix(lower, ".dll") || strings.HasSuffix(lower, ".so") || strings.HasSuffix(lower, ".dylib")
}

func stagePackageFile(staging, relative string, source io.Reader, expected int64) error {
	_, err := writeFileWithinNoFollow(staging, relative, source, secureWriteOptions{
		Mode: 0o640, ExpectedSize: expected, Replace: false,
	})
	return err
}
