package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseSteamAppPublicBuildID(t *testing.T) {
	raw := []byte("log line with an unmatched \" before metadata\n" + `Redirecting stderr to logs/stderr.txt
"222860"
{
	"common" { "name" "Left 4 Dead 2 Dedicated Server" }
	"depots"
	{
		"branches"
		{
			"beta" { "buildid" "111111" }
			"public"
			{
				"buildid" "23456789"
				"timeupdated" "1770000000"
			}
		}
	}
}

func TestSteamAppInfoArgumentsAreSeparatedAndReadOnly(t *testing.T) {
	args := BuildSteamAppInfoArgs(L4D2DedicatedAppID)
	want := []string{"+login", "anonymous", "+app_info_update", "1", "+app_info_print", "222860", "+quit"}
	if len(args) != len(want) {
		t.Fatalf("unexpected args: %#v", args)
	}
	for index := range want {
		if args[index] != want[index] {
			t.Fatalf("arg %d = %q, want %q", index, args[index], want[index])
		}
	}
	}
	for _, arg := range args {
		if arg == "+app_update" || arg == "+force_install_dir" {
			t.Fatalf("read-only query includes mutating argument %q", arg)
		}
	}
}
`)
	buildID, err := ParseSteamAppPublicBuildID(raw, L4D2DedicatedAppID)
	if err != nil {
		t.Fatal(err)
	}
	if buildID != "23456789" {
		t.Fatalf("unexpected build id %q", buildID)
	}
}

func TestParseSteamAppPublicBuildIDRejectsWrongOrMissingBranch(t *testing.T) {
	tests := [][]byte{
		[]byte(`"222860" { "depots" { "branches" { "beta" { "buildid" "12" } } } }`),
		[]byte(`"550" { "depots" { "branches" { "public" { "buildid" "12" } } } }`),
		[]byte(`"222860" { "depots" { "branches" { "public" { "buildid" "not-a-number" } } } }`),
	}
	for _, raw := range tests {
		if _, err := ParseSteamAppPublicBuildID(raw, L4D2DedicatedAppID); err == nil {
			t.Fatalf("expected malformed app info to fail: %s", raw)
		}
	}
}

func TestParseSteamAppManifestBuildID(t *testing.T) {
	raw := []byte(`"AppState"
{
	"appid" "222860"
	"Universe" "1"
	"buildid" "34567890"
}`)
	buildID, err := ParseSteamAppManifestBuildID(raw, L4D2DedicatedAppID)
	if err != nil {
		t.Fatal(err)
	}
	if buildID != "34567890" {
		t.Fatalf("unexpected build id %q", buildID)
	}
	if _, err := ParseSteamAppManifestBuildID(raw, 550); err == nil {
		t.Fatal("expected an app id mismatch")
	}
}

func TestInstalledBuildIDPrefersInstanceManifest(t *testing.T) {
	instanceRoot := t.TempDir()
	steamRoot := t.TempDir()
	steamapps := filepath.Join(instanceRoot, "steamapps")
	if err := os.MkdirAll(steamapps, 0o750); err != nil {
		t.Fatal(err)
	}
	manifest := []byte(`"AppState" { "appid" "222860" "buildid" "45678901" }`)
	if err := os.WriteFile(filepath.Join(steamapps, "appmanifest_222860.acf"), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	steam := NewSteamCMD(filepath.Join(steamRoot, "steamcmd"), 1)
	buildID, source, err := steam.InstalledBuildID(instanceRoot, L4D2DedicatedAppID)
	if err != nil {
		t.Fatal(err)
	}
	if buildID != "45678901" || source != "instance" {
		t.Fatalf("unexpected installed build result %q from %q", buildID, source)
	}
}
