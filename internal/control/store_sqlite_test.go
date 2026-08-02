package control

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func TestSQLiteStoreLockPragmasAndBackup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	databasePath := filepath.Join(t.TempDir(), "control.sqlite3")
	store, err := OpenStore(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(ctx, databasePath); err == nil {
		store.Close()
		t.Fatal("second Store acquired the same database lock")
	}
	foreignKeys, journalMode, err := store.SQLitePragmas(ctx)
	if err != nil || !foreignKeys || journalMode != "wal" {
		store.Close()
		t.Fatalf("unexpected SQLite pragmas: foreign_keys=%v journal_mode=%q err=%v", foreignKeys, journalMode, err)
	}
	for pragma, want := range map[string]int64{
		"busy_timeout":       10000,
		"synchronous":        2,
		"auto_vacuum":        2,
		"trusted_schema":     0,
		"wal_autocheckpoint": 1000,
		"journal_size_limit": 64 << 20,
	} {
		var got int64
		if err := store.db.QueryRowContext(ctx, `PRAGMA `+pragma).Scan(&got); err != nil || got != want {
			store.Close()
			t.Fatalf("PRAGMA %s=%d, want %d (err=%v)", pragma, got, want, err)
		}
	}
	if err := store.Migrate(ctx, filepath.Join("..", "..", "migrations", "sqlite")); err != nil {
		store.Close()
		t.Fatal(err)
	}
	backupPath := filepath.Join(t.TempDir(), "backup.sqlite3")
	if err := store.BackupDatabase(ctx, backupPath); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.BackupDatabase(ctx, backupPath); err == nil {
		store.Close()
		t.Fatal("backup overwrote an existing output")
	}
	store.Close()

	reopened, err := OpenStore(ctx, databasePath)
	if err != nil {
		t.Fatalf("lock was not released by Close: %v", err)
	}
	reopened.Close()
	backup, err := OpenStore(ctx, backupPath)
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	if err := backup.QuickCheck(ctx); err != nil {
		t.Fatal(err)
	}
	if status, err := backup.SchemaStatus(ctx); err != nil || status != "applied" {
		t.Fatalf("backup schema status=%q err=%v", status, err)
	}
}

