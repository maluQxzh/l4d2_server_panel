package domain

import "testing"

func TestPreparedExtensionNames(t *testing.T) {
	for _, kind := range []string{"sourcemod", "metamod"} {
		for _, name := range []string{"extension.zip", "extension-linux.tar.gz", "组件.ZIP"} {
			if err := ValidatePreparedPackageName(kind, name); err != nil {
				t.Fatalf("valid %s: %v", name, err)
			}
		}
		for _, name := range []string{"", "../evil.zip", "x/evil.zip", `x\evil.zip`, "C:evil.zip", "CON.zip", "a.zip:ads", "/evil.zip", "file.dll", "file.7z", "file.zip ", "a\u202e.zip"} {
			if err := ValidatePreparedPackageName(kind, name); err == nil {
				t.Fatalf("unsafe name accepted: %s", name)
			}
		}
		if _, err := PreparedPackageTarget(kind, "extension.zip"); err == nil {
			t.Fatal("archive has single-file destination")
		}
	}
}
