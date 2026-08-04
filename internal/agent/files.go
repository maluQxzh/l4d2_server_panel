package agent

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const maxManagedFileSize = 8 << 20

type ManagedFile struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	IsDir   bool      `json:"is_dir"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"modified_at"`
}

var managedFileRoots = []string{"cfg", "addons", "logs", "left4dead2/cfg", "left4dead2/addons", "left4dead2/logs"}

var blockedManagedFileExtensions = map[string]bool{
	".bat": true, ".cmd": true, ".com": true, ".dll": true, ".dylib": true,
	".exe": true, ".ps1": true, ".sh": true, ".smx": true, ".so": true,
	".vdf": true, ".vpk": true,
}

func validateManagedFileMutation(cleaned string) error {
	if isGeneratedSecretConfig(cleaned) {
		return errors.New("legacy l4d2panel.cfg may contain secrets and is not editable through file jobs")
	}
	if blockedManagedFileExtensions[strings.ToLower(filepath.Ext(cleaned))] {
		return errors.New("executable and load-bearing files must be managed through the package workflow")
	}
	return nil
}

func cleanManagedRelative(path string) (string, error) {
	path = filepath.ToSlash(filepath.Clean(filepath.FromSlash(strings.TrimSpace(path))))
	if path == "." {
		return "", nil
	}
	if strings.HasPrefix(path, "../") || path == ".." || strings.HasPrefix(path, "/") {
		return "", ErrPathEscape
	}
	for _, root := range managedFileRoots {
		if path == root || strings.HasPrefix(path, root+"/") {
			return path, nil
		}
	}
	return "", errors.New("path is outside the managed cfg, addons, and logs directories")
}

func ListManagedFiles(instancePath, relative string) ([]ManagedFile, error) {
	cleaned, err := cleanManagedRelative(relative)
	if err != nil {
		return nil, err
	}
	if cleaned == "" {
		result := make([]ManagedFile, 0, len(managedFileRoots))
		for _, root := range managedFileRoots {
			if st, err := statManagedDirectoryNoFollow(instancePath, root); err == nil && st.IsDir() {
				result = append(result, ManagedFile{Name: filepath.Base(filepath.FromSlash(root)), Path: root, IsDir: true, ModTime: st.ModTime()})
			}
		}
		return result, nil
	}
	result, err := listManagedDirectoryNoFollow(instancePath, cleaned)
	if err != nil {
		return nil, err
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].IsDir != result[j].IsDir {
			return result[i].IsDir
		}
		return strings.ToLower(result[i].Name) < strings.ToLower(result[j].Name)
	})
	return result, nil
}

func ReadManagedFile(instancePath, relative string) (string, int64, string, error) {
	cleaned, err := cleanManagedRelative(relative)
	if err != nil || cleaned == "" {
		return "", 0, "", errors.New("a managed file path is required")
	}
	if isGeneratedSecretConfig(cleaned) {
		return "", 0, "", errors.New("legacy l4d2panel.cfg may contain secrets and is not readable through file jobs")
	}
	f, err := openManagedFileNoFollow(instancePath, cleaned)
	if err != nil {
		return "", 0, "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", 0, "", err
	}
	if !st.Mode().IsRegular() || st.Size() > maxManagedFileSize {
		return "", 0, "", errors.New("file is not regular or exceeds the 8 MiB limit")
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxManagedFileSize+1))
	if err != nil {
		return "", 0, "", err
	}
	if len(raw) > maxManagedFileSize {
		return "", 0, "", errors.New("file exceeds the 8 MiB limit")
	}
	digest := sha256.Sum256(raw)
	return base64.StdEncoding.EncodeToString(raw), int64(len(raw)), hex.EncodeToString(digest[:]), nil
}

func WriteManagedFile(instancePath, relative, content, contentBase64 string) error {
	cleaned, err := cleanManagedRelative(relative)
	if err != nil || cleaned == "" {
		return errors.New("a managed file path is required")
	}
	if err := validateManagedFileMutation(cleaned); err != nil {
		return err
	}
	var raw []byte
	if contentBase64 != "" {
		raw, err = base64.StdEncoding.DecodeString(contentBase64)
	} else {
		raw = []byte(content)
	}
	if err != nil {
		return errors.New("invalid base64 file content")
	}
	if len(raw) > maxManagedFileSize {
		return errors.New("file exceeds the 8 MiB limit")
	}
	return writeManagedFileNoFollow(instancePath, cleaned, raw)
}

