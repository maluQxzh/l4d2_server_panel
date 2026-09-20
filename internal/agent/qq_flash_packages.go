package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	maxQQPackageShareEntries = 10000
	maxQQPackageDownload     = int64(2 << 30)
	maxQQPackageExpanded     = int64(2 << 30)
	maxQQPackageVPKs         = 1000
	qqPackageDiskReserve     = uint64(1 << 30)
)

type qqPackageVPK struct {
	Path    string
	SortKey string
}

func importQQFlashPackages(ctx context.Context, instancePath, stagingParent, shareURL, jobID string, progress ProgressFunc,
	limiters ...*downloadConcurrencyLimiter,
) (map[string]any, error) {
	validated, cleanup, err := prepareQQFlashPackages(ctx, stagingParent, shareURL, jobID, progress, newQQFlashClient(limiters...))
	if err != nil {
		return nil, err
	}
	defer cleanup()
	return commitQQFlashPackages(ctx, instancePath, validated, progress)
}

type validatedQQVPK struct {
	input   qqPackageVPK
	name    string
	path    string
	version int
	sha256  string
	size    int64
}

func prepareQQFlashPackages(ctx context.Context, stagingParent, shareURL, jobID string, progress ProgressFunc, client *qqFlashClient) ([]validatedQQVPK, func(), error) {
	progress(3, "reading QQ flash package metadata")
	listed, deviceID, _, entries, err := client.listAllFiles(ctx, shareURL, maxQQPackageShareEntries, maxQQPackageDownload)
	if err != nil {
		return nil, nil, err
	}
	if len(listed) == 0 {
		return nil, nil, errors.New("QQ flash share contains no files")
	}
	var downloadBytes int64
	for _, item := range listed {
		if len(item.Relative) > 1024 {
			return nil, nil, errors.New("QQ flash package path exceeds the supported length")
		}
		if item.File.Size < 0 || downloadBytes > maxQQPackageDownload-item.File.Size {
			return nil, nil, errors.New("QQ flash share exceeds the package download limit")
		}
		downloadBytes += item.File.Size
	}
	staging, err := os.MkdirTemp(stagingParent, "qq-vpk-import-*")
	if err != nil {
		return nil, nil, fmt.Errorf("create QQ flash package staging directory: %w", err)
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(staging)
		}
	}()
	if err := rejectLinkedAncestors(staging); err != nil {
		return nil, nil, err
	}
	if err := ensureArchiveDiskReserve(staging, downloadBytes, qqPackageDiskReserve, "QQ flash package"); err != nil {
		return nil, nil, err
	}

	state := &mapArchiveState{
		Root: staging, Entries: entries, MaxEntries: maxQQPackageShareEntries,
		MaxEntrySize: maxPackageDownload, MaxExpanded: maxQQPackageExpanded,
		DiskFreeReserve: qqPackageDiskReserve, Subject: "QQ flash package",
	}
	inputs := make([]qqPackageVPK, 0)
	for index, item := range listed {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		relative := filepath.ToSlash(filepath.Join("downloads", filepath.FromSlash(item.Relative)))
		progress(5+index*35/max(1, len(listed)), "downloading QQ flash package files")
		if err := client.downloadFile(ctx, staging, relative, item.File, deviceID, nil); err != nil {
			return nil, nil, fmt.Errorf("download %s: %w", item.Relative, err)
		}
		path, err := ResolveWithin(staging, filepath.FromSlash(relative))
		if err != nil {
			return nil, nil, err
		}
		found, err := discoverQQPackageVPKs(ctx, staging, path, item.Relative, 0, state)
		if err != nil {
			return nil, nil, fmt.Errorf("process %s: %w", item.Relative, err)
		}
		inputs = append(inputs, found...)
		if len(inputs) > maxQQPackageVPKs {
			return nil, nil, errors.New("QQ flash share contains too many VPK packages")
		}
	}
	if len(inputs) == 0 {
		return nil, nil, errors.New("QQ flash share contains no VPK packages")
	}
	sort.Slice(inputs, func(i, j int) bool {
		left, right := strings.ToLower(filepath.ToSlash(inputs[i].SortKey)), strings.ToLower(filepath.ToSlash(inputs[j].SortKey))
		if left == right {
			return inputs[i].SortKey < inputs[j].SortKey
		}
		return left < right
	})

	validated := make([]validatedQQVPK, 0, len(inputs))
	prefix := qqPackageTargetPrefix(jobID)
	for index, input := range inputs {
		progress(42+index*28/max(1, len(inputs)), "validating QQ flash VPK packages")
		version, sha256, err := validateVPK(input.Path)
		if err != nil {
			return nil, nil, fmt.Errorf("validate %s: %w", input.SortKey, err)
		}
		verifiedSHA, size, err := hashRegularFileWithinNoFollow(filepath.Dir(input.Path), filepath.Base(input.Path), maxPackageDownload)
		if err != nil {
			return nil, nil, err
		}
		if verifiedSHA != sha256 {
			return nil, nil, errors.New("VPK content changed during preparation")
		}
		name, err := qqPackageDisplayName(input.SortKey)
		if err != nil {
			return nil, nil, fmt.Errorf("name %s: %w", input.SortKey, err)
		}
		targetName := fmt.Sprintf("%s_%04d_%s", prefix, index+1, name)
		target := filepath.ToSlash(filepath.Join("left4dead2", "addons", targetName))
		if isReservedMapAddonBase(targetName) {
			return nil, nil, errors.New("reserved map-library VPK names cannot be imported as ordinary packages")
		}
		validated = append(validated, validatedQQVPK{input: input, name: name, path: target, version: version, sha256: sha256, size: size})
	}

	keep = true
	return validated, func() { _ = os.RemoveAll(staging) }, nil
}

