package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

// Applied SQLite migration bodies are immutable: Store.Migrate persists these
// checksums and deliberately refuses to start if an old file is rewritten.
func TestSQLiteMigrationChecksums(t *testing.T) {
	t.Parallel()
	expected := map[string]string{
		"001_control_plane.sql":                      "5a95514ce69e52e64613fbd1313eeed81eae422dc917c3e6f7acaae53e2f508f",
		"002_bootstrap_setup_key.sql":                "403054323dc5d62600b26aebdcdcadbabea46cbde0f6b83f136c37c2e4ff3fa1",
		"003_system_update_jobs.sql":                 "508c18088158cc8f1be897e30f510e207d8e2999c1762569216a9bf2d36ddea3",
		"004_runtime_environments.sql":               "2ea900b03eb08199bb3d543907a6a34b92bd158a6c645570afc7ec8dc2ea7104",
		"005_absolute_game_installations.sql":        "17649c639d13e47e480f0f8a75e4a67d02b279d2cbceab006162bdb94f2a8193",
		"006_remove_config_history.sql":              "468fd664f7f60907388db13edb051ecde268dae82ce6e07fb5e3893ae03c43a5",
		"007_map_library.sql":                        "8ff1eb6583f458254ef8add0e7928e26523668ae15423e5d1599e77e401fbd5e",
		"008_map_metadata.sql":                       "4bd22eb20d0a97910dd433e8d48b1ac434c89a9b438093f9f686237f378e4996",
		"009_fine_grained_permissions.sql":           "830a1cc1afb76bda4a6f1fa7d171ad939ac789de02d02bf00de2fd719e367ae4",
		"010_user_activation.sql":                    "d02925cf0d8621472c805e2e89b61aed36d40cdf3e5e53e0d8efbdc1499b575e",
		"011_game_installation_deletion.sql":         "6396f1edfcbd4eb45a28e33655620225eb62c689d834fdcc8e600620a22c5e07",
		"012_instance_port_changes.sql":              "3f62ff7f344090a692b6f28f605eb1c514222b2c99a6bf7e8008f220a004ce22",
		"013_retire_obsolete_permissions.sql":        "10e51770bde143350a4b7ec03f325ed17d1f265c617bc049ff04c76f930508af",
		"014_smart_plugin_archives.sql":              "6e8d4d7b4ddb58653759cd17872a76a8e854728c2a8e1d6a9c49a98b371c7929",
		"015_backup_deletion_permission.sql":         "f08e9075c807888531d5faab099e83ee31eee031fd4488fa52aeeac5862960ce",
		"016_terminal_outcome_conflicts.sql":         "c75401441dc0d3bb633d36cbc8a0e357757cc12ef17fba8f12890821724d9dd5",
		"017_terminal_plugin_inspection_cleanup.sql": "dfd1d59ec45fd120ada1eabf2e45c924262a824dec5eacdccc46e3049ae04323",
		"018_pending_job_projection_index.sql":       "8eb499362eb135bd24dc366e58bcebfb138b27a512366b6ba292e7afe7f21960",
		"019_parallel_plugin_inspection.sql":         "4e9da14d3fe9462e32d1cae24304ae6c874616b6f22dde74693cbbd4264b3d75",
		"020_prepared_packages.sql":                  "d3c19e20b9a5eeae87f11a45528cb584c67b06e6112605651eeca15622f39e5b",
		"021_prepared_install_queue.sql":             "507ad1f75aa6d94dfee535d0007cbf0fb8aeb8bd3c0396c3c7098bac88ae3623",
		"022_parallel_package_scan.sql":              "86ce6723de202a1a8a69ac47aa51dd3c249360988fca4cfc7f5255d0b0765016",
	}
	root := filepath.Join("..", "..", "migrations", "sqlite")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	actual := 0
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".sql" {
			continue
		}
		actual++
		body, err := os.ReadFile(filepath.Join(root, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(body)
		if got, want := hex.EncodeToString(sum[:]), expected[entry.Name()]; want == "" || got != want {
			t.Errorf("SQLite migration %s checksum=%s want=%s", entry.Name(), got, want)
		}
	}
	if actual != len(expected) {
		t.Fatalf("SQLite migration count=%d checksum count=%d", actual, len(expected))
	}
}

func TestTerminalPluginInspectionCleanupMigration(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "plugin-inspection-cleanup.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	root := filepath.Join("..", "..", "migrations", "sqlite")
	if err := store.Migrate(ctx, root); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO nodes(id,name,status) VALUES('node','Node','online');
		INSERT INTO jobs(id,node_id,type,status,payload,result) VALUES(
		'inspection','node','inspect_plugin_archive','failed',
		'{"source":"[encrypted]","source_ciphertext":"sealed-source","content_base64":"[encrypted]","content_base64_ciphertext":"sealed-upload"}',
		'{"_projection_pending":true}');
		INSERT INTO jobs(id,node_id,type,status,payload,result) VALUES(
		'install','node','install_mod','failed',
		'{"content_base64":"[encrypted]","content_base64_ciphertext":"sealed-upload"}',
		'{"_projection_pending":true}')`); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(root, "017_terminal_plugin_inspection_cleanup.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, string(body)); err != nil {
		t.Fatal(err)
	}
	inspection, err := store.GetJob(ctx, "inspection")
	if err != nil {
		t.Fatal(err)
	}
	if jobProjectionPending(inspection) || inspection.Payload["content_base64_ciphertext"] != nil || inspection.Payload["source_ciphertext"] != nil {
		t.Fatalf("inspection cleanup = %#v", inspection)
	}
	install, err := store.GetJob(ctx, "install")
	if err != nil {
		t.Fatal(err)
	}
	if !jobProjectionPending(install) || install.Payload["content_base64_ciphertext"] == nil {
		t.Fatalf("cleanup changed unrelated job: %#v", install)
	}
}

func TestBackupDeletionPermissionMigrationOnlyUpdatesGlobalOwners(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "backup-delete-permission.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	migrationsDir := filepath.Join("..", "..", "migrations", "sqlite")
	if err := store.Migrate(ctx, migrationsDir); err != nil {
		t.Fatal(err)
	}
	_, created, err := store.BootstrapOwner(ctx, "owner", "Owner", "hash")
	if err != nil || !created {
		t.Fatalf("bootstrap owner: created=%v err=%v", created, err)
	}
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO users(id,username,username_normalized,display_name,password_hash,role)
		VALUES('legacy-admin','legacy-admin','legacy-admin','Legacy Admin','hash','admin');
		INSERT INTO role_bindings(id,user_id,role,scope_type) VALUES('legacy-admin-binding','legacy-admin','admin','global');
		DELETE FROM role_binding_permissions WHERE permission='backups.delete';
		DELETE FROM schema_migrations WHERE version='015_backup_deletion_permission.sql'`); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx, migrationsDir); err != nil {
		t.Fatal(err)
	}
	var ownerGrant, adminGrant int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM role_binding_permissions permission
		JOIN role_bindings binding ON binding.id=permission.binding_id
		WHERE binding.role='owner' AND binding.scope_type='global' AND permission.permission='backups.delete'`).Scan(&ownerGrant); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM role_binding_permissions
		WHERE binding_id='legacy-admin-binding' AND permission='backups.delete'`).Scan(&adminGrant); err != nil {
		t.Fatal(err)
	}
	if ownerGrant != 1 || adminGrant != 0 {
		t.Fatalf("backup deletion migration grants: owner=%d admin=%d", ownerGrant, adminGrant)
	}
}

