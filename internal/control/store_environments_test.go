package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func TestRuntimeEnvironmentMigrationRejectsSharedLegacyPath(t *testing.T) {
	ctx := context.Background()
	migrationDir := t.TempDir()
	sourceDir := filepath.Join("..", "..", "migrations", "sqlite")
	copyMigration := func(name string) {
		t.Helper()
		body, err := os.ReadFile(filepath.Join(sourceDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(migrationDir, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"001_control_plane.sql", "002_bootstrap_setup_key.sql", "003_system_update_jobs.sql"} {
		copyMigration(name)
	}
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "control.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx, migrationDir); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO nodes(id,name,status,os,instance_root,created_at) VALUES('node-legacy','legacy','offline','windows','C:\GameRoot',?1);
		INSERT INTO instances(id,node_id,name,status,path,port,config,created_at,updated_at)
		VALUES('instance-one','node-legacy','one','stopped','C:\GameRoot\servers\shared',27015,'{}',?1,?1),
		('instance-two','node-legacy','two','stopped','c:\gameroot\SERVERS\SHARED',27016,'{}',?1,?1)`, now); err != nil {
		t.Fatal(err)
	}
	copyMigration("004_runtime_environments.sql")
	err = store.Migrate(ctx, migrationDir)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "game_installations") || !strings.Contains(strings.ToLower(err.Error()), "unique") {
		t.Fatalf("shared legacy path did not fail with a clear conflict: %v", err)
	}
}

func TestAbsoluteGameInstallationMigrationDropsRelativeRecordsAndRootIndex(t *testing.T) {
	ctx := context.Background()
	migrationDir := t.TempDir()
	sourceDir := filepath.Join("..", "..", "migrations", "sqlite")
	copyMigration := func(name string) {
		t.Helper()
		body, err := os.ReadFile(filepath.Join(sourceDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(migrationDir, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"001_control_plane.sql", "002_bootstrap_setup_key.sql", "003_system_update_jobs.sql", "004_runtime_environments.sql"} {
		copyMigration(name)
	}
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "control.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx, migrationDir); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO nodes(id,name,status,os,created_at) VALUES('node-absolute','node','offline','windows',?1);
		INSERT INTO game_installations(id,environment_id,node_id,root_index,relative_path,path_key,status,created_at,updated_at)
		VALUES('relative','environment-node-absolute','node-absolute',0,'servers/relative','servers/relative','ready',?1,?1),
		('absolute','environment-node-absolute','node-absolute',0,'D:/servers/absolute','d:/servers/absolute','ready',?1,?1);
		INSERT INTO instances(id,node_id,name,status,path,port,config,created_at,updated_at,installation_id)
		VALUES('relative-instance','node-absolute','relative','stopped','servers/relative',27015,'{}',?1,?1,'relative'),
		('absolute-instance','node-absolute','absolute','stopped','D:/servers/absolute',27016,'{}',?1,?1,'absolute')`, now); err != nil {
		t.Fatal(err)
	}
	copyMigration("005_absolute_game_installations.sql")
	if err := store.Migrate(ctx, migrationDir); err != nil {
		t.Fatal(err)
	}
	columns := map[string]bool{}
	rows, err := store.db.QueryContext(ctx, `PRAGMA table_info(game_installations)`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		columns[name] = true
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if !columns["path"] || columns["root_index"] || columns["relative_path"] {
		t.Fatalf("unexpected game_installations columns after migration: %#v", columns)
	}
	var installations, relativeInstances, absoluteInstances, absoluteBinding int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM game_installations`).Scan(&installations); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM instances WHERE id='relative-instance'`).Scan(&relativeInstances); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT count(*),count(installation_id) FROM instances WHERE id='absolute-instance'`).Scan(&absoluteInstances, &absoluteBinding); err != nil {
		t.Fatal(err)
	}
	if installations != 1 || relativeInstances != 0 || absoluteInstances != 1 || absoluteBinding != 0 {
		t.Fatalf("migration result installations=%d relative_instances=%d absolute_instances=%d absolute_bindings=%d", installations, relativeInstances, absoluteInstances, absoluteBinding)
	}
	if rows, err := store.db.QueryContext(ctx, `PRAGMA foreign_key_check`); err != nil {
		t.Fatal(err)
	} else {
		defer rows.Close()
		if rows.Next() {
			t.Fatal("absolute installation migration left a foreign key violation")
		}
	}
}

func TestRuntimeEnvironmentResourcesAndExclusiveBinding(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "control.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx, filepath.Join("..", "..", "migrations", "sqlite")); err != nil {
		t.Fatal(err)
	}
	nodeID := uuid.NewString()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO nodes(id,name,status,os,arch,agent_version,address,instance_root,agent_credential_hash,created_at)
		VALUES(?1,'node','online','windows','amd64','test','','C:\l4d2',X'00',?2)`, nodeID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	environment, err := store.GetEnvironment(ctx, "environment-"+nodeID)
	if err != nil || environment.NodeID != nodeID {
		t.Fatalf("environment trigger failed: %#v %v", environment, err)
	}
	installation, err := store.CreateGameInstallation(ctx, environment.ID, `D:\GameServers\one`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateGameInstallation(ctx, environment.ID, `D:\GameServers\one`); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate physical path was accepted: %v", err)
	}
	if _, err := store.CreateGameInstallation(ctx, environment.ID, `d:\gameservers\ONE`); !errors.Is(err, ErrConflict) {
		t.Fatalf("case-variant Windows path was accepted: %v", err)
	}
	absoluteInstallation, err := store.CreateGameInstallation(ctx, environment.ID, `D:\GameServers\absolute-one`)
	if err != nil || absoluteInstallation.Path != "D:/GameServers/absolute-one" {
		t.Fatalf("Windows absolute installation path was not normalized: %#v %v", absoluteInstallation, err)
	}
	if _, err := store.CreateGameInstallation(ctx, environment.ID, `d:/gameservers/ABSOLUTE-ONE`); !errors.Is(err, ErrConflict) {
		t.Fatalf("case-variant absolute Windows path was accepted: %v", err)
	}
	deletePayload := map[string]any{"installation_id": absoluteInstallation.ID, "path": absoluteInstallation.Path, "mode": "record"}
	deleteJob, _, err := store.QueueEnvironmentJob(ctx, environment.ID, absoluteInstallation.ID, "game.delete", deletePayload, "", "", "")
	if err != nil {
		t.Fatalf("queue game installation deletion: %v", err)
	}
	if _, _, err := store.QueueEnvironmentJob(ctx, environment.ID, absoluteInstallation.ID, "game.import",
		map[string]any{"path": absoluteInstallation.Path}, "", "", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("game installation deletion overlapped another write: %v", err)
	}
	deleteJob.Status = domain.JobSucceeded
	deleteJob.Result = map[string]any{"installation_id": absoluteInstallation.ID, "path": absoluteInstallation.Path, "files_deleted": false}
	if err := store.ApplyJobResult(ctx, deleteJob); err != nil {
		t.Fatalf("project game installation deletion: %v", err)
	}
	if _, err := store.GetGameInstallation(ctx, absoluteInstallation.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted game installation record still exists: %v", err)
	}
	if _, _, err := store.QueueEnvironmentJob(ctx, environment.ID, installation.ID, "game.install",
		map[string]any{"path": "D:/GameServers/one"}, "", "", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("game install without ready SteamCMD was accepted: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE environments SET steamcmd_status='ready',steamcmd_source='custom',steamcmd_path='/opt/steamcmd/steamcmd.sh' WHERE id=?1`, environment.ID); err != nil {
		t.Fatal(err)
	}
	failedSwitch := domain.Job{NodeID: nodeID, Type: "steamcmd.configure", Status: domain.JobFailed, Message: "replacement failed", CreatedAt: time.Now().UTC()}
	if err := store.ApplyJobFailure(ctx, failedSwitch); err != nil {
		t.Fatal(err)
	}
	preserved, err := store.GetEnvironment(ctx, environment.ID)
	if err != nil || preserved.SteamCMDStatus != "ready" || preserved.SteamCMDPath != "/opt/steamcmd/steamcmd.sh" || preserved.LastError != "replacement failed" {
		t.Fatalf("failed SteamCMD switch replaced the active configuration: %#v %v", preserved, err)
	}
	job, _, err := store.QueueEnvironmentJob(ctx, environment.ID, installation.ID, "game.install",
		map[string]any{"path": "D:/GameServers/one"}, "", "", "")
	if err != nil || job.InstallationID != installation.ID {
		t.Fatalf("queue installation job: %#v %v", job, err)
	}
	if _, _, err := store.QueueEnvironmentJob(ctx, environment.ID, installation.ID, "game.install",
		map[string]any{"path": "D:/GameServers/one"}, "", "", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("concurrent installation write was accepted: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE jobs SET status='succeeded' WHERE id=?1; UPDATE game_installations SET status='ready' WHERE id=?2`, job.ID, installation.ID); err != nil {
		t.Fatal(err)
	}
	config := domain.InstanceConfig{Hostname: "one", Mode: "coop", Difficulty: "normal", MaxPlayers: 4, StartMap: "c1m1_hotel"}
	instance, err := store.CreateInstance(ctx, CreateInstanceInput{NodeID: nodeID, InstallationID: installation.ID, Name: "one", Config: config})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateInstance(ctx, CreateInstanceInput{NodeID: nodeID, InstallationID: installation.ID, Name: "two", Config: config}); !errors.Is(err, ErrConflict) {
		t.Fatalf("game installation was bound twice: %v", err)
	}
	if _, _, err := store.QueueEnvironmentJob(ctx, environment.ID, installation.ID, "game.delete",
		map[string]any{"installation_id": installation.ID, "path": installation.Path, "mode": "record"}, "", "", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("bound game installation deletion was accepted: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE instances SET status='stopped' WHERE id=?1`, instance.ID); err != nil {
		t.Fatal(err)
	}
	start, _, err := store.QueueInstanceJob(ctx, instance.ID, "start", map[string]any{}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.QueueEnvironmentJob(ctx, environment.ID, installation.ID, "game.validate",
		map[string]any{"path": "D:/GameServers/one"}, "", "", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("game maintenance overlapped an instance write: %v", err)
	}
	if _, err := store.UpdateGameInstallationLocation(ctx, installation.ID, "D:/GameServers/moved"); !errors.Is(err, ErrConflict) {
		t.Fatalf("path changed during an instance write: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE jobs SET status='succeeded' WHERE id=?1; UPDATE instances SET status='stopped' WHERE id=?2`, start.ID, instance.ID); err != nil {
		t.Fatal(err)
	}
	updated, err := store.UpdateGameInstallationLocation(ctx, installation.ID, "D:/GameServers/moved")
	if err != nil || updated.Path != "D:/GameServers/moved" {
		t.Fatalf("update stopped installation location: %#v %v", updated, err)
	}
	retry, _, err := store.QueueEnvironmentJob(ctx, environment.ID, installation.ID, "game.import",
		map[string]any{"path": "D:/GameServers/moved"}, "", "", "")
	if err != nil || retry.InstanceID != instance.ID {
		t.Fatalf("bound import did not acquire the instance write lock: %#v %v", retry, err)
	}
}
