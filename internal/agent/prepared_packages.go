package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
	"github.com/l4d2-panel/l4d2panel/internal/protocol"
)

type preparedPackageRecord struct {
	domain.PreparedPackage
	Ready bool   `json:"ready"`
	Claim string `json:"claim,omitempty"`
}

func preparedID(id string) bool {
	parsed, err := uuid.Parse(id)
	return err == nil && parsed.String() == id
}

func readPreparedPackage(root, id string) (preparedPackageRecord, error) {
	var record preparedPackageRecord
	if !preparedID(id) {
		return record, errors.New("invalid prepared package ID")
	}
	raw, err := readRegularFileWithinNoFollow(root, id+".json", 4096)
	if err != nil {
		return record, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return record, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return record, errors.New("trailing prepared metadata")
	}
	if record.ID != id || record.InstanceID == "" || record.NodeID == "" || record.ExpiresAt.IsZero() {
		return record, errors.New("invalid prepared package record")
	}
	if _, err := domain.PreparedPackageTarget(record.Kind, record.Name); err != nil {
		return record, err
	}
	return record, nil
}

func savePreparedPackage(root string, record preparedPackageRecord) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	_, err = writeFileWithinNoFollow(root, record.ID+".json", bytes.NewReader(raw), managedRawWriteOptions(len(raw), 0600))
	return err
}