func DeleteManagedFile(instancePath, relative string) error {
	cleaned, err := cleanManagedRelative(relative)
	if err != nil || cleaned == "" {
		return errors.New("a managed file path is required")
	}
	if err := validateManagedFileMutation(cleaned); err != nil {
		return err
	}
	for _, root := range managedFileRoots {
		if cleaned == root {
			return errors.New("cannot delete a managed root directory")
		}
	}
	return deleteManagedFileNoFollow(instancePath, cleaned)
}

func managedPathComponents(cleaned string) ([]string, error) {
	if cleaned == "" || filepath.IsAbs(cleaned) || filepath.VolumeName(cleaned) != "" {
		return nil, ErrPathEscape
	}
	parts := strings.Split(filepath.ToSlash(cleaned), "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsRune(part, '\x00') {
			return nil, ErrPathEscape
		}
	}
	return parts, nil
}

func managedTempName() (string, error) {
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return ".managed-" + hex.EncodeToString(random[:]), nil
}

// secureWriteOptions describes an atomic, directory-handle-relative write.
// ExpectedSize and ExpectedSHA256 are verified before the temporary file is
// committed, so a changing package source cannot install unchecked bytes.
type secureWriteOptions struct {
	Mode               os.FileMode
	ExpectedSize       int64
	ExpectedSHA256     string
	Replace            bool
	RejectExistingLink bool
}

func managedRawWriteOptions(size int, mode os.FileMode) secureWriteOptions {
	return secureWriteOptions{Mode: mode, ExpectedSize: int64(size), Replace: true}
}

func copySecureContent(destination io.Writer, source io.Reader, options secureWriteOptions) (int64, error) {
	hash := sha256.New()
	writer := io.Writer(destination)
	if options.ExpectedSHA256 != "" {
		writer = io.MultiWriter(destination, hash)
	}
	reader := source
	if options.ExpectedSize >= 0 {
		reader = io.LimitReader(source, options.ExpectedSize+1)
	}
	written, err := io.Copy(writer, reader)
	if err != nil {
		return written, err
	}
	if options.ExpectedSize >= 0 && written != options.ExpectedSize {
		return written, fmt.Errorf("source size changed: expected %d bytes, copied %d", options.ExpectedSize, written)
	}
	if options.ExpectedSHA256 != "" && !strings.EqualFold(options.ExpectedSHA256, hex.EncodeToString(hash.Sum(nil))) {
		return written, errors.New("source SHA-256 changed before commit")
	}
	return written, nil
}

func copyRegularFileToRootNoFollow(source, destinationRoot, destinationRelative string, options secureWriteOptions) error {
	in, err := openManagedFileNoFollow(filepath.Dir(source), filepath.Base(source))
	if err != nil {
		return fmt.Errorf("open trusted source: %w", err)
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() < 0 {
		return errors.New("copy source is not a regular file")
	}
	options.ExpectedSize = info.Size()
	_, err = writeFileWithinNoFollow(destinationRoot, destinationRelative, in, options)
	return err
}

func readRegularFileWithinNoFollow(root, relative string, maximum int64) ([]byte, error) {
	file, err := openManagedFileNoFollow(root, relative)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maximum {
		return nil, errors.New("file is not a trusted regular file within the size limit")
	}
	raw, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > maximum {
		return nil, errors.New("file grew beyond the size limit while reading")
	}
	return raw, nil
}

func hashRegularFileWithinNoFollow(root, relative string, maximum int64) (string, int64, error) {
	file, err := openManagedFileNoFollow(root, relative)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", 0, err
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maximum {
		return "", 0, errors.New("file is not a trusted regular file within the size limit")
	}
	hash := sha256.New()
	size, err := io.Copy(hash, io.LimitReader(file, maximum+1))
	if err != nil {
		return "", 0, err
	}
	if size != info.Size() || size > maximum {
		return "", 0, errors.New("file changed size while hashing")
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}

func writeRawFileWithinNoFollow(root, relative string, raw []byte, mode os.FileMode, rejectExistingLink bool) error {
	options := managedRawWriteOptions(len(raw), mode)
	options.RejectExistingLink = rejectExistingLink
	_, err := writeFileWithinNoFollow(root, relative, bytes.NewReader(raw), options)
	return err
}
