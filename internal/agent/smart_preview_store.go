package agent

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

const smartPreviewDirectory = "smart-previews-v2"
const smartPreviewMetadataLimit = 24 << 20 // 5000 bounded source paths plus digests.

// Persist only input identity, never cached target paths, ownership or approvals.
// The credential authenticates local metadata with domain separation. Rotating
// it invalidates this optional cache; the original archive workflow still works.
type smartPreviewRecord struct {
	Version            int                 `json:"version"`
	ID                 string              `json:"id"`
	NodeID             string              `json:"node_id,omitempty"`
	InstanceID         string              `json:"instance_id,omitempty"`
	InstancePathSHA256 string              `json:"instance_path_sha256,omitempty"`
	RequestKey         string              `json:"request_key"`
	CreatedAt          time.Time           `json:"created_at"`
	ExpiresAt          time.Time           `json:"expires_at"`
	ArchiveName        string              `json:"archive_name"`
	ArchiveSHA256      string              `json:"archive_sha256"`
	PathEncoding       string              `json:"path_encoding"`
	Fingerprint        string              `json:"fingerprint"`
	ExpandedBytes      int64               `json:"expanded_bytes"`
	Format             string              `json:"format"`
	PlanSHA256         string              `json:"plan_sha256,omitempty"`
	Files              []smartPreviewInput `json:"files"`
	Claimed            bool                `json:"claimed"`
	MAC                string              `json:"mac"`
}

