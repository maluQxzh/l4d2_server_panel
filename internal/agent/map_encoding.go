package agent

import (
	"bytes"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
)

type mapPathDecodeStats struct {
	VPKGBK  int
	VPKBig5 int
	ZIPGBK  int
	ZIPBig5 int
}

func (stats *mapPathDecodeStats) add(other mapPathDecodeStats) {
	stats.VPKGBK += other.VPKGBK
	stats.VPKBig5 += other.VPKBig5
	stats.ZIPGBK += other.ZIPGBK
	stats.ZIPBig5 += other.ZIPBig5
}

func mapPathDecodeWarnings(stats mapPathDecodeStats) []mapLocalWarning {
	warnings := make([]mapLocalWarning, 0, 4)
	appendWarning := func(count int, code, location, encodingName string) {
		if count > 0 {
			warnings = append(warnings, mapLocalWarning{Code: code,
				Message: location + "中有 " + strconv.Itoa(count) + " 个旧式 " + encodingName + " 路径组件已转换为 UTF-8"})
		}
	}
	appendWarning(stats.VPKGBK, "legacy_gbk_vpk_paths", "VPK", "GBK")
	appendWarning(stats.VPKBig5, "legacy_big5_vpk_paths", "VPK", "Big5")
	appendWarning(stats.ZIPGBK, "legacy_gbk_zip_paths", "ZIP", "GBK")
	appendWarning(stats.ZIPBig5, "legacy_big5_zip_paths", "ZIP", "Big5")
	return warnings
}

func normalizeMapPathEncoding(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		value = domain.MapPathEncodingAuto
	}
	if !domain.ValidMapPathEncoding(value) {
		return "", errors.New("map path_encoding must be auto, utf8, gbk, or big5")
	}
	return value, nil
}

func decodeMapPathBytes(raw []byte, strategy string) (string, string, error) {
	strategy, err := normalizeMapPathEncoding(strategy)
	if err != nil {
		return "", "", err
	}
	if bytes.IndexFunc(raw, func(char rune) bool { return char >= utf8.RuneSelf }) < 0 {
		return string(raw), "", nil
	}
	switch strategy {
	case domain.MapPathEncodingUTF8:
		if !utf8.Valid(raw) {
			return "", "", errors.New("path component is not valid UTF-8")
		}
		return string(raw), "", nil
	case domain.MapPathEncodingGBK:
		value, err := decodeMapLegacyBytes(raw, simplifiedchinese.GBK)
		return value, domain.MapPathEncodingGBK, err
	case domain.MapPathEncodingBig5:
		value, err := decodeMapLegacyBytes(raw, traditionalchinese.Big5)
		return value, domain.MapPathEncodingBig5, err
	default:
		if utf8.Valid(raw) {
			return string(raw), "", nil
		}
		value, err := decodeMapLegacyBytes(raw, simplifiedchinese.GBK)
		return value, domain.MapPathEncodingGBK, err
	}
}

func decodeMapZIPPath(raw string, nonUTF8 bool, strategy string, stats *mapPathDecodeStats) (string, error) {
	strategy, err := normalizeMapPathEncoding(strategy)
	if err != nil {
		return "", err
	}
	// An explicit legacy encoding is an administrator override. Apply it even
	// when a broken ZIP producer incorrectly set the UTF-8 language flag.
	if !nonUTF8 && strategy != domain.MapPathEncodingGBK && strategy != domain.MapPathEncodingBig5 {
		if !utf8.ValidString(raw) {
			return "", errors.New("ZIP path marked as UTF-8 contains invalid bytes")
		}
		return raw, nil
	}
	components := bytes.Split([]byte(raw), []byte{'/'})
	decoded := make([]string, len(components))
	for index, component := range components {
		value, usedEncoding, err := decodeMapPathBytes(component, strategy)
		if err != nil {
			return "", err
		}
		decoded[index] = value
		switch usedEncoding {
		case domain.MapPathEncodingGBK:
			stats.ZIPGBK++
		case domain.MapPathEncodingBig5:
			stats.ZIPBig5++
		}
	}
	return strings.Join(decoded, "/"), nil
}

func decodeMapLegacyBytes(raw []byte, codec encoding.Encoding) (string, error) {
	decoded, err := codec.NewDecoder().Bytes(raw)
	if err != nil || !utf8.Valid(decoded) || bytes.ContainsRune(decoded, utf8.RuneError) {
		return "", errors.New("path component cannot be decoded without replacement characters")
	}
	return string(decoded), nil
}
