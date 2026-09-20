package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

const smartPreviewSlots = 4
const smartPreviewTTL = time.Hour

// Active operations remain process-local. Ready inputs have an authenticated
// manifest; targets and permissions are always reconstructed before installation.
type smartPreview struct {
	requestKey string
	plan       smartPluginPlan
	sources    map[string]string
	expires    time.Time
	active     bool
	ready      bool
	record     smartPreviewRecord
}

func smartPreviewRequestKey(job domain.Job, payload operationPayload, instancePath string) string {
	encoding, err := normalizeMapPathEncoding(payload.PathEncoding)
	if err != nil {
		return ""
	}
	// Hash the source locator/upload as well as the target binding. Changing the
	// source must not accidentally reuse an unrelated archive with the same plan.
	contentHash := sha256.New()
	_, _ = io.Copy(contentHash, strings.NewReader(payload.ContentBase64))
	raw, _ := json.Marshal([]string{job.NodeID, job.InstanceID, instancePath, payload.Name,
		strings.ToLower(strings.TrimSpace(payload.SourceTransport)), payload.Source, payload.SourcePath, payload.SourceID,
		hex.EncodeToString(contentHash.Sum(nil)), encoding})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (e *Executor) cleanupSmartPreviews(now time.Time) error {
	e.smartPreviewMu.Lock()
	defer e.smartPreviewMu.Unlock()
	return e.cleanupSmartPreviewsLocked(now)
}

func (e *Executor) removeSmartPreviewLocked(root, id string) error {
	preview := e.smartPreviews[id]
	if preview == nil {
		return nil
	}
	for _, name := range preview.sources {
		if err := deleteManagedFileNoFollow(root, name); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := deleteManagedFileNoFollow(root, id+".json"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	delete(e.smartPreviews, id)
	return nil
}

func (e *Executor) cleanupSmartPreviewsLocked(now time.Time) error {
	root, err := ensureDataSubdirectory(e.config.DataDir, smartPreviewDirectory)
	if err != nil {
		return err
	}
	if err := e.restoreSmartPreviewsLocked(root, now); err != nil {
		return err
	}
	active := false
	keep := make(map[string]bool)
	for id, preview := range e.smartPreviews {
		if !preview.active && (!preview.ready || !preview.expires.After(now)) {
			if err := e.removeSmartPreviewLocked(root, id); err != nil {
				return err
			}
			continue
		}
		active = active || preview.active
		keep[id+".json"] = true
		for _, name := range preview.sources {
			keep[name] = true
		}
	}
	// Atomic writes may still be creating their temporary names. Sweep orphaned
	// flat files only while idle, after restoring valid records on restart.
	if active {
		return nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if keep[name] {
			continue
		}
		if !smartPreviewStagingName(name) && !strings.HasPrefix(name, ".managed-") {
			return errors.New("unexpected smart preview staging entry")
		}
		if err := deleteManagedFileNoFollow(root, name); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (e *Executor) reserveSmartPreview() (string, error) {
	e.smartPreviewMu.Lock()
	defer e.smartPreviewMu.Unlock()
	if err := e.cleanupSmartPreviewsLocked(time.Now()); err != nil {
		return "", err
	}
	if len(e.smartPreviews) >= smartPreviewSlots {
		return "", nil // Optional cache: inspection still works when it is full.
	}
	root, err := ensureDataSubdirectory(e.config.DataDir, smartPreviewDirectory)
	if err != nil {
		return "", err
	}
	// Account for every slot's worst-case expanded data and one extraction in
	// addition to the cached copy. The archive extractor retains its own reserve.
	if err := ensureArchiveDiskReserve(root, int64(len(e.smartPreviews)+2)*maxSmartPluginExpanded, 1<<30, "smart preview"); err != nil {
		return "", nil
	}
	if e.smartPreviews == nil {
		e.smartPreviews = make(map[string]*smartPreview)
	}
	id := uuid.NewString()
	created := time.Now().UTC()
	e.smartPreviews[id] = &smartPreview{active: true, expires: created.Add(smartPreviewTTL), record: smartPreviewRecord{Version: 1, ID: id, CreatedAt: created, ExpiresAt: created.Add(smartPreviewTTL)}}
	return id, nil
}

func (e *Executor) finishSmartPreview(id string) {
	if id == "" {
		return
	}
	e.smartPreviewMu.Lock()
	defer e.smartPreviewMu.Unlock()
	if preview := e.smartPreviews[id]; preview != nil {
		preview.active = false
	}
	if err := e.cleanupSmartPreviewsLocked(time.Now()); err != nil && e.logger != nil {
		e.logger.Printf("smart preview retention: %v", err)
	}
}

func (e *Executor) saveSmartPreview(ctx context.Context, id, requestKey string, plan smartPluginPlan, extracted string) error {
	if id == "" {
		return ctx.Err()
	}
	root, err := ensureDataSubdirectory(e.config.DataDir, smartPreviewDirectory)
	if err != nil {
		return err
	}
	sources := make(map[string]string, plan.FileCount)
	inputs := make([]smartPreviewInput, 0, plan.FileCount)
	for _, folder := range plan.Folders {
		for _, file := range folder.Files {
			sources[file.SourcePath] = smartPreviewDataFile(id, len(inputs))
			inputs = append(inputs, smartPreviewInput{Source: file.SourcePath, SHA256: file.SHA256, Size: file.Size})
		}
	}
	e.smartPreviewMu.Lock()
	preview := e.smartPreviews[id]
	preview.sources = sources
	e.smartPreviewMu.Unlock()
	for _, folder := range plan.Folders {
		for _, file := range folder.Files {
			if err := ctx.Err(); err != nil {
				return err
			}
			in, err := openManagedFileNoFollow(extracted, file.SourcePath)
			if err != nil {
				return err
			}
			info, statErr := in.Stat()
			if statErr != nil || !info.Mode().IsRegular() || info.Size() != file.Size {
				_ = in.Close()
				return errors.New("smart preview source is not an unchanged regular file")
			}
			_, err = writeFileWithinNoFollow(root, sources[file.SourcePath], in, secureWriteOptions{
				Mode: 0600, ExpectedSize: file.Size, ExpectedSHA256: file.SHA256, RejectExistingLink: true,
			})
			closeErr := in.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	e.smartPreviewMu.Lock()
	defer e.smartPreviewMu.Unlock()
	record := preview.record
	record.RequestKey, record.ArchiveName, record.ArchiveSHA256 = requestKey, plan.ArchiveName, plan.ArchiveSHA256
	record.PathEncoding, record.Fingerprint, record.Format = plan.PathEncoding, plan.Fingerprint, plan.Format
	record.ExpandedBytes, record.Files = plan.ExpandedBytes, inputs
	// Unenrolled test/local executors retain the original in-memory optimization.
	if e.config.Credential != "" {
		if err := saveSmartPreviewRecord(root, record, e.config.Credential); err != nil {
			return err
		}
	}
	preview.requestKey, preview.plan, preview.ready, preview.record = requestKey, plan, true, record
	return nil
}

func (e *Executor) installSmartPreview(ctx context.Context, job domain.Job, payload operationPayload, instancePath string, progress ProgressFunc) (map[string]any, bool, error) {
	key := smartPreviewRequestKey(job, payload, instancePath)
	if key == "" || payload.InspectionFingerprint == "" {
		return nil, false, nil
	}
	e.smartPreviewMu.Lock()
	if err := e.cleanupSmartPreviewsLocked(time.Now()); err != nil {
		e.smartPreviewMu.Unlock()
		return nil, false, err
	}
	var id string
	var preview *smartPreview
	for candidate, cached := range e.smartPreviews {
		if cached.ready && !cached.active && cached.requestKey == key && cached.plan.Fingerprint == payload.InspectionFingerprint {
			id, preview = candidate, cached
			// Commit the one-attempt claim before hashing or modifying the instance.
			// A crash after this point must not resurrect the cached preparation.
			if e.config.Credential != "" {
				root, err := ensureDataSubdirectory(e.config.DataDir, smartPreviewDirectory)
				if err == nil {
					claimed := cached.record
					claimed.Claimed = true
					err = saveSmartPreviewRecord(root, claimed, e.config.Credential)
				}
				if err != nil {
					e.smartPreviewMu.Unlock()
					return nil, true, err
				}
			}
			cached.active, cached.ready = true, false // One attempt, including failure.
			break
		}
	}
	e.smartPreviewMu.Unlock()
	if preview == nil {
		return nil, false, nil
	}
	defer e.finishSmartPreview(id)
	if err := ctx.Err(); err != nil {
		return nil, true, err
	}
	root, err := ensureDataSubdirectory(e.config.DataDir, smartPreviewDirectory)
	if err != nil {
		return nil, true, err
	}
	progress(20, "正在复用预检文件并重新核对内容与目标状态")
	for index, file := range preview.record.Files {
		if err := ctx.Err(); err != nil {
			return nil, true, err
		}
		in, err := openManagedFileNoFollow(root, smartPreviewDataFile(id, index))
		if err != nil {
			return nil, true, err
		}
		info, statErr := in.Stat()
		closeErr := in.Close()
		if statErr != nil || closeErr != nil || !info.Mode().IsRegular() || info.Size() != file.Size {
			return nil, true, errors.New("smart preview input size changed")
		}
	}
	files := make([]extractedMapFile, 0, preview.plan.FileCount)
	for source := range preview.sources {
		files = append(files, extractedMapFile{Relative: source})
	}
	old := preview.plan
	plan, err := buildSmartPluginPlanFromSources(root, old.ArchiveName, old.ArchiveSHA256, old.PathEncoding, old.Format, old.ExpandedBytes, files, instancePath, preview.sources)
	if err != nil {
		return nil, true, err
	}
	if err := ctx.Err(); err != nil {
		return nil, true, err
	}
	// Rebuild hashes and all candidate target states, then use the exact same
	// fingerprint, ownership, takeover, live/native checks and rollback path.
	result, err := e.installSmartPluginPlan(job, payload, plan, root, instancePath, preview.sources, progress)
	return result, true, err
}
