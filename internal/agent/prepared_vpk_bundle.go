package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

// Kept in its own flat directory: older Agents retain their strict v1
// single-file metadata format and cannot mistake a bundle for one file.
type preparedVPKBundleRecord struct {
	domain.PreparedVPKBundle
	Ready bool   `json:"ready"`
	Claim string `json:"claim,omitempty"`
}

func preparedBundleFile(id string, index int) string { return fmt.Sprintf("%s-%04d.bin", id, index+1) }

func readPreparedVPKBundle(root, id string) (preparedVPKBundleRecord, error) {
	var record preparedVPKBundleRecord
	if !preparedID(id) {
		return record, errors.New("invalid prepared bundle ID")
	}
	raw, err := readRegularFileWithinNoFollow(root, id+".json", 1<<20)
	if err != nil {
		return record, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return record, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return record, errors.New("trailing prepared bundle metadata")
	}
	if record.ID != id || record.NodeID == "" || record.InstanceID == "" || record.Kind != domain.PreparedVPKBundleKind || record.Name != domain.PreparedVPKBundleName || record.ExpiresAt.IsZero() {
		return record, errors.New("invalid prepared bundle binding")
	}
	if record.Ready || len(record.Files) > 0 {
		if err := record.Validate(); err != nil {
			return record, err
		}
	}
	return record, nil
}

func savePreparedVPKBundle(root string, record preparedVPKBundleRecord) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	_, err = writeFileWithinNoFollow(root, record.ID+".json", bytes.NewReader(raw), managedRawWriteOptions(len(raw), 0600))
	return err
}

func removePreparedVPKBundle(root string, record preparedVPKBundleRecord) error {
	if !preparedID(record.ID) || len(record.Files) > domain.PreparedVPKBundleMaxFiles {
		return errors.New("invalid bundle cleanup")
	}
	for index := range record.Files {
		if err := deleteManagedFileNoFollow(root, preparedBundleFile(record.ID, index)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := deleteManagedFileNoFollow(root, record.ID+".json"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Caller holds preparedMu. Active preparation and consumption remain pinned.
func (e *Executor) cleanupPreparedVPKBundlesLocked(root string, now time.Time) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasSuffix(name, ".json") && preparedID(strings.TrimSuffix(name, ".json")) {
			id := strings.TrimSuffix(name, ".json")
			if e.preparedActive[id] {
				continue
			}
			record, err := readPreparedVPKBundle(root, id)
			if err != nil {
				return errors.New("invalid prepared bundle staging metadata")
			}
			if !record.Ready || record.Claim != "" || !record.ExpiresAt.After(now) {
				if err := removePreparedVPKBundle(root, record); err != nil {
					return err
				}
			}
		} else if len(name) == 45 && strings.HasSuffix(name, ".bin") && preparedID(name[:36]) {
			index, err := strconv.Atoi(name[37:41])
			if err != nil || name[36] != '-' || index < 1 || index > domain.PreparedVPKBundleMaxFiles {
				return errors.New("invalid prepared bundle staging filename")
			}
			if !e.preparedActive[name[:36]] {
				if _, err := os.Lstat(filepath.Join(root, name[:36]+".json")); errors.Is(err, os.ErrNotExist) {
					if err := deleteManagedFileNoFollow(root, name); err != nil && !errors.Is(err, os.ErrNotExist) {
						return err
					}
				}
			}
		} else if strings.HasPrefix(name, ".managed-") {
			if len(e.preparedActive) == 0 {
				if err := deleteManagedFileNoFollow(root, name); err != nil {
					return err
				}
			}
		} else {
			return errors.New("unexpected prepared bundle staging entry")
		}
	}
	return nil
}

func (e *Executor) cleanupPreparedVPKBundles(now time.Time) error {
	e.preparedMu.Lock()
	defer e.preparedMu.Unlock()
	root, err := ensureDataSubdirectory(e.config.DataDir, "prepared-vpk-bundles")
	if err != nil {
		return err
	}
	return e.cleanupPreparedVPKBundlesLocked(root, now)
}

func (e *Executor) reservePreparedVPKBundle(record preparedVPKBundleRecord) (string, error) {
	e.preparedMu.Lock()
	defer e.preparedMu.Unlock()
	root, err := ensureDataSubdirectory(e.config.DataDir, "prepared-vpk-bundles")
	if err != nil {
		return "", err
	}
	if err := e.cleanupPreparedVPKBundlesLocked(root, time.Now()); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", err
	}
	count := 0
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") {
			count++
		}
	}
	if count >= domain.PreparedVPKBundleSlots {
		return "", errors.New("整包预下载暂存已满，请安装或丢弃已有文件包")
	}
	// Reserve every bundle's worst case plus source download and extraction.
	if err := ensureArchiveDiskReserve(root, int64(count+3)*domain.PreparedVPKBundleMaxBytes, 1<<30, "VPK bundle preparation"); err != nil {
		return "", err
	}
	if _, err := os.Lstat(filepath.Join(root, record.ID+".json")); !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("prepared bundle ID already reserved")
	}
	if err := savePreparedVPKBundle(root, record); err != nil {
		return "", err
	}
	if e.preparedActive == nil {
		e.preparedActive = make(map[string]bool)
	}
	e.preparedActive[record.ID] = true
	return root, nil
}

