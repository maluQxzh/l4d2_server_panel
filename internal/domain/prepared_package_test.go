package domain

import "testing"

func TestPreparedPackageTargets(t *testing.T) {
	for _, item := range []struct{ kind, name string }{
		{"vpk", "Campaign.VPK"}, {"smx", "插件.smx"}, {"native", "plugin.dll"}, {"native", "plugin.so"},
	} {
		if _, err := PreparedPackageTarget(item.kind, item.name); err != nil {
			t.Fatalf("%#v: %v", item, err)
		}
	}
	for _, name := range []string{"../x.smx", `C:\x.smx`, "CON.smx", "x:ads.smx", "l4d2panel-map-x.smx", "x\u200b.smx", "x.smx.", "LPT1.so", "plugin.exe"} {
		for _, kind := range []string{"vpk", "smx", "native"} {
			if _, err := PreparedPackageTarget(kind, name); err == nil {
				t.Fatalf("unsafe name accepted: %s %s", kind, name)
			}
		}
	}
}
