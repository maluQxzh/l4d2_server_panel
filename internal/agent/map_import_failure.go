package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

const (
	mapImportStagePreflight = "preflight"
	mapImportStageStaging   = "staging"
	mapImportStageMetadata  = "metadata"
	mapImportStageDownload  = "download"
	mapImportStageArchive   = "archive"
	mapImportStageVPK       = "vpk"
	mapImportStagePublish   = "publish"
)

type mapImportInputError struct {
	Stage  string
	Source string
	Err    error
}

func (err *mapImportInputError) Error() string { return err.Err.Error() }
func (err *mapImportInputError) Unwrap() error { return err.Err }

type mapImportFailure struct {
	Stage       string
	Code        string
	Summary     string
	Remediation string
	Source      string
	Retryable   bool
	Err         error
}

func (failure *mapImportFailure) Error() string {
	if failure.Err == nil {
		return failure.Summary
	}
	return failure.Summary + "：" + failure.Err.Error()
}

func (failure *mapImportFailure) Unwrap() error { return failure.Err }

func (failure *mapImportFailure) JobFailureResult() map[string]any {
	details := map[string]any{
		"stage":       failure.Stage,
		"code":        failure.Code,
		"summary":     failure.Summary,
		"remediation": failure.Remediation,
		"retryable":   failure.Retryable,
	}
	if source := truncateUTF8(strings.ToValidUTF8(strings.TrimSpace(failure.Source), "?"), 512); source != "" {
		details["source"] = source
	}
	return map[string]any{"failure": details}
}

func wrapMapImportFailure(stage, source string, err error) error {
	if err == nil || errors.Is(err, context.Canceled) {
		return err
	}
	var existing *mapImportFailure
	if errors.As(err, &existing) {
		return err
	}
	var input *mapImportInputError
	if errors.As(err, &input) {
		stage, source, err = input.Stage, input.Source, input.Err
	}
	code, summary, remediation, retryable := classifyMapImportFailure(stage, err)
	return &mapImportFailure{Stage: stage, Code: code, Summary: summary, Remediation: remediation,
		Source: source, Retryable: retryable, Err: err}
}

func classifyMapImportFailure(stage string, err error) (code, summary, remediation string, retryable bool) {
	detail := strings.ToLower(err.Error())
	switch stage {
	case mapImportStagePreflight:
		if strings.Contains(detail, "free disk space") || strings.Contains(detail, "reserved free") {
			return "map_disk_space_insufficient", "节点磁盘空间不足", "释放地图根所在磁盘空间，并确保处理后仍保留至少 5 GiB 可用空间。", false
		}
		return "map_preflight_failed", "地图导入预检失败", "检查分享内容数量、大小和所选路径编码后重新提交。", false
	case mapImportStageStaging:
		return "map_staging_failed", "无法准备地图暂存目录", "检查节点地图根权限、磁盘状态和剩余空间后重试。", false
	case mapImportStageMetadata:
		if strings.Contains(detail, "contains no files") {
			return "qq_share_empty", "QQ 闪传中没有可下载文件", "确认分享内容已上传完成，并重新创建包含地图文件的分享链接。", false
		}
		if strings.Contains(detail, "expired") || strings.Contains(detail, "unavailable") {
			return "qq_share_unavailable", "QQ 闪传链接已失效或不可用", "重新创建仍在有效期内且允许匿名下载的 QQ 闪传链接。", false
		}
		return "qq_metadata_failed", "读取 QQ 闪传信息失败", "确认节点能够访问 qfile.qq.com；若链接仍有效，可稍后重试。", true
	case mapImportStageDownload:
		return "map_download_failed", "地图文件下载失败", "检查节点到 multimedia.qfile.qq.com 的网络连接；临时网络错误可直接重试。", true
	case mapImportStageArchive:
		if strings.Contains(detail, "decode zip path") || strings.Contains(detail, "path encoding") || strings.Contains(detail, "valid utf-8 after decoding") {
			return "map_archive_path_encoding_invalid", "ZIP 内部路径编码无法识别", "在导入窗口改选 UTF-8、GBK 或 Big5；仍失败时请重新打包 ZIP。", false
		}
		if strings.Contains(detail, "encrypted") || strings.Contains(detail, "multi-volume") {
			return "map_archive_unsupported", "地图归档使用了不支持的加密或分卷格式", "解密并合并为单个 ZIP、7Z 或 RAR 后重新分享。", false
		}
		if strings.Contains(detail, "unsafe path") || strings.Contains(detail, "unsafe path component") || strings.Contains(detail, "link or special file") {
			return "map_archive_unsafe_path", "地图归档包含不安全路径或链接", "移除绝对路径、上级目录、链接、设备名或 ADS 条目后重新打包。", false
		}
		if strings.Contains(detail, "exceeds") || strings.Contains(detail, "reserved free") {
			return "map_archive_limit_exceeded", "地图归档超过处理限制", "减少归档层级、条目数或展开大小，并确认节点至少保留 5 GiB 空闲空间。", false
		}
		return "map_archive_invalid", "地图归档处理失败", "检查归档是否加密、分卷、损坏或包含危险路径；重新打包后再试。", false
	case mapImportStageVPK:
		if strings.Contains(detail, "cannot be decoded") || strings.Contains(detail, "path component") {
			return "map_vpk_path_encoding_invalid", "VPK 内部路径编码无法识别", "在导入窗口改选 UTF-8、GBK 或 Big5；不要只重命名外层 ZIP。", false
		}
		if strings.Contains(detail, "checksum mismatch") {
			return "map_vpk_checksum_mismatch", "VPK 内容校验失败", "源 VPK 已损坏或打包不完整，请重新获取或重新生成地图包。", false
		}
		if strings.Contains(detail, "did not contain a vpk") {
			return "map_vpk_missing", "没有找到可处理的 VPK", "确认分享内容包含地图 VPK，或包含可递归打开的 ZIP、7Z、RAR。", false
		}
		if strings.Contains(detail, "does not contain maps/*.bsp") {
			return "map_bsp_missing", "没有找到可运行的 BSP 地图", "确认分享的是 L4D2 地图 VPK，而不是素材包或普通模组。", false
		}
		return "map_vpk_invalid", "VPK 校验或合并失败", "检查任务中的具体 VPK 路径和错误；修复源 VPK 后重新导入。", false
	case mapImportStagePublish:
		return "map_publish_failed", "地图对象发布失败", "检查地图根权限、磁盘空间和对象库健康状态后重试。", false
	default:
		return "map_import_failed", "地图导入失败", fmt.Sprintf("检查任务错误详情后重试；处理阶段：%s。", stage), false
	}
}