func TestSQLiteMigrationValidationAndRollback(t *testing.T) {
	t.Run("checksum", func(t *testing.T) {
		ctx := context.Background()
		dir := t.TempDir()
		migration := filepath.Join(dir, "001_test.sql")
		if err := os.WriteFile(migration, []byte("CREATE TABLE example(id INTEGER PRIMARY KEY);"), 0o600); err != nil {
			t.Fatal(err)
		}
		store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "control.sqlite3"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		if err := store.Migrate(ctx, dir); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(migration, []byte("CREATE TABLE example(id TEXT PRIMARY KEY);"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := store.Migrate(ctx, dir); err == nil || !strings.Contains(err.Error(), "checksum changed") {
			t.Fatalf("rewritten migration was accepted: %v", err)
		}
	})

	t.Run("unknown", func(t *testing.T) {
		ctx := context.Background()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "001_test.sql"), []byte("SELECT 1;"), 0o600); err != nil {
			t.Fatal(err)
		}
		store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "control.sqlite3"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		if _, err := store.db.ExecContext(ctx, `CREATE TABLE schema_migrations(version TEXT PRIMARY KEY,checksum TEXT NOT NULL,applied_at TIMESTAMP NOT NULL)`); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.ExecContext(ctx, `INSERT INTO schema_migrations VALUES('999_unknown.sql','deadbeef',?1)`, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		if err := store.Migrate(ctx, dir); err == nil || !strings.Contains(err.Error(), "unknown migration") {
			t.Fatalf("unknown applied migration was accepted: %v", err)
		}
	})

	t.Run("rollback", func(t *testing.T) {
		ctx := context.Background()
		dir := t.TempDir()
		body := "CREATE TABLE rolled_back(id INTEGER PRIMARY KEY); INSERT INTO missing_table VALUES(1);"
		if err := os.WriteFile(filepath.Join(dir, "001_broken.sql"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "control.sqlite3"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		if err := store.Migrate(ctx, dir); err == nil {
			t.Fatal("broken migration succeeded")
		}
		var count int
		if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema WHERE name='rolled_back'`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("failed migration was not rolled back: count=%d err=%v", count, err)
		}
	})
}

func TestSQLiteSchemaRejectsInvalidDirectWrites(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "constraints.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx, filepath.Join("..", "..", "migrations", "sqlite")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO nodes(id,name,status) VALUES('node','node','offline')`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO users(id,username,username_normalized,display_name,password_hash,role)
		VALUES('user','user','user','User','hash','owner')`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO instances(id,node_id,name,status,port,config) VALUES('quarantine-instance','node','quarantine','stopped',27018,'{}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO jobs(id,node_id,instance_id,type,status,payload,result) VALUES('quarantine-job','node','quarantine-instance','delete','succeeded','{}','{}')`); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name      string
		statement string
	}{
		{"node status", `INSERT INTO nodes(id,name,status) VALUES('bad-node','bad','unknown')`},
		{"job status", `INSERT INTO jobs(id,node_id,type,status,payload,result) VALUES('bad-job','node','test','unknown','{}','{}')`},
		{"foreign key", `INSERT INTO instances(id,node_id,name,status,port,config) VALUES('i1','missing','bad','stopped',27015,'{}')`},
		{"port", `INSERT INTO instances(id,node_id,name,status,port,config) VALUES('i2','node','bad','stopped',70000,'{}')`},
		{"json", `INSERT INTO instances(id,node_id,name,status,port,config) VALUES('i3','node','bad','stopped',27016,'not-json')`},
		{"json blob", `INSERT INTO instances(id,node_id,name,status,port,config) VALUES('i4','node','bad','stopped',27017,x'7b7d')`},
		{"boolean", `UPDATE users SET disabled=2 WHERE id='user'`},
		{"token hash", `INSERT INTO api_tokens(id,user_id,token_hash,name,scope_mask) VALUES('t1','user',x'00','bad',1)`},
		{"scope mask", `INSERT INTO api_tokens(id,user_id,token_hash,name,scope_mask) VALUES('t2','user',zeroblob(32),'bad',8)`},
		{"quarantine name", `INSERT INTO quarantine_entries(id,node_id,source_job_id,original_instance_id,instance_name,root_index,entry_name,quarantined_at,retention_expires_at)
			VALUES('q1','node','quarantine-job','quarantine-instance','instance',0,'aaaaaaaaaaaaaaaaa:',1000000,604801000000)`},
		{"quarantine retention", `INSERT INTO quarantine_entries(id,node_id,source_job_id,original_instance_id,instance_name,root_index,entry_name,quarantined_at,retention_expires_at)
			VALUES('q2','node','quarantine-job','quarantine-instance','instance',0,'entry-20260801T000000Z',1000000,1000001)`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if _, err := store.db.ExecContext(ctx, test.statement); err == nil || !isSQLiteConstraint(err) {
				t.Fatalf("invalid direct write was accepted or misclassified: %v", err)
			}
		})
	}
}

func TestSQLiteConcurrentBootstrapCreatesOneOwner(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "bootstrap.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx, filepath.Join("..", "..", "migrations", "sqlite")); err != nil {
		t.Fatal(err)
	}

	const contenders = 30
	var wait sync.WaitGroup
	results := make(chan error, contenders)
	created := make(chan string, contenders)
	for index := range contenders {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			user, wasCreated, err := store.BootstrapOwner(ctx, fmt.Sprintf("owner-%d", index), "Owner", "password-hash")
			if err != nil {
				results <- err
				return
			}
			if wasCreated {
				created <- user.ID
			}
		}(index)
	}
	wait.Wait()
	close(results)
	close(created)
	for err := range results {
		t.Fatalf("concurrent bootstrap failed: %v", err)
	}
	if len(created) != 1 {
		t.Fatalf("concurrent bootstrap created %d owners, want 1", len(created))
	}
	var owners int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE role='owner'`).Scan(&owners); err != nil || owners != 1 {
		t.Fatalf("owner count=%d err=%v", owners, err)
	}
}

func TestSetupKeyCanRotateWhileControlOwnsProcessLock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	databasePath := filepath.Join(t.TempDir(), "setup-key.sqlite3")
	store, err := OpenStore(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx, filepath.Join("..", "..", "migrations", "sqlite")); err != nil {
		t.Fatal(err)
	}
	first, err := IssueSetupKeyAtPath(ctx, databasePath)
	if err != nil || !ValidSetupKey(first) {
		t.Fatalf("issue first setup key: valid=%v err=%v", ValidSetupKey(first), err)
	}
	second, err := IssueSetupKeyAtPath(ctx, databasePath)
	if err != nil || first == second {
		t.Fatalf("rotate setup key: distinct=%v err=%v", first != second, err)
	}
	if err := store.ValidateSetupKey(ctx, first); !errors.Is(err, ErrInvalidSetupKey) {
		t.Fatalf("old setup key remained valid: %v", err)
	}
	if err := store.ValidateSetupKey(ctx, second); err != nil {
		t.Fatalf("new setup key is invalid: %v", err)
	}
	var stored []byte
	if err := store.db.QueryRowContext(ctx, `SELECT key_hash FROM bootstrap_setup_key WHERE singleton=1`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if len(stored) != 32 || bytes.Contains(stored, []byte(second)) {
		t.Fatalf("database did not retain only the setup-key digest")
	}
	for _, path := range []string{databasePath, databasePath + "-wal", databasePath + "-shm"} {
		contents, readErr := os.ReadFile(path)
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			t.Fatal(readErr)
		}
		if bytes.Contains(contents, []byte(first)) || bytes.Contains(contents, []byte(second)) {
			t.Fatalf("plaintext setup key leaked into SQLite file %s", filepath.Base(path))
		}
	}
}

func TestSQLiteConcurrentSetupKeyBootstrapCreatesOneOwner(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "setup-key-bootstrap.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx, filepath.Join("..", "..", "migrations", "sqlite")); err != nil {
		t.Fatal(err)
	}
	setupKey, err := store.IssueSetupKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	const contenders = 20
	var wait sync.WaitGroup
	created := make(chan string, contenders)
	errorsSeen := make(chan error, contenders)
	for range contenders {
		wait.Add(1)
		go func() {
			defer wait.Done()
			user, err := store.BootstrapOwnerWithSetupKey(ctx, setupKey, "password-hash")
			if err == nil {
				created <- user.ID
			} else if !errors.Is(err, ErrBootstrapComplete) {
				errorsSeen <- err
			}
		}()
	}
	wait.Wait()
	close(created)
	close(errorsSeen)
	for err := range errorsSeen {
		t.Fatalf("concurrent setup-key bootstrap failed: %v", err)
	}
	if len(created) != 1 {
		t.Fatalf("concurrent setup-key bootstrap created %d owners, want 1", len(created))
	}
	if _, err := store.IssueSetupKey(ctx); !errors.Is(err, ErrBootstrapComplete) {
		t.Fatalf("setup key was issued after bootstrap: %v", err)
	}
}

func TestSQLiteConcurrentNotificationClaimsAreExclusive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "notifications.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx, filepath.Join("..", "..", "migrations", "sqlite")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO notification_channels(id,name,kind,config) VALUES('channel','test','webhook','{}')`); err != nil {
		t.Fatal(err)
	}
	for index := range 20 {
		notificationID := fmt.Sprintf("notification-%d", index)
		deliveryID := fmt.Sprintf("delivery-%d", index)
		if _, err := store.db.ExecContext(ctx, `INSERT INTO notifications(id,severity,type,title,message,metadata) VALUES(?1,'info','test','test','test','{}')`, notificationID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.ExecContext(ctx, `INSERT INTO notification_deliveries(id,notification_id,channel_id,next_attempt_at) VALUES(?1,?2,'channel',?3)`, deliveryID, notificationID, time.Now().UTC().Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
	}

	var wait sync.WaitGroup
	claimed := make(chan string, 24)
	errorsSeen := make(chan error, 8)
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			items, err := store.claimNotificationDeliveries(ctx, 3)
			if err != nil {
				errorsSeen <- err
				return
			}
			for _, item := range items {
				claimed <- item.ID
			}
		}()
	}
	wait.Wait()
	close(claimed)
	close(errorsSeen)
	for err := range errorsSeen {
		t.Fatalf("claim notification delivery: %v", err)
	}
	unique := make(map[string]bool)
	for id := range claimed {
		if unique[id] {
			t.Fatalf("delivery %s was claimed more than once", id)
		}
		unique[id] = true
	}
	if len(unique) != 20 {
		t.Fatalf("claimed %d deliveries, want 20", len(unique))
	}
}

