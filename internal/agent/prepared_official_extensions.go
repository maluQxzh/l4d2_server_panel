package agent

import (
	"context"
	"errors"
	"net/url"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func (e *Executor) downloadPreparedOfficialExtension(ctx context.Context, root string, record *preparedPackageRecord, progress ProgressFunc) error {
	resolver := e.officialPackageResolver
	if resolver == nil {
		resolver = ResolveOfficialPackage
	}
	progress(5, "正在解析当前节点平台的官方稳定版")
	resolved, err := resolver(ctx, record.Kind)
	if err != nil {
		return err
	}
	if resolved.Kind != record.Kind || resolved.OS != runtime.GOOS || !domain.ValidPreparedOfficialVersion(resolved.Version) {
		return errors.New("official resolver returned mismatched package metadata")
	}
	if err := validateOfficialAssetURL(resolved.URL); err != nil {
		return err
	}
	u, err := url.Parse(resolved.URL)
	if err != nil {
		return err
	}
	name := path.Base(u.Path)
	spec, ok := officialPackageSpecs[record.Kind]
	versionMatch := spec.versionRegexp
	if !ok || versionMatch == nil {
		return errors.New("unsupported official component")
	}
	match := versionMatch.FindStringSubmatch(name)
	suffix := "-" + runtime.GOOS + ".zip"
	if runtime.GOOS == "linux" {
		suffix = "-linux.tar.gz"
	}
	if len(match) != 2 || match[1] != resolved.Version || !strings.HasSuffix(name, suffix) {
		return errors.New("official asset name does not match version or platform")
	}
	if err := domain.ValidatePreparedPackageName(record.Kind, name); err != nil {
		return err
	}
	release, err := e.safeDownloadLimiter().acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	downloader := e.packageDownloader
	if downloader == nil {
		downloader = func(ctx context.Context, source, destination string, _ bool) (string, int64, error) {
			return DownloadOfficialHTTPS(ctx, source, destination)
		}
	}
	sha, size, _, err := downloadPackageFromSources(ctx, officialPackageDownloadSources(resolved), filepath.Join(root, record.ID+".bin"), true, resolved.ExpectedSHA256, resolved.ExpectedSize, downloader, progress)
	if err != nil {
		return err
	}
	actualSHA, actualSize, err := hashRegularFileWithinNoFollow(root, record.ID+".bin", domain.PreparedPackageMaxBytes)
	if err != nil || actualSize <= 0 || actualSHA != strings.ToLower(sha) || actualSize != size || (resolved.ExpectedSHA256 != "" && (!strings.EqualFold(actualSHA, resolved.ExpectedSHA256) || actualSize != resolved.ExpectedSize)) {
		return errors.New("official archive integrity changed during preparation")
	}
	record.Name, record.Version = name, resolved.Version
	record.SHA256, record.Size = actualSHA, actualSize
	return nil
}
