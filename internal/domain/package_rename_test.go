package domain

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
)

func TestPackageRenamePaths(t *testing.T) {
	for _, kind := range []string{"vpk", "workshop", "plugin", "smx"} {
		dir, ext, disabled := "left4dead2/addons", ".vpk", "/panel-disabled"
		if kind == "plugin" || kind == "smx" {
			dir, ext, disabled = "left4dead2/addons/sourcemod/plugins", ".smx", "/disabled"
		}
		for _, state := range []string{"", disabled} {
			target, old, next, err := PackageRenamePaths(kind, dir+state+"/old"+ext, "中文 新名称"+ext)
			if err != nil || target != dir+state+"/中文 新名称"+ext || old != dir+"/old"+ext || next != dir+"/中文 新名称"+ext {
				t.Fatalf("%s: %q %q %q %v", kind, target, old, next, err)
			}
		}
	}
	for _, name := range []string{"", "../a.vpk", `a\b.vpk`, "C:a.vpk", "CON.vpk", "COM¹.vpk", "lpt9.foo.vpk", "a.vpk ", "a.vpk.", "a.smx", "a\x00.vpk", "a\u202e.vpk", "l4d2panel-map-x.vpk", strings.Repeat("中", 81) + ".vpk", "OLD.vpk"} {
		if _, _, _, err := PackageRenamePaths("vpk", "left4dead2/addons/old.vpk", name); err == nil {
			t.Errorf("accepted unsafe name %q", name)
		}
	}
	for _, source := range []string{"left4dead2/addons/../old.vpk", "/left4dead2/addons/old.vpk", "left4dead2/addons/workshop/old.vpk", "left4dead2/addons/l4d2panel-map-a.vpk"} {
		if _, _, _, err := PackageRenamePaths("vpk", source, "new.vpk"); err == nil {
			t.Errorf("accepted unsafe source %q", source)
		}
	}
}

func TestRenamedPackageSnapshotRejectsForeignPaths(t *testing.T) {
	key := func(value string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(value))) }
	base := strings.Repeat("a", 64) + ".bin"
	storage := "package-versions/" + key("instance") + "/" + key("old") + "/" + base
	got, err := RenamedPackageSnapshot("instance", "old", "new", storage)
	if err != nil || got != "package-versions/"+key("instance")+"/"+key("new")+"/"+base {
		t.Fatalf("rename snapshot: %s %v", got, err)
	}
	for _, bad := range []string{base, "../" + storage, strings.Replace(storage, key("instance"), key("other"), 1), strings.Replace(storage, key("old"), key("other"), 1)} {
		if _, err := RenamedPackageSnapshot("instance", "old", "new", bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
