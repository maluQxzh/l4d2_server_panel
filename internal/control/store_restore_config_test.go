package control

import (
	"testing"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func TestMergeRestoredLaunchConfigPreservesTargetPolicy(t *testing.T) {
	target := domain.InstanceConfig{Hostname: "target", Mode: "coop", Difficulty: "normal", MaxPlayers: 8,
		StartMap: "c1m1_hotel", PublicStatus: true, MaintenanceCron: "0 4 * * *", ServerPassword: "must-not-persist"}
	value := map[string]any{"hostname": "restored", "mode": "versus", "difficulty": "hard", "max_players": 12,
		"start_map": "c5m1_waterfront", "region": "3", "steam_group_id": "12345", "extra_args": []any{"-strictportbind"},
		"cvars": map[string]any{"sv_tags": "community"}, "auto_restart": true}
	restored, err := decodeRestoredLaunchConfig(value)
	if err != nil {
		t.Fatal(err)
	}
	merged, err := mergeRestoredLaunchConfig(target, *restored)
	if err != nil {
		t.Fatal(err)
	}
	if merged.Hostname != "restored" || merged.Mode != "versus" || merged.MaxPlayers != 12 || !merged.AutoRestart {
		t.Fatalf("portable settings were not restored: %#v", merged)
	}
	if !merged.PublicStatus || merged.MaintenanceCron != target.MaintenanceCron || merged.ServerPassword != "" {
		t.Fatalf("target policy or secret boundary changed: %#v", merged)
	}
}

func TestDecodeRestoredLaunchConfigRejectsSecretsAndUnknownFields(t *testing.T) {
	base := map[string]any{"mode": "coop", "max_players": 8, "start_map": "c1m1_hotel"}
	for name, mutate := range map[string]func(map[string]any){
		"secret field": func(value map[string]any) { value["server_password"] = "secret" },
		"secret CVar":  func(value map[string]any) { value["cvars"] = map[string]any{"rcon_password": "secret"} },
		"managed CVar": func(value map[string]any) { value["cvars"] = map[string]any{"HOSTNAME": "duplicate"} },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := make(map[string]any, len(base)+1)
			for key, value := range base {
				candidate[key] = value
			}
			mutate(candidate)
			if _, err := decodeRestoredLaunchConfig(candidate); err == nil {
				t.Fatal("unsafe restored launch configuration was accepted")
			}
		})
	}
}

func TestRestoredLaunchConfigRequiresVerifiedV2Result(t *testing.T) {
	launch := map[string]any{"mode": "coop", "max_players": 8, "start_map": "c1m1_hotel"}
	if _, err := restoredLaunchConfigFromResult(map[string]any{"restored_launch_config": launch}); err == nil {
		t.Fatal("launch configuration without v2 provenance was accepted")
	}
	got, err := restoredLaunchConfigFromResult(map[string]any{
		"format_version": 2, "launch_config_restored": true, "restored_launch_config": launch,
	})
	if err != nil || got == nil {
		t.Fatalf("verified v2 launch configuration was rejected: %#v err=%v", got, err)
	}
	if _, err := restoredLaunchConfigFromResult(map[string]any{"format_version": 2, "launch_config_restored": true}); err == nil {
		t.Fatal("v2 result without restored_launch_config was accepted")
	}
	if got, err := restoredLaunchConfigFromResult(map[string]any{"format_version": 1}); err != nil || got != nil {
		t.Fatalf("legacy v1 result was not preserved: %#v err=%v", got, err)
	}
}