func TestRetireObsoletePermissionsMigrationFailsClosedForEmptyBindings(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sourceRoot := filepath.Join("..", "..", "migrations", "sqlite")
	migrationRoot := t.TempDir()
	copyMigration := func(name string) {
		t.Helper()
		body, err := os.ReadFile(filepath.Join(sourceRoot, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(migrationRoot, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{
		"001_control_plane.sql",
		"002_bootstrap_setup_key.sql",
		"003_system_update_jobs.sql",
		"004_runtime_environments.sql",
		"005_absolute_game_installations.sql",
		"006_remove_config_history.sql",
		"007_map_library.sql",
		"008_map_metadata.sql",
		"009_fine_grained_permissions.sql",
		"010_user_activation.sql",
		"011_game_installation_deletion.sql",
		"012_instance_port_changes.sql",
	} {
		copyMigration(name)
	}
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "pre-permission-retirement.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx, migrationRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO users(id,username,username_normalized,display_name,password_hash,role) VALUES
			('mixed-user','mixed-user','mixed-user','Mixed','hash','operator'),
			('retired-user','retired-user','retired-user','Retired','hash','operator'),
			('empty-user','empty-user','empty-user','Empty','hash','operator'),
			('full-admin','full-admin','full-admin','Full admin','hash','admin'),
			('custom-admin','custom-admin','custom-admin','Custom admin','hash','admin'),
			('post009-admin','post009-admin','post009-admin','Post-009 admin','hash','admin'),
			('post009-owner','post009-owner','post009-owner','Post-009 owner','hash','owner'),
			('operator-user','operator-user','operator-user','Operator','hash','operator'),
			('existing-admin','existing-admin','existing-admin','Existing admin','hash','admin');
		INSERT INTO nodes(id,name,status) VALUES('schedule-node','Schedule node','online');
		INSERT INTO instances(id,node_id,name,status,port,config)
			VALUES('schedule-instance','schedule-node','Schedule instance','stopped',27015,'{}');
		INSERT INTO role_bindings(id,user_id,role,scope_type,node_id,instance_id) VALUES
			('mixed-binding','mixed-user','operator','global',NULL,NULL),
			('retired-binding','retired-user','operator','global',NULL,NULL),
			('empty-binding','empty-user','operator','global',NULL,NULL),
			('full-admin-global','full-admin','admin','global',NULL,NULL),
			('full-admin-node','full-admin','admin','node','schedule-node',NULL),
			('full-admin-instance','full-admin','admin','instance',NULL,'schedule-instance'),
			('custom-admin-binding','custom-admin','admin','global',NULL,NULL),
			('post009-full-binding','post009-admin','admin','global',NULL,NULL),
			('post009-owner-binding','post009-owner','owner','global',NULL,NULL),
			('operator-binding','operator-user','operator','global',NULL,NULL),
			('existing-delete-binding','existing-admin','admin','global',NULL,NULL);
		INSERT INTO role_binding_permissions(binding_id,permission) VALUES
			('mixed-binding','instances.read'),
			('mixed-binding','instances.update'),
			('mixed-binding','instances.validate'),
			('mixed-binding','files.upload'),
			('retired-binding','instances.update'),
			('retired-binding','files.upload');
		INSERT INTO schedules(id,instance_id,name,cron,action,enabled) VALUES
			('update-schedule','schedule-instance','Legacy update','0 4 * * *','update',1),
			('validate-schedule','schedule-instance','Legacy validate','0 5 * * *','validate',1),
			('backup-schedule','schedule-instance','Current backup','0 6 * * *','backup',1);
	`); err != nil {
		t.Fatal(err)
	}
	insertLegacyPreset := func(bindingID string, role domain.Role, scopeType, omitted string, includePackageDelete bool) {
		t.Helper()
		for _, permission := range presetPermissionsForScope(role, scopeType) {
			if permission == string(PermissionPackagesDelete) || permission == omitted {
				continue
			}
			if _, err := store.db.ExecContext(ctx, `INSERT INTO role_binding_permissions(binding_id,permission) VALUES(?1,?2)`, bindingID, permission); err != nil {
				t.Fatal(err)
			}
		}
		for _, retired := range []struct {
			permission string
			minimum    int
		}{
			{permission: "instances.update", minimum: 2},
			{permission: "instances.validate", minimum: 2},
			{permission: "files.upload", minimum: 3},
		} {
			if roleRank(role) < retired.minimum || retired.permission == omitted {
				continue
			}
			if _, err := store.db.ExecContext(ctx, `INSERT INTO role_binding_permissions(binding_id,permission) VALUES(?1,?2)`, bindingID, retired.permission); err != nil {
				t.Fatal(err)
			}
		}
		if includePackageDelete {
			if _, err := store.db.ExecContext(ctx, `INSERT INTO role_binding_permissions(binding_id,permission) VALUES(?1,?2)`, bindingID, PermissionPackagesDelete); err != nil {
				t.Fatal(err)
			}
		}
	}
	insertLegacyPreset("full-admin-global", domain.RoleAdmin, "global", "", false)
	insertLegacyPreset("full-admin-node", domain.RoleAdmin, "node", "", false)
	insertLegacyPreset("full-admin-instance", domain.RoleAdmin, "instance", "", false)
	insertLegacyPreset("custom-admin-binding", domain.RoleAdmin, "global", string(PermissionPackagesInstall), false)
	insertLegacyPreset("post009-full-binding", domain.RoleAdmin, "global", "", false)
	insertLegacyPreset("post009-owner-binding", domain.RoleOwner, "global", "", false)
	insertLegacyPreset("operator-binding", domain.RoleOperator, "global", "", false)
	insertLegacyPreset("existing-delete-binding", domain.RoleAdmin, "global", "", true)
	var migrationTimestampType string
	if err := store.db.QueryRowContext(ctx, `SELECT typeof(applied_at) FROM schema_migrations
		WHERE version='009_fine_grained_permissions.sql'`).Scan(&migrationTimestampType); err != nil {
		t.Fatal(err)
	}
	if migrationTimestampType != "integer" {
		t.Fatalf("migration timestamp storage type=%q, want integer unix_micro", migrationTimestampType)
	}
	if _, err := store.db.ExecContext(ctx, `
		UPDATE role_bindings
		SET created_at=(SELECT applied_at-1 FROM schema_migrations
			WHERE version='009_fine_grained_permissions.sql')
		WHERE id IN (
			'full-admin-global','full-admin-node','full-admin-instance',
			'custom-admin-binding','operator-binding','existing-delete-binding'
		);
		UPDATE role_bindings
		SET created_at=(SELECT applied_at+1 FROM schema_migrations
			WHERE version='009_fine_grained_permissions.sql')
		WHERE id IN ('post009-full-binding','post009-owner-binding');
	`); err != nil {
		t.Fatal(err)
	}
	copyMigration("013_retire_obsolete_permissions.sql")
	if err := store.Migrate(ctx, migrationRoot); err != nil {
		t.Fatal(err)
	}

	var mixedPermissions int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM role_binding_permissions WHERE binding_id='mixed-binding' AND permission='instances.read'`).Scan(&mixedPermissions); err != nil {
		t.Fatal(err)
	}
	if mixedPermissions != 1 {
		t.Fatalf("surviving binding permission count=%d, want 1", mixedPermissions)
	}
	var retiredPermissions int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM role_binding_permissions WHERE permission IN ('instances.update','instances.validate','files.upload')`).Scan(&retiredPermissions); err != nil {
		t.Fatal(err)
	}
	if retiredPermissions != 0 {
		t.Fatalf("obsolete permission count=%d, want 0", retiredPermissions)
	}
	var unsafeBindings int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM role_bindings WHERE id IN ('retired-binding','empty-binding')`).Scan(&unsafeBindings); err != nil {
		t.Fatal(err)
	}
	if unsafeBindings != 0 {
		t.Fatalf("empty fallback binding count=%d, want 0", unsafeBindings)
	}
	var disabledLegacySchedules int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM schedules WHERE action IN ('update','validate') AND enabled=0`).Scan(&disabledLegacySchedules); err != nil {
		t.Fatal(err)
	}
	if disabledLegacySchedules != 2 {
		t.Fatalf("disabled legacy schedule count=%d, want 2", disabledLegacySchedules)
	}
	var enabledBackupSchedules int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM schedules WHERE action='backup' AND enabled=1`).Scan(&enabledBackupSchedules); err != nil {
		t.Fatal(err)
	}
	if enabledBackupSchedules != 1 {
		t.Fatalf("enabled current schedule count=%d, want 1", enabledBackupSchedules)
	}
	var backfilledFullPresets int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM role_binding_permissions
		WHERE binding_id IN ('full-admin-global','full-admin-node','full-admin-instance') AND permission='packages.delete'`).Scan(&backfilledFullPresets); err != nil {
		t.Fatal(err)
	}
	if backfilledFullPresets != 3 {
		t.Fatalf("full legacy preset package delete count=%d, want 3", backfilledFullPresets)
	}
	var ineligibleBackfills int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM role_binding_permissions
		WHERE binding_id IN ('custom-admin-binding','post009-full-binding','operator-binding') AND permission='packages.delete'`).Scan(&ineligibleBackfills); err != nil {
		t.Fatal(err)
	}
	if ineligibleBackfills != 0 {
		t.Fatalf("custom/post-009/operator package delete count=%d, want 0", ineligibleBackfills)
	}
	var post009OwnerBackfill int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM role_binding_permissions
		WHERE binding_id='post009-owner-binding' AND permission='packages.delete'`).Scan(&post009OwnerBackfill); err != nil {
		t.Fatal(err)
	}
	if post009OwnerBackfill != 1 {
		t.Fatalf("post-009 owner package delete count=%d, want 1", post009OwnerBackfill)
	}
	var existingDeleteCount int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM role_binding_permissions
		WHERE binding_id='existing-delete-binding' AND permission='packages.delete'`).Scan(&existingDeleteCount); err != nil {
		t.Fatal(err)
	}
	if existingDeleteCount != 1 {
		t.Fatalf("existing package delete count=%d, want idempotent 1", existingDeleteCount)
	}
}

