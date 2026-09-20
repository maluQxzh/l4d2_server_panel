package domain

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPreparedVPKBundleManifestLimits(t *testing.T) {
	for _, scenario := range []string{"valid", "traversal", "device", "ads", "file-size", "total-size", "file-count", "node-binding", "version"} {
		t.Run(scenario, func(t *testing.T) {
			bundle := PreparedVPKBundle{PreparedPackage: PreparedPackage{ID: uuid.NewString(), NodeID: "node", InstanceID: "instance", Kind: PreparedVPKBundleKind, Name: PreparedVPKBundleName, ExpiresAt: time.Now().UTC().Add(time.Hour), Size: 1}, Files: []PreparedVPKFile{{Name: "file.vpk", SHA256: strings.Repeat("a", 64), Size: 1, Version: 1}}}
			switch scenario {
			case "traversal":
				bundle.Files[0].Name = "../file.vpk"
			case "device":
				bundle.Files[0].Name = "CON.vpk"
			case "ads":
				bundle.Files[0].Name = "file:stream.vpk"
			case "file-size":
				bundle.Files[0].Size = PreparedPackageMaxBytes + 1
				bundle.Size = bundle.Files[0].Size
			case "total-size":
				bundle.Files[0].Size = PreparedPackageMaxBytes
				for i := 0; i < 4; i++ {
					bundle.Files = append(bundle.Files, bundle.Files[0])
				}
				bundle.Size = 5 * PreparedPackageMaxBytes
			case "file-count":
				bundle.Files = make([]PreparedVPKFile, 1001)
			case "version":
				bundle.Files[0].Version = 3
			}
			bundle.SHA256 = bundle.Digest()
			if scenario == "node-binding" {
				bundle.NodeID = "another"
			}
			err := bundle.Validate()
			if (err == nil) != (scenario == "valid") {
				t.Fatalf("validation=%v", err)
			}
		})
	}
}
