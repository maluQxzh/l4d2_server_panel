package domain

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPreparedWorkshopManifestPreservesOpaqueLargePayloads(t *testing.T) {
	for _, scenario := range []string{"valid-large", "size", "total", "duplicate", "path", "id", "missing", "hash", "binding", "count", "item-count", "device", "ads", "dots"} {
		t.Run(scenario, func(t *testing.T) {
			bundle := PreparedWorkshop{PreparedPackage: PreparedPackage{ID: uuid.NewString(), NodeID: "node", InstanceID: "instance", Name: PreparedWorkshopName, Kind: PreparedWorkshopKind, ExpiresAt: time.Now().Add(time.Hour), Size: 600 << 20}, SourceID: "123", SourceIDs: []string{"123"}, Files: []PreparedWorkshopFile{{SourceID: "123", Name: "123_Addon.vpk", SHA256: strings.Repeat("a", 64), Size: 600 << 20}}}
			switch scenario {
			case "size":
				bundle.Files[0].Size = PreparedWorkshopMaxBytes + 1
			case "total":
				bundle.Size++
			case "duplicate":
				bundle.Files = append(bundle.Files, bundle.Files[0])
				bundle.Size *= 2
			case "path":
				bundle.Files[0].Name = "../123_Addon.vpk"
			case "id":
				bundle.SourceID = "123+quit"
			case "missing":
				bundle.Collection = true
				bundle.SourceIDs = append(bundle.SourceIDs, "456")
			case "hash":
				bundle.Files[0].SHA256 = "bad"
			case "binding":
				bundle.InstanceID = ""
			case "count":
				bundle.Files = make([]PreparedWorkshopFile, PreparedWorkshopMaxFiles+1)
			case "item-count":
				bundle.SourceIDs = make([]string, 201)
			case "device":
				bundle.Files[0].Name = "CON.vpk"
			case "ads":
				bundle.Files[0].Name = "123_Addon:stream.vpk"
			case "dots":
				bundle.Files[0].Name = "123_Add.on.vpk"
			}
			bundle.SHA256 = bundle.Digest()
			if err := bundle.Validate(); (err == nil) != (scenario == "valid-large") {
				t.Fatalf("unexpected validation: %v", err)
			}
		})
	}
}