func (e *Executor) prepareVPKBundle(ctx context.Context, job domain.Job, progress ProgressFunc) (result map[string]any, err error) {
	if !preparedID(job.ID) || job.NodeID != e.config.NodeID || exactPayloadString(job.Payload, "source_transport") != "qq_flash" || exactPayloadString(job.Payload, "content_base64") != "" || exactPayloadString(job.Payload, "name") != "" {
		return nil, errors.New("invalid prepared VPK bundle request")
	}
	if _, ok := e.registry.Get(job.InstanceID); !ok {
		return nil, errors.New("instance is not registered")
	}
	release, err := e.acquirePreparationSlot(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	record := preparedVPKBundleRecord{PreparedVPKBundle: domain.PreparedVPKBundle{PreparedPackage: domain.PreparedPackage{
		ID: job.ID, NodeID: job.NodeID, InstanceID: job.InstanceID, Kind: domain.PreparedVPKBundleKind, Name: domain.PreparedVPKBundleName, ExpiresAt: time.Now().UTC().Add(domain.PreparedPackageTTL),
	}}}
	root, err := e.reservePreparedVPKBundle(record)
	if err != nil {
		return nil, err
	}
	defer func() {
		e.preparedMu.Lock()
		defer e.preparedMu.Unlock()
		delete(e.preparedActive, job.ID)
		if err != nil {
			_ = removePreparedVPKBundle(root, record)
		}
	}()
	client := e.preparedQQClient
	if client == nil {
		client = newQQFlashClient(e.safeDownloadLimiter())
	}
	files, cleanup, err := prepareQQFlashPackages(ctx, e.config.DataDir, exactPayloadString(job.Payload, "source"), job.ID, progress, client)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("QQ 闪传整包准备失败，请检查分享内容、VPK 格式、归档限制和磁盘空间")
	}
	defer cleanup()
	for _, file := range files {
		record.Files = append(record.Files, domain.PreparedVPKFile{Name: file.name, SHA256: file.sha256, Size: file.size, Version: file.version})
		record.Size += file.size
	}
	record.SHA256 = record.Digest()
	if err := record.Validate(); err != nil {
		return nil, err
	}
	// Persist the complete bounded cleanup manifest before writing any payload.
	if err := savePreparedVPKBundle(root, record); err != nil {
		return nil, err
	}
	for index, file := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := copyVerifiedPackageFile(file.input.Path, root, preparedBundleFile(job.ID, index), file.sha256, file.size, maxPackageDownload, 0600); err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	record.Ready = true
	if err := savePreparedVPKBundle(root, record); err != nil {
		return nil, err
	}
	progress(100, "文件包已准备完成，可加入安装队列")
	return structToMap(record.PreparedVPKBundle)
}

func (e *Executor) consumePreparedVPKBundle(ctx context.Context, job domain.Job, discard bool, progress ProgressFunc) (map[string]any, error) {
	id := exactPayloadString(job.Payload, "prepared_package_id")
	if !preparedID(id) {
		return nil, errors.New("invalid prepared bundle ID")
	}
	release, err := e.locks.acquire(ctx, "prepared:"+id)
	if err != nil {
		return nil, err
	}
	defer release()
	e.preparedMu.Lock()
	root, err := ensureDataSubdirectory(e.config.DataDir, "prepared-vpk-bundles")
	if err != nil {
		e.preparedMu.Unlock()
		return nil, err
	}
	record, err := readPreparedVPKBundle(root, id)
	if err != nil || record.NodeID != job.NodeID || record.NodeID != e.config.NodeID || record.InstanceID != job.InstanceID || e.preparedActive[id] || record.Claim != "" {
		e.preparedMu.Unlock()
		return nil, errors.New("预下载文件包已失效、被使用或不属于此实例")
	}
	if !discard && (!record.Ready || !record.ExpiresAt.After(time.Now()) || record.SHA256 != exactPayloadString(job.Payload, "expected_sha256") || record.Size != int64Payload(job.Payload, "expected_size") || record.Name != exactPayloadString(job.Payload, "name")) {
		e.preparedMu.Unlock()
		return nil, errors.New("prepared bundle metadata mismatch or expired")
	}
	record.Claim = job.ID
	if err := savePreparedVPKBundle(root, record); err != nil {
		e.preparedMu.Unlock()
		return nil, err
	}
	if e.preparedActive == nil {
		e.preparedActive = make(map[string]bool)
	}
	e.preparedActive[id] = true
	e.preparedMu.Unlock()
	defer func() {
		e.preparedMu.Lock()
		defer e.preparedMu.Unlock()
		delete(e.preparedActive, id)
		_ = removePreparedVPKBundle(root, record)
	}()
	if discard {
		return map[string]any{"discarded": true}, nil
	}
	instance, ok := e.registry.Get(job.InstanceID)
	if !ok || instance.ExternalPID > 0 {
		return nil, errors.New("instance is unavailable or externally managed")
	}
	instancePath, err := e.instancePath(instance)
	if err != nil {
		return nil, err
	}
	files := make([]validatedQQVPK, 0, len(record.Files))
	for index, file := range record.Files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		local := filepath.Join(root, preparedBundleFile(id, index))
		sha, size, err := hashRegularFileWithinNoFollow(root, preparedBundleFile(id, index), domain.PreparedPackageMaxBytes)
		if err != nil || sha != file.SHA256 || size != file.Size {
			return nil, errors.New("prepared VPK bundle content changed")
		}
		version, sha, err := validateVPK(local)
		if err != nil || sha != file.SHA256 || version != file.Version {
			return nil, errors.New("prepared VPK bundle failed validation")
		}
		target, err := domain.PreparedVPKBundleTarget(id, index, file.Name)
		if err != nil {
			return nil, err
		}
		files = append(files, validatedQQVPK{input: qqPackageVPK{Path: local}, name: file.Name, path: target, version: version, sha256: sha, size: size})
	}
	return commitQQFlashPackages(ctx, instancePath, files, progress)
}
