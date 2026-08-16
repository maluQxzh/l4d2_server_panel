package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	maxQQPluginShareEntries = 1024
	maxQQPluginDownload     = int64(2 << 30)
	qqPluginDiskReserve     = uint64(1 << 30)
)

// stageQQFlashPluginPackage downloads exactly one shared package into the
// Agent-owned staging directory. Requiring one regular file keeps package-kind
// selection explicit and prevents a share layout from choosing an archive
// implicitly.
func (e *Executor) stageQQFlashPluginPackage(ctx context.Context, jobID, shareURL string, progress ProgressFunc) (string, string, string, int64, func(), error) {
	return e.stageQQFlashPluginPackageWithClient(ctx, newQQFlashClient(e.safeDownloadLimiter()), jobID, shareURL, progress)
}

func (e *Executor) stageQQFlashPluginPackageWithClient(ctx context.Context, client *qqFlashClient, jobID, shareURL string, progress ProgressFunc) (string, string, string, int64, func(), error) {
	progress(3, "reading QQ flash plugin package metadata")
	listed, deviceID, _, _, err := client.listAllFiles(ctx, shareURL, maxQQPluginShareEntries, maxQQPluginDownload)
	if err != nil {
		return "", "", "", 0, nil, err
	}
	if len(listed) != 1 {
		return "", "", "", 0, nil, errors.New("QQ flash plugin share must contain exactly one package file")
	}
	item := listed[0]
	name := filepath.Base(filepath.FromSlash(item.Relative))
	if name == "" || name == "." || strings.ContainsAny(name, "\x00/\\") {
		return "", "", "", 0, nil, errors.New("QQ flash plugin package name is invalid")
	}
	staging, err := ensureDataSubdirectory(e.config.DataDir, "staging")
	if err != nil {
		return "", "", "", 0, nil, err
	}
	if err := ensureArchiveDiskReserve(staging, item.File.Size, qqPluginDiskReserve, "QQ flash plugin package"); err != nil {
		return "", "", "", 0, nil, err
	}
	extension := qqPluginPackageExtension(name)
	relative := "qq-plugin-" + safeLogName(jobID) + extension
	path, err := ResolveWithin(staging, relative)
	if err != nil {
		return "", "", "", 0, nil, err
	}
	cleanup := func() { _ = os.Remove(path) }
	progress(8, "downloading QQ flash plugin package")
	if err := client.downloadFile(ctx, staging, relative, item.File, deviceID, func(percent int) {
		progress(8+percent*17/100, "downloading QQ flash plugin package")
	}); err != nil {
		cleanup()
		return "", "", "", 0, nil, fmt.Errorf("download QQ flash plugin package: %w", err)
	}
	digest, size, err := hashFile(path)
	if err != nil {
		cleanup()
		return "", "", "", 0, nil, err
	}
	if size != item.File.Size {
		cleanup()
		return "", "", "", 0, nil, errors.New("QQ flash plugin package size changed after download")
	}
	return path, name, digest, size, cleanup, nil
}

func qqPluginPackageExtension(name string) string {
	lower := strings.ToLower(name)
	for _, suffix := range []string{".tar.gz", ".tar.bz2", ".tar.xz", ".zip", ".7z", ".rar", ".tar", ".gz", ".bz2", ".xz", ".smx", ".dll", ".so"} {
		if strings.HasSuffix(lower, suffix) {
			return suffix
		}
	}
	return ".package"
}
