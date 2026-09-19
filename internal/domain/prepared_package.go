package domain

import (
	"errors"
	"path"
	"strings"
	"time"
)

const PreparedPackageMaxBytes int64 = 512 << 20
const PreparedPackageTTL = time.Hour
const PreparedPackageSlots = 16

func ValidPreparedPackageKind(kind string) bool {
	switch kind {
	case "vpk", "smx", "plugin", "native":
		return true
	default:
		return false
	}
}

// PreparedPackage contains no source URL, upload body or host path. ID is a job
// ID, not a bearer credential: both ends must enforce node/instance binding.
type PreparedPackage struct {
	ID         string    `json:"id"`
	NodeID     string    `json:"node_id"`
	InstanceID string    `json:"instance_id"`
	Name       string    `json:"name"`
	Kind       string    `json:"kind"`
	SHA256     string    `json:"sha256"`
	Size       int64     `json:"size"`
	ExpiresAt  time.Time `json:"expires_at"`
}

func PreparedPackageTarget(kind, name string) (string, error) {
	ext := strings.ToLower(path.Ext(name))
	dir := "left4dead2/addons/sourcemod/plugins/"
	switch kind {
	case "vpk":
		if ext != ".vpk" {
			return "", errors.New("VPK 文件名必须以 .vpk 结尾")
		}
		_, _, target, err := PackageRenamePaths("vpk", "left4dead2/addons/package.vpk", name)
		return target, err
	case "smx", "plugin":
		if ext != ".smx" {
			return "", errors.New("插件文件名必须以 .smx 结尾")
		}
	case "native":
		if ext != ".dll" && ext != ".so" {
			return "", errors.New("原生扩展必须以 .dll 或 .so 结尾")
		}
		dir = "left4dead2/addons/sourcemod/extensions/"
	default:
		return "", errors.New("预下载仅支持单文件 VPK、SMX 和原生扩展")
	}
	// Reuse the same basename/device/ADS/UTF-8 checks as ordinary package names.
	if _, _, _, err := PackageRenamePaths("plugin", "left4dead2/addons/sourcemod/plugins/package.smx", strings.TrimSuffix(name, path.Ext(name))+".smx"); err != nil {
		return "", err
	}
	if ValidatePackageName(name) != nil {
		return "", errors.New("包文件名无效")
	}
	return dir + name, nil
}
