package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/uuid"
)

const PreparedWorkshopKind = "workshop"
const PreparedWorkshopName = "Workshop 文件包"
const PreparedWorkshopMaxBytes int64 = 8 << 30
const PreparedWorkshopMaxFiles = 1000
const PreparedWorkshopSlots = 4

type PreparedWorkshopFile struct {
	SourceID string `json:"source_id"`
	Name     string `json:"name"`
	SHA256   string `json:"sha256"`
	Size     int64  `json:"size"`
}

type PreparedWorkshop struct {
	PreparedPackage
	SourceID   string                 `json:"source_id"`
	SourceIDs  []string               `json:"source_ids"`
	Collection bool                   `json:"collection"`
	Files      []PreparedWorkshopFile `json:"files"`
}

func ValidWorkshopID(id string) bool {
	return len(id) > 0 && len(id) <= 20 && strings.Trim(id, "0123456789") == ""
}

func (file PreparedWorkshopFile) Target() (string, error) {
	if !ValidWorkshopID(file.SourceID) || !strings.HasPrefix(file.Name, file.SourceID+"_") ||
		!strings.HasSuffix(file.Name, ".vpk") || strings.Contains(strings.TrimSuffix(file.Name, ".vpk"), ".") || len(file.Name) <= len(file.SourceID)+5 {
		return "", errors.New("invalid prepared Workshop filename")
	}
	return PreparedPackageTarget("vpk", file.Name)
}

func (bundle PreparedWorkshop) Digest() string {
	bundle.SHA256 = ""
	raw, _ := json.Marshal(bundle)
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func (bundle PreparedWorkshop) Validate() error {
	id, err := uuid.Parse(bundle.ID)
	if err != nil || id.String() != bundle.ID || bundle.Kind != PreparedWorkshopKind || bundle.Name != PreparedWorkshopName || bundle.NodeID == "" || bundle.InstanceID == "" || bundle.ExpiresAt.IsZero() ||
		!ValidWorkshopID(bundle.SourceID) || len(bundle.SourceIDs) == 0 || len(bundle.SourceIDs) > 200 || len(bundle.Files) == 0 || len(bundle.Files) > PreparedWorkshopMaxFiles ||
		(!bundle.Collection && (len(bundle.SourceIDs) != 1 || bundle.SourceIDs[0] != bundle.SourceID)) {
		return errors.New("invalid prepared Workshop metadata")
	}
	items := make(map[string]bool)
	for _, id := range bundle.SourceIDs {
		if !ValidWorkshopID(id) {
			return errors.New("invalid prepared Workshop item")
		}
		if _, exists := items[id]; exists {
			return errors.New("duplicate prepared Workshop item")
		}
		items[id] = false
	}
	targets := make(map[string]bool)
	var size int64
	for _, file := range bundle.Files {
		target, err := file.Target()
		_, exists := items[file.SourceID]
		if err != nil || !exists || targets[strings.ToLower(target)] || len(file.SHA256) != 64 || strings.Trim(file.SHA256, "0123456789abcdef") != "" || file.Size <= 0 || file.Size > PreparedWorkshopMaxBytes || size > PreparedWorkshopMaxBytes-file.Size {
			return errors.New("invalid prepared Workshop payload")
		}
		targets[strings.ToLower(target)], items[file.SourceID] = true, true
		size += file.Size
	}
	for _, present := range items {
		if !present {
			return errors.New("missing prepared Workshop item")
		}
	}
	if size != bundle.Size || bundle.SHA256 != bundle.Digest() {
		return errors.New("prepared Workshop digest or size mismatch")
	}
	return nil
}