func commitQQFlashPackages(ctx context.Context, instancePath string, validated []validatedQQVPK, progress ProgressFunc) (map[string]any, error) {
	created := make([]string, 0, len(validated))
	rollback := func() {
		for index := len(created) - 1; index >= 0; index-- {
			_ = deleteManagedFileNoFollow(instancePath, created[index])
		}
	}
	packages := make([]map[string]any, 0, len(validated))
	for index, item := range validated {
		if err := ctx.Err(); err != nil {
			rollback()
			return nil, err
		}
		progress(72+index*26/max(1, len(validated)), "installing QQ flash VPK packages")
		alreadyPresent, err := commitQQPackageVPK(item.input.Path, instancePath, item.path, item.sha256, item.size)
		if err != nil {
			rollback()
			return nil, err
		}
		if !alreadyPresent {
			created = append(created, item.path)
		}
		packages = append(packages, map[string]any{
			"name": item.name, "kind": "vpk", "path": item.path, "sha256": item.sha256,
			"size": item.size, "vpk_version": item.version,
		})
	}
	progress(100, "QQ flash VPK packages installed")
	return map[string]any{"kind": "qq_flash_vpk", "packages": packages, "files_imported": len(packages)}, nil
}

func discoverQQPackageVPKs(ctx context.Context, stagingRoot, filePath, sortKey string, depth int,
	state *mapArchiveState,
) ([]qqPackageVPK, error) {
	kind, err := detectMapFileKind(filePath)
	if err != nil {
		return nil, err
	}
	switch kind {
	case mapFileVPK:
		if !strings.EqualFold(filepath.Ext(filePath), ".vpk") {
			return nil, errors.New("VPK content must use the .vpk file extension")
		}
		return []qqPackageVPK{{Path: filePath, SortKey: filepath.ToSlash(sortKey)}}, nil
	case mapFileZIP, mapFile7Z, mapFileRAR:
		if depth >= maxMapArchiveDepth {
			return nil, errors.New("nested archive depth exceeds three levels")
		}
		destination, err := os.MkdirTemp(filepath.Join(stagingRoot, "expanded"), "archive-*")
		if errors.Is(err, os.ErrNotExist) {
			if err = os.MkdirAll(filepath.Join(stagingRoot, "expanded"), 0o700); err == nil {
				destination, err = os.MkdirTemp(filepath.Join(stagingRoot, "expanded"), "archive-*")
			}
		}
		if err != nil {
			return nil, err
		}
		files, err := extractMapArchive(ctx, filePath, destination, kind, state)
		if err != nil {
			return nil, err
		}
		result := make([]qqPackageVPK, 0)
		for _, extracted := range files {
			childKey := filepath.ToSlash(sortKey) + "!/" + filepath.ToSlash(extracted.Relative)
			found, err := discoverQQPackageVPKs(ctx, stagingRoot, extracted.Path, childKey, depth+1, state)
			if err != nil {
				return nil, err
			}
			result = append(result, found...)
		}
		return result, nil
	default:
		return nil, errors.New("only VPK files and ZIP, 7Z, or RAR archives containing VPK files are accepted")
	}
}

func qqPackageDisplayName(sortKey string) (string, error) {
	base := filepath.Base(filepath.FromSlash(sortKey))
	base = strings.TrimSuffix(base, filepath.Ext(base))
	title, err := sanitizeWorkshopTitle(base)
	if err != nil {
		return "", err
	}
	return title + ".vpk", nil
}

func qqPackageTargetPrefix(jobID string) string {
	compact := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(jobID)), "-", "")
	if len(compact) > 16 {
		compact = compact[:16]
	}
	if compact == "" {
		compact = safeLogName(jobID)
	}
	return "qq_" + compact
}

func commitQQPackageVPK(source, instancePath, target, expectedSHA256 string, expectedSize int64) (bool, error) {
	if existingSHA, _, err := hashRegularFileWithinNoFollow(instancePath, target, maxPackageDownload); err == nil {
		if strings.EqualFold(existingSHA, expectedSHA256) {
			return true, nil
		}
		return false, errors.New("QQ flash VPK target already exists with different content")
	}
	err := copyVerifiedPackageFile(source, instancePath, target, expectedSHA256, expectedSize, maxPackageDownload, 0640)
	if err == nil {
		return false, nil
	}
	if existingSHA, _, hashErr := hashRegularFileWithinNoFollow(instancePath, target, maxPackageDownload); hashErr == nil && strings.EqualFold(existingSHA, expectedSHA256) {
		return true, nil
	}
	return false, fmt.Errorf("install QQ flash VPK %s: %w", filepath.Base(target), err)
}

func copyVerifiedPackageFile(source, root, target, sha string, size, limit int64, mode os.FileMode) error {
	if size <= 0 || size > limit {
		return errors.New("invalid package commit size")
	}
	in, err := openManagedFileNoFollow(filepath.Dir(source), filepath.Base(source))
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != size {
		return errors.New("package source size changed before commit")
	}
	_, err = writeFileWithinNoFollow(root, target, in, secureWriteOptions{Mode: mode, ExpectedSize: size, ExpectedSHA256: sha, Replace: false, RejectExistingLink: true})
	return err
}
