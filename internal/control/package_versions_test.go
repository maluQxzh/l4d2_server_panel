package control

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func TestParsePackageVersionMetadataRejectsUntrustedPaths(t *testing.T) {
	sha := strings.Repeat("a", 64)
	key := strings.Repeat("b", 64)
	job := strings.Repeat("c", 64)
	target := "left4dead2/addons/campaign.vpk"
	valid := map[string]any{
		"storage_path": "package-versions/" + key + "/" + key + "/" + job + ".bin",
		"target_path":  target, "sha256": sha, "size": float64(42),
	}
	metadata, ok := parsePackageVersionMetadata(valid, target)
	if !ok || metadata.Size != 42 || metadata.SHA256 != sha {
		t.Fatalf("valid package metadata was rejected: %#v %v", metadata, ok)
	}
	for _, path := range []string{"../../outside.bin", "package-versions/../outside.bin", "package-versions/not-a-key/file.bin"} {
		candidate := make(map[string]any, len(valid))
		for name, value := range valid {
			candidate[name] = value
		}
		candidate["storage_path"] = path
		if _, accepted := parsePackageVersionMetadata(candidate, target); accepted {
			t.Fatalf("unsafe storage path %q was accepted", path)
		}
	}
}

func TestPackageResultPathAllowlist(t *testing.T) {
	for _, candidate := range []struct{ kind, path string }{
		{"vpk", "left4dead2/addons/campaign.vpk"},
		{"plugin", "left4dead2/addons/sourcemod/plugins/welcome.smx"},
	} {
		if err := validateVersionedPackageResultPath(candidate.kind, candidate.path); err != nil {
			t.Errorf("safe target was rejected: %v", err)
		}
	}
	for _, candidate := range []struct{ kind, path string }{
		{"vpk", "left4dead2/cfg/campaign.vpk"},
		{"plugin", "left4dead2/addons/sourcemod/extensions/welcome.smx"},
		{"native", "left4dead2/addons/sourcemod/extensions/unsafe.dll"},
	} {
		if err := validateVersionedPackageResultPath(candidate.kind, candidate.path); err == nil {
			t.Errorf("unsafe/unsupported target was accepted: %#v", candidate)
		}
	}
}

func TestGenericPackageRollbackEndpointIsRejected(t *testing.T) {
	server := &Server{}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/instances/example/actions",
		strings.NewReader(`{"type":"rollback_package","payload":{"snapshot_path":"injected"}}`))
	request = request.WithContext(context.WithValue(request.Context(), principalContextKey, Principal{
		User: domain.User{ID: "owner", Role: domain.RoleOwner},
	}))
	response := httptest.NewRecorder()
	server.handleInstanceAction(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "version-scoped") {
		t.Fatalf("generic rollback returned %d: %s", response.Code, response.Body.String())
	}
}
