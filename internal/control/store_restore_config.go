package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

// restoredLaunchConfig mirrors the secret-free, port-independent launch
// subset carried by a v2 Agent backup. Server/RCON passwords, public exposure,
// maintenance policy and node ports are deliberately not representable here.
type restoredLaunchConfig struct {
	Hostname     string            `json:"hostname,omitempty"`
	Mode         string            `json:"mode"`
	Difficulty   string            `json:"difficulty,omitempty"`
	MaxPlayers   int               `json:"max_players"`
	StartMap     string            `json:"start_map"`
	Region       string            `json:"region,omitempty"`
	SteamGroupID string            `json:"steam_group_id,omitempty"`
	ExtraArgs    []string          `json:"extra_args,omitempty"`
	CVars        map[string]string `json:"cvars,omitempty"`
	AutoRestart  bool              `json:"auto_restart"`
}

func decodeRestoredLaunchConfig(value any) (*restoredLaunchConfig, error) {
	if value == nil {
		return nil, nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode restored launch configuration: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var restored restoredLaunchConfig
	if err := decoder.Decode(&restored); err != nil {
		return nil, errors.New("invalid restored launch configuration")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("invalid restored launch configuration")
	}
	if restored.MaxPlayers < 1 || restored.MaxPlayers > 32 ||
		!sourceConfigIdentifier.MatchString(restored.Mode) || !sourceConfigIdentifier.MatchString(restored.StartMap) {
		return nil, errors.New("restored mode, start_map, or max_players is invalid")
	}
	seen := make(map[string]bool, len(restored.CVars))
	for name := range restored.CVars {
		folded := strings.ToLower(name)
		if seen[folded] {
			return nil, fmt.Errorf("restored configuration contains duplicate CVar %q", name)
		}
		seen[folded] = true
		switch folded {
		case "hostname", "z_difficulty", "sv_region", "sv_steamgroup", "rcon_password", "sv_password":
			return nil, fmt.Errorf("restored configuration contains managed or secret CVar %q", name)
		}
	}
	return &restored, nil
}

func mergeRestoredLaunchConfig(target domain.InstanceConfig, restored restoredLaunchConfig) (domain.InstanceConfig, error) {
	merged := target
	merged.Hostname = restored.Hostname
	merged.Mode = restored.Mode
	merged.Difficulty = restored.Difficulty
	merged.MaxPlayers = restored.MaxPlayers
	merged.StartMap = restored.StartMap
	merged.Region = restored.Region
	merged.SteamGroupID = restored.SteamGroupID
	merged.ExtraArgs = append([]string(nil), restored.ExtraArgs...)
	merged.CVars = make(map[string]string, len(restored.CVars))
	for name, value := range restored.CVars {
		merged.CVars[name] = value
	}
	if len(merged.CVars) == 0 {
		merged.CVars = nil
	}
	merged.AutoRestart = restored.AutoRestart
	merged.ServerPassword = ""
	if err := validateInstanceConfig(merged); err != nil {
		return domain.InstanceConfig{}, fmt.Errorf("invalid restored launch configuration: %w", err)
	}
	return merged, nil
}

func restoredLaunchConfigFromResult(result map[string]any) (*restoredLaunchConfig, error) {
	value, exists := result["restored_launch_config"]
	formatVersion := intFromAny(result["format_version"])
	launchRestored, _ := result["launch_config_restored"].(bool)
	if formatVersion == 0 && !launchRestored && (!exists || value == nil) {
		// Legacy Agents and v1 archives did not report portable launch metadata.
		return nil, nil
	}
	if formatVersion == 1 && !launchRestored && (!exists || value == nil) {
		return nil, nil
	}
	if formatVersion != 2 || !launchRestored || !exists || value == nil {
		return nil, errors.New("restored launch configuration lacks a verified v2 backup provenance marker")
	}
	return decodeRestoredLaunchConfig(value)
}

// applyRestoredLaunchConfigTx stores the previous configuration and advances
// the current version in the caller's transaction. The source_job_id marker
// makes the operation safe when an Agent replays an unacknowledged terminal
// journal entry.
func applyRestoredLaunchConfigTx(ctx context.Context, tx pgx.Tx, job domain.Job, restored restoredLaunchConfig) error {
	current, err := scanInstance(tx.QueryRow(ctx, `SELECT `+instanceColumns+` FROM instances WHERE id=$1 FOR UPDATE`, job.InstanceID))
	if err != nil {
		return err
	}
	var alreadyApplied bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM config_versions WHERE source_job_id=$1)`, job.ID).Scan(&alreadyApplied); err != nil {
		return err
	}
	if alreadyApplied {
		return nil
	}
	merged, err := mergeRestoredLaunchConfig(current.Config, restored)
	if err != nil {
		return err
	}
	previous, err := json.Marshal(current.Config)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO config_versions(id,instance_id,version,config,created_by,source_job_id)
		VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(source_job_id) DO NOTHING`, uuid.NewString(), current.ID,
		current.Version, previous, nullableUUID(job.RequestedBy), job.ID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	raw, err := json.Marshal(merged)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE instances SET config=$2,max_players=$3,version=version+1,updated_at=now() WHERE id=$1`,
		current.ID, raw, merged.MaxPlayers); err != nil {
		return err
	}
	return nil
}

// applyRestoreTerminalProjectionTx keeps the job's terminal transition, the
// portable launch configuration, and an uploaded backup's public state in one
// commit. Callers must lock the instance before the job row so a concurrent
// configuration write cannot wait on the partial active-job index while
// holding the inverse lock order.
func applyRestoreTerminalProjectionTx(ctx context.Context, tx pgx.Tx, job domain.Job) error {
	switch job.Status {
	case domain.JobSucceeded:
		restored, err := restoredLaunchConfigFromResult(job.Result)
		if err != nil {
			return err
		}
		if restored != nil {
			if err := applyRestoredLaunchConfigTx(ctx, tx, job, *restored); err != nil {
				return err
			}
		}
		return finishBackupUploadRestoreTx(ctx, tx, job, "succeeded")
	case domain.JobFailed:
		return finishBackupUploadRestoreTx(ctx, tx, job, "failed")
	case domain.JobCanceled:
		if job.InstanceID != "" {
			if _, err := tx.Exec(ctx, `UPDATE instances SET status='degraded',updated_at=now() WHERE id=$1`, job.InstanceID); err != nil {
				return err
			}
		}
		return finishBackupUploadRestoreTx(ctx, tx, job, "failed")
	default:
		return errors.New("restore projection requires a terminal job")
	}
}