type smartPreviewInput struct {
	Source string `json:"source"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

func smartPreviewDataFile(id string, index int) string {
	return fmt.Sprintf("%s-%04d.bin", id, index+1)
}

func smartPreviewPlanFile(id string) string { return id + ".plan" }

const smartPreviewPlanLimit = 24 << 20

func saveSmartPreviewPlan(root, id string, plan smartPluginPlan) (string, error) {
	if !preparedID(id) || plan.Version != smartPluginPlanVersion || len(plan.Folders) == 0 || plan.FileCount <= 0 || plan.ExpandedBytes < 0 || plan.ExpandedBytes > maxSmartPluginExpanded {
		return "", errors.New("invalid smart preview plan")
	}
	// Target existence, hashes and package ownership are observations from the
	// inspection moment, never installation authorization. Keep only the
	// stable mapping choices needed to render/rebuild the preview after restart.
	persisted := plan
	persisted.Folders = append([]smartPluginFolder(nil), plan.Folders...)
	for folderIndex := range persisted.Folders {
		persisted.Folders[folderIndex].Conflicts = 0
		persisted.Folders[folderIndex].Files = append([]smartPluginFile(nil), plan.Folders[folderIndex].Files...)
		for fileIndex := range persisted.Folders[folderIndex].Files {
			file := &persisted.Folders[folderIndex].Files[fileIndex]
			file.Existing = false
			file.ExistingSHA256 = ""
			file.ManagedPackageID = ""
			file.PathStates = nil
		}
	}
	raw, err := json.Marshal(persisted)
	if err != nil || len(raw) > smartPreviewPlanLimit {
		return "", errors.New("smart preview plan exceeds limit")
	}
	if _, err := writeFileWithinNoFollow(root, smartPreviewPlanFile(id), bytes.NewReader(raw), managedRawWriteOptions(len(raw), 0600)); err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

func readSmartPreviewPlan(root, id, expected string) (smartPluginPlan, error) {
	var plan smartPluginPlan
	if !preparedID(id) || !validSmartPreviewDigest(expected) {
		return plan, errors.New("invalid smart preview plan binding")
	}
	raw, err := readRegularFileWithinNoFollow(root, smartPreviewPlanFile(id), smartPreviewPlanLimit)
	if err != nil {
		return plan, err
	}
	digest := sha256.Sum256(raw)
	if !strings.EqualFold(hex.EncodeToString(digest[:]), expected) {
		return plan, errors.New("smart preview plan changed")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&plan); err != nil {
		return plan, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF || plan.Version != smartPluginPlanVersion || len(plan.Folders) == 0 || plan.FileCount <= 0 || plan.ExpandedBytes < 0 || plan.ExpandedBytes > maxSmartPluginExpanded {
		return plan, errors.New("invalid smart preview plan")
	}
	return plan, nil
}

func smartPreviewMAC(record smartPreviewRecord, credential string) string {
	record.MAC = ""
	raw, _ := json.Marshal(record)
	mac := hmac.New(sha256.New, []byte(credential))
	_, _ = mac.Write([]byte("l4d2panel/smart-preview/v1\x00"))
	_, _ = mac.Write(raw)
	return hex.EncodeToString(mac.Sum(nil))
}

func validSmartPreviewDigest(value string) bool {
	return len(value) == 64 && strings.Trim(value, "0123456789abcdef") == ""
}

func (record smartPreviewRecord) validate(id string) error {
	if !preparedID(id) || record.ID != id || record.Version != 1 || !validSmartPreviewDigest(record.RequestKey) ||
		!validSmartPreviewDigest(record.Fingerprint) || !validSmartPreviewDigest(record.ArchiveSHA256) ||
		record.CreatedAt.IsZero() || !record.ExpiresAt.Equal(record.CreatedAt.Add(smartPreviewTTL)) ||
		len(record.Files) == 0 || len(record.Files) > maxSmartPluginFiles ||
		record.ExpandedBytes < 0 || record.ExpandedBytes > maxSmartPluginExpanded {
		return errors.New("invalid smart preview metadata")
	}
	if len(record.NodeID) > 256 || len(record.InstanceID) > 256 || strings.ContainsAny(record.NodeID+record.InstanceID, "\x00\r\n") {
		return errors.New("invalid smart preview binding")
	}
	if _, err := validateMapArchivePath(record.ArchiveName); err != nil || strings.Contains(record.ArchiveName, "/") {
		return errors.New("invalid smart preview archive name")
	}
	if record.PlanSHA256 != "" && !validSmartPreviewDigest(record.PlanSHA256) {
		return errors.New("invalid smart preview plan digest")
	}
	encoding, err := normalizeMapPathEncoding(record.PathEncoding)
	if err != nil || encoding != record.PathEncoding || (record.Format != "zip" && record.Format != "7z" && record.Format != "rar") {
		return errors.New("invalid smart preview archive format")
	}
	seen := make(map[string]bool, len(record.Files))
	var total int64
	for _, file := range record.Files {
		clean, err := validateMapArchivePath(file.Source)
		if err != nil || clean != file.Source || !utf8.ValidString(file.Source) || strings.ContainsRune(file.Source, utf8.RuneError) ||
			seen[strings.ToLower(clean)] || !validSmartPreviewDigest(file.SHA256) || file.Size < 0 || file.Size > maxSmartPluginEntry || total > maxSmartPluginExpanded-file.Size {
			return errors.New("invalid smart preview file manifest")
		}
		seen[strings.ToLower(clean)] = true
		total += file.Size
	}
	if total != record.ExpandedBytes {
		return errors.New("smart preview expanded size mismatch")
	}
	return nil
}

func saveSmartPreviewRecord(root string, record smartPreviewRecord, credential string) error {
	if credential == "" {
		return errors.New("smart preview persistence requires an enrolled Agent")
	}
	if err := record.validate(record.ID); err != nil {
		return err
	}
	record.MAC = smartPreviewMAC(record, credential)
	raw, err := json.Marshal(record)
	if err != nil || len(raw) > smartPreviewMetadataLimit {
		return errors.New("smart preview metadata exceeds limit")
	}
	_, err = writeFileWithinNoFollow(root, record.ID+".json", bytes.NewReader(raw), managedRawWriteOptions(len(raw), 0600))
	return err
}

func readSmartPreviewRecord(root, id, credential string) (smartPreviewRecord, error) {
	var record smartPreviewRecord
	if !preparedID(id) || credential == "" {
		return record, errors.New("invalid smart preview identity")
	}
	raw, err := readRegularFileWithinNoFollow(root, id+".json", smartPreviewMetadataLimit)
	if err != nil {
		return record, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return record, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return record, errors.New("trailing smart preview metadata")
	}
	if !hmac.Equal([]byte(record.MAC), []byte(smartPreviewMAC(record, credential))) {
		return record, errors.New("smart preview authentication failed")
	}
	return record, record.validate(id)
}

// Called once under smartPreviewMu before any new preparation. Data filenames
// are derived from the record's own UUID and ordinal, never read from metadata.
func (e *Executor) restoreSmartPreviewsLocked(root string, now time.Time) error {
	if e.smartPreviewsLoaded {
		return nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	loaded := make(map[string]*smartPreview)
	for _, entry := range entries {
		id := strings.TrimSuffix(entry.Name(), ".json")
		if !strings.HasSuffix(entry.Name(), ".json") || !preparedID(id) {
			continue
		}
		record, err := readSmartPreviewRecord(root, id, e.config.Credential)
		if err != nil || record.Claimed || !record.ExpiresAt.After(now) || record.CreatedAt.After(now) || len(loaded) >= smartPreviewSlots {
			// Invalid/rotated credentials discard optional preparation, using
			// only this observed fixed filename. Never trust a rejected manifest.
			if err := deleteManagedFileNoFollow(root, entry.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			continue
		}
		sources := make(map[string]string, len(record.Files))
		for index, file := range record.Files {
			sources[file.Source] = smartPreviewDataFile(id, index)
		}
		plan := smartPluginPlan{ArchiveName: record.ArchiveName, ArchiveSHA256: record.ArchiveSHA256, PathEncoding: record.PathEncoding,
			Format: record.Format, Fingerprint: record.Fingerprint, FileCount: len(record.Files), ExpandedBytes: record.ExpandedBytes}
		if record.PlanSHA256 != "" {
			persistedPlan, planErr := readSmartPreviewPlan(root, id, record.PlanSHA256)
			if planErr != nil || persistedPlan.Fingerprint != record.Fingerprint || persistedPlan.ArchiveSHA256 != record.ArchiveSHA256 {
				if err := deleteManagedFileNoFollow(root, entry.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
				continue
			}
			plan = persistedPlan
		}
		loaded[id] = &smartPreview{requestKey: record.RequestKey, sources: sources, expires: record.ExpiresAt, ready: true, record: record, plan: plan}
	}
	e.smartPreviews, e.smartPreviewsLoaded = loaded, true
	return nil
}

func smartPreviewStagingName(name string) bool {
	if strings.HasSuffix(name, ".json") && preparedID(strings.TrimSuffix(name, ".json")) {
		return true
	}
	if strings.HasSuffix(name, ".plan") && preparedID(strings.TrimSuffix(name, ".plan")) {
		return true
	}
	if len(name) != 45 || !preparedID(name[:36]) {
		return false
	}
	var index int
	if _, err := fmt.Sscanf(name[36:], "-%04d.bin", &index); err != nil || index < 1 || index > maxSmartPluginFiles {
		return false
	}
	return name == smartPreviewDataFile(name[:36], index-1)
}

func (e *Executor) cleanupLegacySmartPreviews() error {
	root, err := ensureDataSubdirectory(e.config.DataDir, "smart-previews")
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if !((strings.HasSuffix(name, ".bin") && preparedID(strings.TrimSuffix(name, ".bin"))) || strings.HasPrefix(name, ".managed-")) {
			return errors.New("unexpected legacy smart preview staging entry")
		}
		if err := deleteManagedFileNoFollow(root, name); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
