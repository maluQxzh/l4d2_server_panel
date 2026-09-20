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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

// Kept in its own flat directory: older Agents retain their strict v1
// single-file metadata format and cannot mistake a bundle for one file.
type preparedWorkshopRecord struct {
	domain.PreparedWorkshop
	Ready bool   `json:"ready"`
	Claim string `json:"claim,omitempty"`
}

func readPreparedWorkshop(root, id string) (preparedWorkshopRecord, error) {
	var record preparedWorkshopRecord
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
	if record.ID != id || record.NodeID == "" || record.InstanceID == "" || record.Kind != domain.PreparedWorkshopKind || record.Name != domain.PreparedWorkshopName || !domain.ValidWorkshopID(record.SourceID) || record.ExpiresAt.IsZero() {
		return record, errors.New("invalid prepared bundle binding")
	}
	if record.Ready || len(record.Files) > 0 {
		if err := record.Validate(); err != nil {
			return record, err
		}
	}
	return record, nil
}

func savePreparedWorkshop(root string, record preparedWorkshopRecord) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	_, err = writeFileWithinNoFollow(root, record.ID+".json", bytes.NewReader(raw), managedRawWriteOptions(len(raw), 0600))
	return err
}

func removePreparedWorkshop(root string, record preparedWorkshopRecord) error {
	if !preparedID(record.ID) || len(record.Files) > domain.PreparedWorkshopMaxFiles {
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
func (e *Executor) cleanupPreparedWorkshopsLocked(root string, now time.Time) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	sort.SliceStable(entries, func(i, j int) bool {
		return strings.HasSuffix(entries[i].Name(), ".json") && !strings.HasSuffix(entries[j].Name(), ".json")
	})
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasSuffix(name, ".json") && preparedID(strings.TrimSuffix(name, ".json")) {
			id := strings.TrimSuffix(name, ".json")
			if e.preparedActive[id] {
				continue
			}
			record, err := readPreparedWorkshop(root, id)
			if err != nil {
				return errors.New("invalid prepared bundle staging metadata")
			}
			if !record.Ready || record.Claim != "" || !record.ExpiresAt.After(now) {
				if err := removePreparedWorkshop(root, record); err != nil {
					return err
				}
			}
		} else if len(name) == 45 && strings.HasSuffix(name, ".bin") && preparedID(name[:36]) {
			index, err := strconv.Atoi(name[37:41])
			if err != nil || name[36] != '-' || index < 1 || index > domain.PreparedWorkshopMaxFiles {
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

func (e *Executor) cleanupPreparedWorkshops(now time.Time) error {
	e.preparedMu.Lock()
	defer e.preparedMu.Unlock()
	root, err := ensureDataSubdirectory(e.config.DataDir, "prepared-workshop")
	if err != nil {
		return err
	}
	return e.cleanupPreparedWorkshopsLocked(root, now)
}

func (e *Executor) reservePreparedWorkshop(record preparedWorkshopRecord) (string, error) {
	e.preparedMu.Lock()
	defer e.preparedMu.Unlock()
	root, err := ensureDataSubdirectory(e.config.DataDir, "prepared-workshop")
	if err != nil {
		return "", err
	}
	if err := e.cleanupPreparedWorkshopsLocked(root, time.Now()); err != nil {
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
	if count >= domain.PreparedWorkshopSlots {
		return "", errors.New("整包预下载暂存已满，请安装或丢弃已有文件包")
	}
	// Reserve every snapshot's worst case plus one bounded staging allowance.
	// SteamCMD maintains its own cache; its downloads have no VPK size cap.
	if err := ensureArchiveDiskReserve(root, int64(count+2)*domain.PreparedWorkshopMaxBytes, 1<<30, "Workshop snapshot preparation"); err != nil {
		return "", err
	}
	if _, err := os.Lstat(filepath.Join(root, record.ID+".json")); !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("prepared bundle ID already reserved")
	}
	if err := savePreparedWorkshop(root, record); err != nil {
		return "", err
	}
	if e.preparedActive == nil {
		e.preparedActive = make(map[string]bool)
	}
	e.preparedActive[record.ID] = true
	return root, nil
}

func (e *Executor) prepareWorkshop(ctx context.Context, job domain.Job, progress ProgressFunc) (result map[string]any, err error) {
	source := exactPayloadString(job.Payload, "source")
	if !preparedID(job.ID) || job.NodeID != e.config.NodeID || !domain.ValidWorkshopID(source) || exactPayloadString(job.Payload, "source_transport") != "" || exactPayloadString(job.Payload, "content_base64") != "" || exactPayloadString(job.Payload, "name") != domain.PreparedWorkshopName {
		return nil, errors.New("invalid prepared Workshop request")
	}
	if _, ok := e.registry.Get(job.InstanceID); !ok {
		return nil, errors.New("instance is not registered")
	}
	release, err := e.acquirePreparationSlot(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	record := preparedWorkshopRecord{PreparedWorkshop: domain.PreparedWorkshop{PreparedPackage: domain.PreparedPackage{
		ID: job.ID, NodeID: job.NodeID, InstanceID: job.InstanceID, Kind: domain.PreparedWorkshopKind, Name: domain.PreparedWorkshopName, ExpiresAt: time.Now().UTC().Add(domain.PreparedPackageTTL),
	}, SourceID: source}}
	root, err := e.reservePreparedWorkshop(record)
	if err != nil {
		return nil, err
	}
	defer func() {
		e.preparedMu.Lock()
		defer e.preparedMu.Unlock()
		delete(e.preparedActive, job.ID)
		if err != nil {
			_ = removePreparedWorkshop(root, record)
		}
	}()
	// Each item's snapshot is copied while holding its download lock. SteamCMD
	// still enforces the node-level limit, including game maintenance jobs.
	metadata, err := e.visitWorkshopDownloads(ctx, source, progress, func(download workshopDownload) error {
		baseName, err := workshopInstalledVPKName(download.itemID, download.title)
		if err != nil {
			return err
		}
		if len(download.files) == 0 || len(download.files) > domain.PreparedWorkshopMaxFiles-len(record.Files) {
			return errors.New("Workshop prepared file limit exceeded")
		}
		for index, sourceFile := range download.files {
			if err := ctx.Err(); err != nil {
				return err
			}
			name := baseName
			if len(download.files) > 1 {
				name = strings.TrimSuffix(baseName, ".vpk") + fmt.Sprintf("_%d.vpk", index+1)
			}
			// No VPK parsing or 512 MiB cap: only bound this optional snapshot.
			in, err := openManagedFileNoFollow(filepath.Dir(sourceFile), filepath.Base(sourceFile))
			if err != nil {
				return err
			}
			info, statErr := in.Stat()
			_ = in.Close()
			if statErr != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > domain.PreparedWorkshopMaxBytes-record.Size {
				return errors.New("Workshop prepared snapshot exceeds 8 GiB")
			}
			sha, size, err := hashRegularFileWithinNoFollow(filepath.Dir(sourceFile), filepath.Base(sourceFile), info.Size())
			if err != nil || size != info.Size() {
				return errors.New("Workshop source changed")
			}
			file := domain.PreparedWorkshopFile{SourceID: download.itemID, Name: name, SHA256: sha, Size: size}
			if _, err := file.Target(); err != nil {
				return err
			}
			// Unfinished records are reservations only; flat orphan filenames
			// allow recovery if interrupted before the final manifest commit.
			if err := copyVerifiedPackageFile(sourceFile, root, preparedBundleFile(job.ID, len(record.Files)), sha, size, domain.PreparedWorkshopMaxBytes, 0600); err != nil {
				return err
			}
			record.Files = append(record.Files, file)
			record.Size += size
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	record.SourceIDs = metadata["source_ids"].([]string)
	record.Collection = metadata["collection"].(bool)
	record.SHA256 = record.Digest()
	if err := record.Validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	record.Ready = true
	if err := savePreparedWorkshop(root, record); err != nil {
		return nil, err
	}
	progress(100, "Workshop 文件包已准备完成，可加入安装队列")
	return structToMap(record.PreparedWorkshop)
}

func (e *Executor) consumePreparedWorkshop(ctx context.Context, job domain.Job, discard bool, progress ProgressFunc) (map[string]any, error) {
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
	root, err := ensureDataSubdirectory(e.config.DataDir, "prepared-workshop")
	if err != nil {
		e.preparedMu.Unlock()
		return nil, err
	}
	record, err := readPreparedWorkshop(root, id)
	if err != nil || record.NodeID != job.NodeID || record.NodeID != e.config.NodeID || record.InstanceID != job.InstanceID || e.preparedActive[id] || record.Claim != "" {
		e.preparedMu.Unlock()
		return nil, errors.New("预下载文件包已失效、被使用或不属于此实例")
	}
	if !discard && (!record.Ready || !record.ExpiresAt.After(time.Now()) || record.SHA256 != exactPayloadString(job.Payload, "expected_sha256") || record.Size != int64Payload(job.Payload, "expected_size") || record.Name != exactPayloadString(job.Payload, "name")) {
		e.preparedMu.Unlock()
		return nil, errors.New("prepared bundle metadata mismatch or expired")
	}
	record.Claim = job.ID
	if err := savePreparedWorkshop(root, record); err != nil {
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
		_ = removePreparedWorkshop(root, record)
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
	for index, file := range record.Files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		sha, size, err := hashRegularFileWithinNoFollow(root, preparedBundleFile(id, index), file.Size)
		if err != nil || sha != file.SHA256 || size != file.Size {
			return nil, errors.New("prepared Workshop content changed")
		}
	}
	progress(65, "正在安装已准备的 Workshop 文件")
	created := []string{}
	committed := false
	defer func() {
		if !committed {
			for i := len(created) - 1; i >= 0; i-- {
				_ = deleteManagedFileNoFollow(instancePath, created[i])
			}
		}
	}()
	installed := []map[string]any{}
	for index, file := range record.Files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		target, _ := file.Target()
		if err := copyVerifiedPackageFile(filepath.Join(root, preparedBundleFile(id, index)), instancePath, target, file.SHA256, file.Size, domain.PreparedWorkshopMaxBytes, 0640); err != nil {
			return nil, err
		}
		created = append(created, target)
		installed = append(installed, map[string]any{"source_id": file.SourceID, "path": target, "sha256": file.SHA256, "size": file.Size, "version": file.SHA256[:12]})
		progress(65+(index+1)*30/len(record.Files), "正在提交 Workshop 文件")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	committed = true
	return map[string]any{"kind": "workshop", "source_id": record.SourceID, "source_ids": record.SourceIDs, "collection": record.Collection, "files": installed}, nil
}
