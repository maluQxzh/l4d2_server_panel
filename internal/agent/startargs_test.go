package agent

import (
	"reflect"
	"testing"
)

func TestBuildStartArgsUsesArgumentVector(t *testing.T) {
	got, err := BuildStartArgs(StartOptions{Port: 27015, TVPort: 27020, MaxPlayers: 8, Map: "c1m1_hotel", Mode: "coop", ExtraArgs: []string{"-secure"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-console", "-game", "left4dead2", "-port", "27015", "+tv_port", "27020", "+map", "c1m1_hotel", "+mp_gamemode", "coop", "+maxplayers", "8", "+exec", "l4d2panel.cfg", "-secure"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
}

func TestStartArgsRejectManagedConflictsAndInjection(t *testing.T) {
	for _, extra := range [][]string{
		{"-tickrate"},
		{"-tickrate", "0"},
		{"-tickrate=1001"},
		{"-tickrate", "fast"},
		{"-tickrate", "100", "-tickrate", "60"},
		{"+rcon_password", "secret"},
		{"+rcon_password secret"},
		{"+sv_password\tsecret"},
		{"-port=27016"},
		{"-port 27016"},
		{"-basedir", "C:/Windows"},
		{`-basedir C:\escape`},
		{"-defaultgamedir=other"},
		{"+plugin_load addons/evil"},
		{"+hostname", "safe; quit"},
	} {
		if _, err := BuildStartArgs(StartOptions{Port: 27015, MaxPlayers: 8, ExtraArgs: extra}); err == nil {
			t.Errorf("expected %#v to fail", extra)
		}
	}
	if err := ValidateStartArgs([]string{"-console", "-game", "other"}); err == nil {
		t.Fatal("expected alternate game directory to fail")
	}
	if err := ValidateStartArgs([]string{"-console", "+rcon_password", "secret"}); err == nil {
		t.Fatal("expected raw RCON password to fail")
	}
	if err := ValidateStartArgs([]string{"-console", "+sv_password", "secret"}); err == nil {
		t.Fatal("expected raw server password to fail")
	}
	if err := ValidateStartArgs([]string{"-console", "+plugin_load", "addons/evil"}); err == nil {
		t.Fatal("expected native plugin loader to fail")
	}
	for _, argument := range []string{"+rcon_password secret", "+sv_password=secret", "+plugin_load\taddons/evil", `-basedir C:\escape`, "-defaultgamedir=other"} {
		if err := ValidateStartArgs([]string{"-console", "-game", "left4dead2", argument}); err == nil {
			t.Errorf("ValidateStartArgs accepted packed reserved argument %q", argument)
		}
	}
}

func TestBuildStartArgsRejectsManagedSecretOverrides(t *testing.T) {
	for _, argument := range []string{"+sv_password=hunter2", "+plugin_load=evil"} {
		if _, err := BuildStartArgs(StartOptions{Port: 27015, MaxPlayers: 8, Map: "c1m1_hotel", Mode: "coop", ExtraArgs: []string{argument}}); err == nil {
			t.Fatalf("BuildStartArgs accepted %q", argument)
		}
	}
}

func TestBuildStartArgsKeepsSafePackedArgumentsAvailable(t *testing.T) {
	for _, argument := range []string{"-secure=true", "-tickrate=100", "+sv_tags=coop campaign", "+hostname Community Server"} {
		if _, err := BuildStartArgs(StartOptions{Port: 27015, MaxPlayers: 8, Map: "c1m1_hotel", Mode: "coop", ExtraArgs: []string{argument}}); err != nil {
			t.Fatalf("BuildStartArgs rejected safe argument %q: %v", argument, err)
		}
	}
	if _, err := BuildStartArgs(StartOptions{Port: 27015, MaxPlayers: 8, ExtraArgs: []string{"-tickrate", "60"}}); err != nil {
		t.Fatalf("BuildStartArgs rejected separate tickrate value: %v", err)
	}
	for _, args := range [][]string{
		{"-console", "-game=left4dead2", "+exec=l4d2panel.cfg"},
		{"-console", "-game left4dead2", "+exec l4d2panel.cfg"},
	} {
		if err := ValidateStartArgs(args); err != nil {
			t.Fatalf("ValidateStartArgs rejected safe managed arguments %#v: %v", args, err)
		}
	}
}

func TestSteamCMDArgumentsAreSeparated(t *testing.T) {
	got := BuildSteamCMDArgs(`D:\servers\A server`, L4D2DedicatedAppID, true)
	want := []string{"+force_install_dir", `D:\servers\A server`, "+login", "anonymous", "+app_update", "222860", "validate", "+quit"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
}

func TestNormalizeJobTypeAliases(t *testing.T) {
	want := map[string]string{"toggle_package": "toggle_package", "rollback_package": "rollback_package", "install_package": "install_package", "kick_player": "kick_player", "ban_player": "ban_player", "unban_player": "unban_player", "change_map": "change_map", "apply_config": "configure", "query_players": "rcon.players"}
	for input, expected := range want {
		if got := normalizeJobType(input); got != expected {
			t.Errorf("normalizeJobType(%q) = %q, want %q", input, got, expected)
		}
	}
}
