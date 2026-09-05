package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

func ValidatePackageName(name string) error {
	if name != strings.TrimSpace(name) || name == "" || len(name) > 240 || !utf8.ValidString(name) {
		return errors.New("名称不能为空、首尾不能有空格，且最多 240 个 UTF-8 字节")
	}
	for _, r := range name {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return errors.New("名称不能包含控制字符或隐藏格式字符")
		}
	}
	return nil
}

// PackageRenamePaths only permits changing a basename in the existing fixed
// package directory. The canonical paths also cover disabled packages' history.
func PackageRenamePaths(kind, source, filename string) (target, oldCanonical, newCanonical string, err error) {
	fail := func() (string, string, string, error) {
		return "", "", "", errors.New("仅允许重命名固定模组/插件目录中的同类型 VPK 或 SMX 文件；名称不能包含路径、保留字符或设备名")
	}
	var dir, disabled, ext string
	switch kind {
	case "vpk", "workshop":
		dir, disabled, ext = "left4dead2/addons", "panel-disabled", ".vpk"
	case "plugin", "smx":
		dir, disabled, ext = "left4dead2/addons/sourcemod/plugins", "disabled", ".smx"
	default:
		return fail()
	}
	for _, base := range []string{path.Base(source), filename} {
		if ValidatePackageName(base) != nil || strings.ContainsAny(base, `<>:"/\|?*`) || strings.HasSuffix(base, ".") || !strings.EqualFold(path.Ext(base), ext) || strings.HasPrefix(strings.ToLower(base), "l4d2panel-map-") {
			return fail()
		}
		stem := strings.ToUpper(strings.Split(base, ".")[0])
		if stem == "" || strings.TrimSpace(stem) != stem || stem == "CON" || stem == "PRN" || stem == "AUX" || stem == "NUL" || stem == "CLOCK$" || stem == "CONIN$" || stem == "CONOUT$" || (len([]rune(stem)) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && strings.ContainsRune("123456789¹²³", []rune(stem)[3])) {
			return fail()
		}
	}
	if path.Dir(source) != dir && path.Dir(source) != dir+"/"+disabled {
		return fail()
	}
	if path.Clean(source) != source || strings.Contains(source, `\`) {
		return fail()
	}
	target = path.Dir(source) + "/" + filename
	if source != target && strings.EqualFold(source, target) {
		return "", "", "", errors.New("仅改变大小写的重命名不受支持，请先使用一个不同的名称")
	}
	return target, dir + "/" + path.Base(source), dir + "/" + filename, nil
}

func RenamedPackageSnapshot(instanceID, oldTarget, newTarget, storage string) (string, error) {
	key := func(value string) string { sum := sha256.Sum256([]byte(value)); return hex.EncodeToString(sum[:]) }
	prefix := "package-versions/" + key(instanceID) + "/"
	oldPrefix := prefix + key(oldTarget) + "/"
	if !strings.HasPrefix(storage, oldPrefix) {
		return "", errors.New("package snapshot is outside its registered target directory")
	}
	base := strings.TrimPrefix(storage, oldPrefix)
	if len(base) != 68 || !strings.HasSuffix(base, ".bin") {
		return "", errors.New("invalid package snapshot path")
	}
	if _, err := hex.DecodeString(strings.TrimSuffix(base, ".bin")); err != nil {
		return "", err
	}
	return prefix + key(newTarget) + "/" + base, nil
}