func TestRemoveConfigHistoryMigrationPreservesRestoreProjectionMarkers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sourceRoot := filepath.Join("..", "..", "migrations", "sqlite")
	migrationRoot := t.TempDir()
	copyMigration := func(name string) {
		t.Helper()
		body, err := os.ReadFile(filepath.Join(sourceRoot, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(migrationRoot, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{
		"001_control_plane.sql",
		"002_bootstrap_setup_key.sql",
		"003_system_update_jobs.sql",
		"004_runtime_environments.sql",
		"005_absolute_game_installations.sql",
	} {
		copyMigration(name)
	}
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "pre-history-removal.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx, migrationRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO nodes(id,name,status) VALUES('node-1','node-1','online');
		INSERT INTO instances(id,node_id,name,status,port,config) VALUES('instance-1','node-1','instance-1','stopped',27015,'{}');
		INSERT INTO jobs(id,node_id,instance_id,type,status) VALUES('restore-job-1','node-1','instance-1','restore','succeeded');
		INSERT INTO config_versions(id,instance_id,version,config,source_job_id) VALUES('config-version-1','instance-1',2,'{}','restore-job-1');
	`); err != nil {
		t.Fatal(err)
	}
	copyMigration("006_remove_config_history.sql")
	if err := store.Migrate(ctx, migrationRoot); err != nil {
		t.Fatal(err)
	}
	var instanceID string
	if err := store.db.QueryRowContext(ctx, `SELECT instance_id FROM restore_config_projections WHERE job_id='restore-job-1'`).Scan(&instanceID); err != nil {
		t.Fatal(err)
	}
	if instanceID != "instance-1" {
		t.Fatalf("restore projection instance=%q", instanceID)
	}
	var oldTableCount int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema WHERE type='table' AND name='config_versions'`).Scan(&oldTableCount); err != nil {
		t.Fatal(err)
	}
	if oldTableCount != 0 {
		t.Fatalf("config_versions table still exists after migration")
	}
}
