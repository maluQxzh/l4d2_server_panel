package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

func TestMapStoreAssignsVerifiedHardLinkAndRequiresUnassignBeforeDelete(t *testing.T) {
	base := t.TempDir()
	dataDir := filepath.Join(base, "data")
	mapRoot := filepath.Join(dataDir, "maps")
	store, err := newMapStore(&Config{DataDir: dataDir, MapRoots: []string{mapRoot}})
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(base, "output.vpk")
	if err := os.WriteFile(output, []byte("deterministic-map-object"), 0o600); err != nil {
		t.Fatal(err)
	}
	sha, size, err := hashFile(output)
	if err != nil {
		t.Fatal(err)
	}
	mapID := uuid.NewString()
	record, err := store.publish(mapLocalRecord{
		ID:              mapID,
		Name:            "Hard-link test map",
		AggregateSHA256: sha,
		TotalSize:       size,
		Artifacts:       []mapLocalArtifact{{Ordinal: 0, SHA256: sha, Size: size}},
		BSPs:            []mapLocalBSP{{VirtualPath: "maps/test_map.bsp", StartName: "test_map"}},
		Assignments:     make(map[string]mapLocalAssignment),
	}, []string{output})
	if err != nil {
		t.Fatal(err)
	}

	instanceRoot := filepath.Join(base, "instance")
	if err := os.MkdirAll(filepath.Join(instanceRoot, "left4dead2", "addons"), 0o700); err != nil {
		t.Fatal(err)
	}
	instance := InstanceRecord{ID: "instance-1"}
	if err := store.assign(mapID, instance, instanceRoot); err != nil {
		t.Fatal(err)
	}
	assigned, ok := store.registry.get(mapID)
	if !ok || len(assigned.Assignments[instance.ID].Targets) != 1 {
		t.Fatalf("assignment was not persisted: %+v", assigned.Assignments)
	}
	target := assigned.Assignments[instance.ID].Targets[0]
	expectedTarget := filepath.ToSlash(filepath.Join("left4dead2", "addons", "workshop", "l4d2panel-map-"+mapID+"-00.vpk"))
	if target != expectedTarget {
		t.Fatalf("assignment target = %q, want %q", target, expectedTarget)
	}
	sourceID, sourceLinks, err := managedHardLinkIdentity(store.defaultRoot().Path, record.Artifacts[0].Replicas[store.defaultRoot().FilesystemID])
	if err != nil {
		t.Fatal(err)
	}
	targetID, targetLinks, err := managedHardLinkIdentity(instanceRoot, target)
	if err != nil {
		t.Fatal(err)
	}
	if sourceID != targetID || sourceLinks != 2 || targetLinks != 2 {
		t.Fatalf("hard-link identity mismatch: source=%s/%d target=%s/%d", sourceID, sourceLinks, targetID, targetLinks)
	}
	if err := store.delete(mapID); err == nil {
		t.Fatal("assigned map was deleted")
	}
	if err := store.unassign(mapID, instance, instanceRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(instanceRoot, filepath.FromSlash(target)), []byte("deterministic-map-object"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := store.assign(mapID, instance, instanceRoot); err != nil {
		t.Fatalf("adopt restored Workshop VPK: %v", err)
	}
	restoredTargetID, _, err := managedHardLinkIdentity(instanceRoot, target)
	if err != nil {
		t.Fatal(err)
	}
	if restoredTargetID != sourceID {
		t.Fatal("restored Workshop VPK was not replaced with the registered hard link")
	}
	if err := store.unassign(mapID, instance, instanceRoot); err != nil {
		t.Fatal(err)
	}
	sourcePath, err := ResolveWithin(store.defaultRoot().Path, filepath.FromSlash(record.Artifacts[0].Replicas[store.defaultRoot().FilesystemID]))
	if err != nil {
		t.Fatal(err)
	}
	unknownLink := filepath.Join(base, "unknown-map-link.vpk")
	if err := os.Link(sourcePath, unknownLink); err != nil {
		t.Fatal(err)
	}
	if err := store.delete(mapID); err == nil {
		t.Fatal("map with an unknown hard link was deleted")
	}
	if _, err := ManagedMapHardLinks(store.config); err == nil {
		t.Fatal("service-install proof accepted an unknown hard link")
	}
	if err := os.Remove(unknownLink); err != nil {
		t.Fatal(err)
	}
	if err := store.delete(mapID); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.registry.get(mapID); ok {
		t.Fatal("deleted map remains in registry")
	}
}

func TestMapTargetValidationOnlyAcceptsWorkshopDirectory(t *testing.T) {
	mapID := uuid.NewString()
	if !validMapTargetRelative(mapID, mapTargetRelative(mapID, 0)) {
		t.Fatal("Workshop map target was rejected")
	}
	legacy := filepath.ToSlash(filepath.Join("left4dead2", "addons", "l4d2panel-map-"+mapID+"-00.vpk"))
	if validMapTargetRelative(mapID, legacy) || isReservedMapAddonPath(legacy) {
		t.Fatal("legacy addons-root map target was accepted")
	}
	if !isReservedMapAddonPath(mapTargetRelative(mapID, 0)) {
		t.Fatal("Workshop map target was not reserved")
	}
}

func TestMapStoreRejectsCorruptRegistryAndDuplicateFilesystemRoots(t *testing.T) {
	base := t.TempDir()
	corruptRoot := filepath.Join(base, "corrupt", "maps")
	if err := os.MkdirAll(corruptRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(corruptRoot, "registry.json"), []byte(`{"version":1,"maps":{"not-a-uuid":{}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newMapStore(&Config{DataDir: filepath.Join(base, "corrupt"), MapRoots: []string{corruptRoot}}); err == nil {
		t.Fatal("corrupt map registry was accepted")
	}

	firstRoot := filepath.Join(base, "first-root")
	secondRoot := filepath.Join(base, "second-root")
	if _, err := newMapStore(&Config{DataDir: filepath.Join(base, "duplicate"), MapRoots: []string{firstRoot, secondRoot}}); err == nil {
		t.Fatal("two map roots on the same filesystem were accepted")
	}
}
