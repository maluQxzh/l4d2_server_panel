package agent

import (
	"context"
	"errors"
	"path"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

// Keep the share's payload in the same bounded preparation reservation as
// HTTPS/uploads. Do not stage a second 2 GiB copy through the archive pathway.
func downloadPreparedQQPackage(ctx context.Context, client *qqFlashClient, source, root, relative, kind string, progress ProgressFunc) (string, error) {
	files, deviceID, _, _, err := client.listAllFiles(ctx, source, maxQQPluginShareEntries, domain.PreparedPackageMaxBytes)
	if err != nil {
		return "", err
	}
	if len(files) != 1 {
		return "", errors.New("预下载分享必须恰好包含一个文件，多文件或归档请使用原导入流程")
	}
	file := files[0]
	name := path.Base(file.Relative)
	if err := domain.ValidatePreparedPackageName(kind, name); err != nil {
		return "", err
	}
	if file.File.Size <= 0 || file.File.Size > domain.PreparedPackageMaxBytes {
		return "", errors.New("预下载文件必须大于 0 且不超过 512 MiB")
	}
	// downloadFile retains QQ's signed endpoint, SSRF, shared concurrency,
	// range consistency, exact-size and no-follow commit checks.
	if err := client.downloadFile(ctx, root, relative, file.File, deviceID, func(percent int) {
		progress(10+percent*60/100, "正在从 QQ 闪传预下载单文件包")
	}); err != nil {
		return "", err
	}
	_, size, err := hashRegularFileWithinNoFollow(root, relative, domain.PreparedPackageMaxBytes)
	if err != nil || size != file.File.Size {
		return "", errors.New("QQ flash prepared file size changed")
	}
	return name, nil
}