func removePreparedPackage(root, id string) error {
	if !preparedID(id) {
		return errors.New("invalid prepared package ID")
	}
	// Flat fixed paths only; no caller path, directory traversal or recursive delete.
	for _, suffix := range []string{".bin", ".json"} {
		if err := deleteManagedFileNoFollow(root, id+suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// Caller holds preparedMu. Active tasks are never cleaned, even if their TTL
// elapses mid-operation. Incomplete/crashed reservations are reclaimed at startup.
func (e *Executor) cleanupPreparedLocked(root string, now time.Time) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".bin") {
			id := strings.TrimSuffix(entry.Name(), ".bin")
			if preparedID(id) && !e.preparedActive[id] {
				if _, err := os.Lstat(filepath.Join(root, id+".json")); errors.Is(err, os.ErrNotExist) {
					if err := deleteManagedFileNoFollow(root, entry.Name()); err != nil {
						return err
					}
				}
			}
		}
		// Atomic staging writes may leave temporary files after a process crash.
		// Never remove temporaries while any live preparation/commit uses this root.
		if len(e.preparedActive) == 0 && (strings.HasPrefix(entry.Name(), ".download-") || strings.HasPrefix(entry.Name(), ".managed-")) {
			if err := deleteManagedFileNoFollow(root, entry.Name()); err != nil {
				return err
			}
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if !preparedID(id) || e.preparedActive[id] {
			continue
		}
		record, err := readPreparedPackage(root, id)
		if err != nil {
			return errors.New("invalid prepared package staging metadata")
		}
		if !record.Ready || record.Claim != "" || !record.ExpiresAt.After(now) {
			if err := removePreparedPackage(root, id); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *Executor) cleanupPreparedPackages(now time.Time) error {
	e.preparedMu.Lock()
	defer e.preparedMu.Unlock()
	root, err := ensureDataSubdirectory(e.config.DataDir, "prepared-packages")
	if err != nil {
		return err
	}
	return e.cleanupPreparedLocked(root, now)
}

func (e *Executor) reservePrepared(job domain.Job, record preparedPackageRecord) (string, error) {
	e.preparedMu.Lock()
	defer e.preparedMu.Unlock()
	root, err := ensureDataSubdirectory(e.config.DataDir, "prepared-packages")
	if err != nil {
		return "", err
	}
	if e.preparedActive == nil {
		e.preparedActive = make(map[string]bool)
	}
	if err := e.cleanupPreparedLocked(root, time.Now()); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", err
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() || !entry.Type().IsRegular() {
			return "", errors.New("unexpected prepared staging entry")
		}
		if !strings.HasPrefix(entry.Name(), ".download-") && !strings.HasPrefix(entry.Name(), ".managed-") &&
			!((strings.HasSuffix(entry.Name(), ".json") || strings.HasSuffix(entry.Name(), ".bin")) && preparedID(strings.TrimSuffix(strings.TrimSuffix(entry.Name(), ".json"), ".bin"))) {
			return "", errors.New("unexpected unaccounted prepared staging file")
		}
		if strings.HasSuffix(entry.Name(), ".json") {
			count++
		}
	}
	if count >= domain.PreparedPackageSlots {
		return "", errors.New("预下载暂存已满，请安装或丢弃已有包后重试")
	}
	// Reserve the maximum size of every slot, including incomplete downloads.
	if err := ensureArchiveDiskReserve(root, int64(count+1)*domain.PreparedPackageMaxBytes, 1<<30, "package preparation"); err != nil {
		return "", err
	}
	if _, err := os.Lstat(filepath.Join(root, job.ID+".json")); !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("prepared ID already reserved")
	}
	if err := savePreparedPackage(root, record); err != nil {
		return "", err
	}
	e.preparedActive[job.ID] = true
	return root, nil
}

func (e *Executor) acquirePreparationSlot(ctx context.Context) (func(), error) {
	e.preparedMu.Lock()
	if e.preparedSlots == nil {
		e.preparedSlots = make(chan struct{}, 2)
	}
	slots := e.preparedSlots
	e.preparedMu.Unlock()
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (e *Executor) preparePackage(ctx context.Context, job domain.Job, progress ProgressFunc) (result map[string]any, err error) {
	if exactPayloadString(job.Payload, "kind") == domain.PreparedWorkshopKind {
		return e.prepareWorkshop(ctx, job, progress)
	}
	if exactPayloadString(job.Payload, "kind") == domain.PreparedVPKBundleKind {
		return e.prepareVPKBundle(ctx, job, progress)
	}
	if !preparedID(job.ID) || job.NodeID != e.config.NodeID {
		return nil, errors.New("invalid preparation job binding")
	}
	if _, ok := e.registry.Get(job.InstanceID); !ok {
		return nil, errors.New("instance is not registered")
	}
	name, kind := exactPayloadString(job.Payload, "name"), exactPayloadString(job.Payload, "kind")
	transport := exactPayloadString(job.Payload, "source_transport")
	if transport != "" && transport != "qq_flash" {
		return nil, errors.New("unsupported preparation source transport")
	}
	if transport == "qq_flash" {
		if !domain.ValidPreparedPackageKind(kind) || name != "" || exactPayloadString(job.Payload, "content_base64") != "" {
			return nil, errors.New("QQ flash preparation requires a single-file kind and share source only")
		}
		// Preserve the v1 on-disk reservation format even before QQ metadata is
		// known, so an older Agent can clean interrupted preparations after rollback.
		name = "preparing.smx"
		if kind == "vpk" {
			name = "preparing.vpk"
		} else if kind == "native" {
			name = "preparing.so"
		}
	} else if _, err := domain.PreparedPackageTarget(kind, name); err != nil {
		return nil, err
	}
	content, source := exactPayloadString(job.Payload, "content_base64"), exactPayloadString(job.Payload, "source")
	if (content == "") == (source == "") {
		return nil, errors.New("provide exactly one upload or HTTPS source")
	}
	if source != "" {
		if err := validateDownloadURL(source); err != nil {
			return nil, err
		}
	}
	release, err := e.acquirePreparationSlot(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	record := preparedPackageRecord{PreparedPackage: domain.PreparedPackage{ID: job.ID, NodeID: job.NodeID, InstanceID: job.InstanceID, Name: name, Kind: kind, ExpiresAt: time.Now().UTC().Add(domain.PreparedPackageTTL)}}
	root, err := e.reservePrepared(job, record)
	if err != nil {
		return nil, err
	}
	defer func() {
		e.preparedMu.Lock()
		defer e.preparedMu.Unlock()
		delete(e.preparedActive, job.ID)
		if err != nil {
			_ = removePreparedPackage(root, job.ID)
		}
	}()
	progress(10, "正在下载到节点暂存目录，不占用实例写锁")
	local := filepath.Join(root, job.ID+".bin")
	if content != "" {
		if len(content) > protocol.MaxEncodedPackageUploadBytes {
			return nil, errors.New("uploaded package exceeds 100 MiB")
		}
		raw, decodeErr := base64.StdEncoding.Strict().DecodeString(content)
		if decodeErr != nil || len(raw) == 0 || len(raw) > protocol.MaxPackageUploadBytes {
			return nil, errors.New("invalid uploaded package")
		}
		_, err = writeFileWithinNoFollow(root, job.ID+".bin", bytes.NewReader(raw), secureWriteOptions{Mode: 0600, ExpectedSize: int64(len(raw)), Replace: false})
	} else if transport == "qq_flash" {
		client := e.preparedQQClient
		if client == nil {
			client = newQQFlashClient(e.safeDownloadLimiter())
		}
		record.Name, err = downloadPreparedQQPackage(ctx, client, source, root, job.ID+".bin", kind, progress)
	} else {
		// Account for this single-stream HTTPS transfer in the shared node budget.
		release, acquireErr := e.safeDownloadLimiter().acquire(ctx)
		if acquireErr != nil {
			return nil, acquireErr
		}
		defer release()
		downloader := e.packageDownloader
		if downloader == nil {
			downloader = func(ctx context.Context, source, destination string, _ bool) (string, int64, error) {
				return DownloadHTTPS(ctx, source, destination)
			}
		}
		_, _, err = downloader(ctx, source, local, false)
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if transport == "qq_flash" {
			return nil, errors.New("QQ 闪传预下载失败：请确认分享有效、仅含一个与所选类型一致的文件且不超过 512 MiB；压缩包或多文件请使用原导入流程")
		}
		return nil, errors.New("包下载或上传暂存失败，请检查来源及节点磁盘")
	}
	progress(70, "正在校验暂存包")
	record.SHA256, record.Size, err = hashRegularFileWithinNoFollow(root, job.ID+".bin", domain.PreparedPackageMaxBytes)
	if err != nil || record.Size == 0 {
		return nil, errors.New("invalid prepared file size or hash")
	}
	if kind == "vpk" {
		_, hash, validationErr := validateVPK(local)
		if validationErr != nil || hash != record.SHA256 {
			return nil, errors.New("VPK 结构、CRC 或摘要校验失败")
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	record.Ready = true
	if err := savePreparedPackage(root, record); err != nil {
		return nil, err
	}
	progress(100, "预下载完成，可在有效期内安装")
	return structToMap(record.PreparedPackage)
}

func (e *Executor) consumePreparedPackage(ctx context.Context, job domain.Job, discard bool, progress ProgressFunc) (map[string]any, error) {
	if exactPayloadString(job.Payload, "kind") == domain.PreparedWorkshopKind {
		return e.consumePreparedWorkshop(ctx, job, discard, progress)
	}
	if exactPayloadString(job.Payload, "kind") == domain.PreparedVPKBundleKind {
		return e.consumePreparedVPKBundle(ctx, job, discard, progress)
	}
	id := exactPayloadString(job.Payload, "prepared_package_id")
	if !preparedID(id) {
		return nil, errors.New("invalid prepared package ID")
	}
	release, err := e.locks.acquire(ctx, "prepared:"+id)
	if err != nil {
		return nil, err
	}
	defer release()
	e.preparedMu.Lock()
	root, err := ensureDataSubdirectory(e.config.DataDir, "prepared-packages")
	if err != nil {
		e.preparedMu.Unlock()
		return nil, err
	}
	record, err := readPreparedPackage(root, id)
	if err != nil || record.NodeID != job.NodeID || record.NodeID != e.config.NodeID || record.InstanceID != job.InstanceID || e.preparedActive[id] || record.Claim != "" {
		e.preparedMu.Unlock()
		return nil, errors.New("预下载包已失效、被使用或不属于此实例，请重新准备")
	}
	if !discard && (!record.Ready || !record.ExpiresAt.After(time.Now()) || record.SHA256 != exactPayloadString(job.Payload, "expected_sha256") || record.Size != int64Payload(job.Payload, "expected_size") || record.Kind != exactPayloadString(job.Payload, "kind") || record.Name != exactPayloadString(job.Payload, "name")) {
		e.preparedMu.Unlock()
		return nil, errors.New("prepared package metadata mismatch or expired")
	}
	if e.preparedActive == nil {
		e.preparedActive = make(map[string]bool)
	}
	record.Claim = job.ID
	if err := savePreparedPackage(root, record); err != nil {
		e.preparedMu.Unlock()
		return nil, err
	}
	e.preparedActive[id] = true
	e.preparedMu.Unlock()
	defer func() {
		e.preparedMu.Lock()
		defer e.preparedMu.Unlock()
		delete(e.preparedActive, id)
		_ = removePreparedPackage(root, id)
	}()
	if discard {
		return map[string]any{"discarded": true}, nil
	}
	sha, size, err := hashRegularFileWithinNoFollow(root, id+".bin", domain.PreparedPackageMaxBytes)
	if err != nil || sha != record.SHA256 || size != record.Size {
		return nil, errors.New("prepared package bytes changed")
	}
	payload, err := decodeOperation(job)
	if err != nil {
		return nil, err
	}
	instance, ok := e.registry.Get(job.InstanceID)
	if !ok || instance.ExternalPID > 0 {
		return nil, errors.New("instance is unavailable or externally managed")
	}
	instancePath, err := e.instancePath(instance)
	if err != nil {
		return nil, err
	}
	target, err := domain.PreparedPackageTarget(record.Kind, record.Name)
	if err != nil {
		return nil, err
	}
	local := filepath.Join(root, id+".bin")
	kind := record.Kind
	result := map[string]any{"path": target, "sha256": sha, "size": size}
	if kind == "vpk" {
		version, hash, err := validateVPK(local)
		if err != nil || hash != sha {
			return nil, errors.New("prepared VPK failed commit validation")
		}
		result["vpk_version"] = version
	} else if kind != "native" {
		kind = "plugin"
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	progress(70, "取得实例写锁，重新校验并安装预下载包")
	if kind == "native" {
		if !payload.RiskConfirmed && !payload.AcknowledgeRisk {
			return nil, errors.New("native extension requires acknowledge_native_risk")
		}
		err = copyRegularFileToRootNoFollow(local, instancePath, target, secureWriteOptions{Mode: 0640, ExpectedSHA256: sha, Replace: true, RejectExistingLink: true})
	} else {
		previous, pruned, installErr := e.installVersionedPackage(ctx, job, payload, kind, instancePath, local, target, sha, progress)
		err = installErr
		if previous != nil {
			result["previous_version"] = previous.result()
		}
		if len(pruned) > 0 {
			result["pruned_snapshots"] = pruned
		}
	}
	if err != nil {
		return nil, fmt.Errorf("install prepared package: %w", err)
	}
	result["kind"] = kind
	progress(100, "预下载包安装完成")
	return result, nil
}

func int64Payload(payload map[string]any, key string) int64 {
	switch v := payload[key].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	default:
		return 0
	}
}