func TestSQLiteWALCrashRecovery(t *testing.T) {
	if databasePath := os.Getenv("L4D2PANEL_SQLITE_CRASH_CHILD"); databasePath != "" {
		store, err := OpenStore(context.Background(), databasePath)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`INSERT INTO crash_probe(id,value) VALUES(1,'committed')`); err != nil {
			t.Fatal(err)
		}
		tx, err := store.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO crash_probe(id,value) VALUES(2,'uncommitted')`); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintln(os.Stdout, "ready")
		_ = os.Stdout.Sync()
		for {
			time.Sleep(time.Hour)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	databasePath := filepath.Join(t.TempDir(), "crash.sqlite3")
	store, err := OpenStore(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `CREATE TABLE crash_probe(id INTEGER PRIMARY KEY,value TEXT NOT NULL)`); err != nil {
		store.Close()
		t.Fatal(err)
	}
	store.Close()

	command := exec.Command(os.Args[0], "-test.run=^TestSQLiteWALCrashRecovery$")
	command.Env = append(os.Environ(), "L4D2PANEL_SQLITE_CRASH_CHILD="+databasePath)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() || scanner.Text() != "ready" {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatalf("crash child did not become ready: stdout=%q stderr=%q", scanner.Text(), stderr.String())
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()

	recovered, err := OpenStore(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if err := recovered.QuickCheck(ctx); err != nil {
		t.Fatal(err)
	}
	var committed, uncommitted int
	if err := recovered.db.QueryRowContext(ctx, `SELECT count(*) FROM crash_probe WHERE id=1 AND value='committed'`).Scan(&committed); err != nil {
		t.Fatal(err)
	}
	if err := recovered.db.QueryRowContext(ctx, `SELECT count(*) FROM crash_probe WHERE id=2`).Scan(&uncommitted); err != nil {
		t.Fatal(err)
	}
	if committed != 1 || uncommitted != 0 {
		t.Fatalf("unexpected crash recovery state: committed=%d uncommitted=%d", committed, uncommitted)
	}
}

func TestSQLiteStoreLockIsProcessExclusive(t *testing.T) {
	if databasePath := os.Getenv("L4D2PANEL_SQLITE_LOCK_CHILD"); databasePath != "" {
		store, err := OpenStore(context.Background(), databasePath)
		if err == nil {
			store.Close()
			t.Fatal("child process acquired the active database lock")
		}
		return
	}

	databasePath := filepath.Join(t.TempDir(), "locked.sqlite3")
	store, err := OpenStore(context.Background(), databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	command := exec.Command(os.Args[0], "-test.run=^TestSQLiteStoreLockIsProcessExclusive$")
	command.Env = append(os.Environ(), "L4D2PANEL_SQLITE_LOCK_CHILD="+databasePath)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("lock child failed: %v output=%s", err, output)
	}
}

func TestSQLiteCorruptDatabaseFailsClosed(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "corrupt.sqlite3")
	if err := os.WriteFile(databasePath, []byte("not a SQLite database"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(context.Background(), databasePath)
	if err == nil {
		defer store.Close()
		err = store.QuickCheck(context.Background())
	}
	if err == nil {
		t.Fatal("corrupt SQLite database was accepted")
	}
}

func TestSQLiteTypeRoundTrips(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, filepath.Join(t.TempDir(), "types.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx, filepath.Join("..", "..", "migrations", "sqlite")); err != nil {
		t.Fatal(err)
	}
	owner, created, err := store.BootstrapOwner(ctx, "owner", "Owner", "password-hash")
	if err != nil || !created {
		t.Fatalf("bootstrap owner: created=%v err=%v", created, err)
	}
	var lastLogin sql.NullTime
	if err := store.db.QueryRowContext(ctx, `SELECT last_login_at FROM users WHERE id=?1`, owner.ID).Scan(&lastLogin); err != nil || lastLogin.Valid {
		t.Fatalf("nullable timestamp: %#v err=%v", lastLogin, err)
	}

	expiresAt := time.Date(2030, 2, 3, 4, 5, 6, 123456000, time.UTC)
	digest := bytes.Repeat([]byte{0x31}, 32)
	csrf := bytes.Repeat([]byte{0x42}, 32)
	sessionID, err := store.CreateSession(ctx, owner.ID, digest, csrf, "127.0.0.1", "test", expiresAt)
	if err != nil {
		t.Fatal(err)
	}
	var scannedExpiry time.Time
	var hashType, expiryType string
	var hashLength int
	if err := store.db.QueryRowContext(ctx, `SELECT expires_at,typeof(token_hash),length(token_hash),typeof(expires_at) FROM sessions WHERE id=?1`, sessionID).
		Scan(&scannedExpiry, &hashType, &hashLength, &expiryType); err != nil {
		t.Fatal(err)
	}
	if !scannedExpiry.Equal(expiresAt) || scannedExpiry.Nanosecond() != 123456000 || hashType != "blob" || hashLength != 32 || expiryType != "integer" {
		t.Fatalf("session type round trip: expiry=%s hash=%s/%d expiry_type=%s", scannedExpiry, hashType, hashLength, expiryType)
	}

	if _, err := store.db.ExecContext(ctx, `UPDATE users SET disabled=1 WHERE id=?1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	var disabled bool
	var disabledType string
	if err := store.db.QueryRowContext(ctx, `SELECT disabled,typeof(disabled) FROM users WHERE id=?1`, owner.ID).Scan(&disabled, &disabledType); err != nil || !disabled || disabledType != "integer" {
		t.Fatalf("boolean round trip: disabled=%v type=%q err=%v", disabled, disabledType, err)
	}

	if _, err := store.db.ExecContext(ctx, `INSERT INTO nodes(id,name,status) VALUES('node','node','online')`); err != nil {
		t.Fatal(err)
	}
	instance, err := store.CreateInstance(ctx, CreateInstanceInput{NodeID: "node", Name: "instance", Port: 27015,
		Config: domain.InstanceConfig{Hostname: "SQLite JSON", MaxPlayers: 8}})
	if err != nil {
		t.Fatal(err)
	}
	var jsonValid bool
	var jsonType string
	if err := store.db.QueryRowContext(ctx, `SELECT json_valid(config),typeof(config) FROM instances WHERE id=?1`, instance.ID).Scan(&jsonValid, &jsonType); err != nil || !jsonValid || jsonType != "text" {
		t.Fatalf("JSON round trip: valid=%v type=%q err=%v", jsonValid, jsonType, err)
	}

	token := APIToken{UserID: owner.ID, Name: "all scopes", Scopes: []string{"read", "operate", "manage"}, ExpiresAt: &expiresAt}
	createdToken, err := store.CreateAPIToken(ctx, token, bytes.Repeat([]byte{0x53}, 32))
	if err != nil {
		t.Fatal(err)
	}
	listed, err := store.ListAPITokens(ctx, owner.ID)
	if err != nil || len(listed) != 1 || strings.Join(listed[0].Scopes, ",") != "read,operate,manage" || listed[0].ID != createdToken.ID {
		t.Fatalf("scope-mask round trip: %#v err=%v", listed, err)
	}
	var scopeMask int
	if err := store.db.QueryRowContext(ctx, `SELECT scope_mask FROM api_tokens WHERE id=?1`, createdToken.ID).Scan(&scopeMask); err != nil || scopeMask != 7 {
		t.Fatalf("scope mask=%d err=%v", scopeMask, err)
	}
}
