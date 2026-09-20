package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

const PreparedVPKBundleKind = "qq_vpk_bundle"
const PreparedVPKBundleName = "QQ 闪传 VPK 文件包"
const PreparedVPKBundleMaxBytes int64 = 2 << 30
const PreparedVPKBundleSlots = 4
const PreparedVPKBundleMaxFiles = 1000

type PreparedVPKFile struct {
	Name    string `json:"name"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
	Version int    `json:"vpk_version"`
}

type PreparedVPKBundle struct {
	PreparedPackage
	Files []PreparedVPKFile `json:"files"`
}

func PreparedVPKBundleTarget(id string, index int, name string) (string, error) {
	parsed, err := uuid.Parse(id)
	if err != nil || parsed.String() != id || index < 0 || index >= PreparedVPKBundleMaxFiles || len(name) > 204 {
		return "", errors.New("invalid prepared VPK bundle target")
	}
	if _, err := PreparedPackageTarget("vpk", name); err != nil {
		return "", err
	}
	return fmt.Sprintf("left4dead2/addons/qq_%s_%04d_%s", strings.ReplaceAll(id, "-", "")[:16], index+1, name), nil
}

// The digest binds every file and the node/instance/expiry to the result held by
// Control. Claim and ready state are local and deliberately excluded.
func (bundle PreparedVPKBundle) Digest() string {
	bundle.SHA256 = ""
	raw, _ := json.Marshal(bundle)
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func (bundle PreparedVPKBundle) Validate() error {
	if bundle.Kind != PreparedVPKBundleKind || bundle.Name != PreparedVPKBundleName || bundle.NodeID == "" || bundle.InstanceID == "" || bundle.ExpiresAt.IsZero() ||
		len(bundle.Files) == 0 || len(bundle.Files) > PreparedVPKBundleMaxFiles {
		return errors.New("invalid prepared VPK bundle metadata")
	}
	var size int64
	for index, file := range bundle.Files {
		if _, err := PreparedVPKBundleTarget(bundle.ID, index, file.Name); err != nil {
			return err
		}
		if len(file.SHA256) != 64 || strings.Trim(file.SHA256, "0123456789abcdef") != "" || file.Size <= 0 || file.Size > PreparedPackageMaxBytes ||
			(file.Version != 1 && file.Version != 2) || size > PreparedVPKBundleMaxBytes-file.Size {
			return errors.New("invalid prepared VPK bundle file")
		}
		size += file.Size
	}
	if size != bundle.Size || bundle.SHA256 != bundle.Digest() {
		return errors.New("prepared VPK bundle digest or size mismatch")
	}
	return nil
}
