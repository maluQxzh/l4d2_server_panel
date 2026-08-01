package control

import (
	"reflect"
	"strings"
	"testing"
)

func TestJobPayloadSecretsRoundTrip(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	payload := map[string]any{"command": "sm_cvar rcon_password hunter2"}
	if err := sealJobPayloadSecrets(key, "rcon", payload); err != nil {
		t.Fatal(err)
	}
	if payload["command"] == "sm_cvar rcon_password hunter2" || payload["command_ciphertext"] == "" {
		t.Fatalf("command was not protected: %#v", payload)
	}
	if err := unsealJobPayloadSecrets(key, "rcon", payload); err != nil {
		t.Fatal(err)
	}
	if payload["command"] != "sm_cvar rcon_password hunter2" {
		t.Fatalf("round trip command = %#v", payload["command"])
	}
}

func TestExternalHandoffPasswordIsEncryptedAtRest(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	payload := map[string]any{"existing_rcon_password": "old-secret"}
	if err := sealJobPayloadSecrets(key, "restart", payload); err != nil {
		t.Fatal(err)
	}
	if payload["existing_rcon_password"] != encryptedValuePlaceholder || payload["existing_rcon_password_ciphertext"] == "" {
		t.Fatalf("handoff password was not protected: %#v", payload)
	}
	if err := unsealJobPayloadSecrets(key, "restart", payload); err != nil {
		t.Fatal(err)
	}
	if payload["existing_rcon_password"] != "old-secret" || payload["existing_rcon_password_ciphertext"] != nil {
		t.Fatalf("handoff password round trip failed: %#v", payload)
	}
}

func TestTransferPayloadAndResultsAreEncryptedAtRest(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	backupPayload := map[string]any{"archive": "/srv/backups/private.zip", "backup_id": "backup-1"}
	if err := sealJobPayloadSecrets(key, "backup.chunk", backupPayload); err != nil {
		t.Fatal(err)
	}
	if backupPayload["archive"] != encryptedValuePlaceholder || backupPayload["archive_ciphertext"] == "" {
		t.Fatalf("backup path was not protected: %#v", backupPayload)
	}
	if err := unsealJobPayloadSecrets(key, "backup.chunk", backupPayload); err != nil || backupPayload["archive"] != "/srv/backups/private.zip" {
		t.Fatalf("backup path round trip failed: %v %#v", err, backupPayload)
	}

	searchPayload := map[string]any{"keyword": "STEAM_1:secret"}
	if err := sealJobPayloadSecrets(key, "logs.search", searchPayload); err != nil {
		t.Fatal(err)
	}
	if searchPayload["keyword"] != encryptedValuePlaceholder || searchPayload["keyword_ciphertext"] == "" {
		t.Fatalf("log keyword was not protected: %#v", searchPayload)
	}

	wantEntries := []map[string]any{{"stream": "stdout", "message": "private log line"}}
	searchResult := map[string]any{"entries": wantEntries, "returned": 1}
	if err := sealJobResultSecrets(key, "logs.search", searchResult); err != nil {
		t.Fatal(err)
	}
	if searchResult["entries"] != nil || searchResult["entries_ciphertext"] == "" {
		t.Fatalf("log entries were not protected: %#v", searchResult)
	}
	unsealed, err := unsealTransferJobResult(key, "logs.search", searchResult)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(unsealed["entries"], wantEntries) {
		t.Fatalf("unsealed entries = %#v", unsealed["entries"])
	}

	chunk := map[string]any{"content_base64": "c2VjcmV0"}
	if err := sealJobResultSecrets(key, "backup.chunk", chunk); err != nil {
		t.Fatal(err)
	}
	if chunk["content_base64"] != encryptedValuePlaceholder || chunk["content_base64_ciphertext"] == "" {
		t.Fatalf("chunk was not protected: %#v", chunk)
	}
}

func TestSignedPackageURLIsEncryptedAndDisplayURLIsClean(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	payload := map[string]any{"source": "https://mods.example/plugin.zip?token=secret#fragment"}
	if err := sealJobPayloadSecrets(key, "install_mod", payload); err != nil {
		t.Fatal(err)
	}
	if payload["source"] != "https://mods.example/plugin.zip" || payload["source_ciphertext"] == "" {
		t.Fatalf("URL was not protected: %#v", payload)
	}
	if err := unsealJobPayloadSecrets(key, "install_mod", payload); err != nil {
		t.Fatal(err)
	}
	if payload["source"] != "https://mods.example/plugin.zip?token=secret#fragment" {
		t.Fatalf("round trip URL = %#v", payload["source"])
	}
}

func TestUploadedPackageContentIsEncryptedAtRest(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	payload := map[string]any{
		"kind":           "plugin",
		"name":           "private-plugin.smx",
		"content_base64": "cHJpdmF0ZS1wbHVnaW4=",
	}
	if err := sealJobPayloadSecrets(key, "install_mod", payload); err != nil {
		t.Fatal(err)
	}
	if payload["content_base64"] != encryptedValuePlaceholder || payload["content_base64_ciphertext"] == "" {
		t.Fatalf("uploaded package was not protected: %#v", payload)
	}
	if err := unsealJobPayloadSecrets(key, "install_mod", payload); err != nil {
		t.Fatal(err)
	}
	if payload["content_base64"] != "cHJpdmF0ZS1wbHVnaW4=" {
		t.Fatalf("round trip upload = %#v", payload["content_base64"])
	}
	if payload["content_base64_ciphertext"] != nil {
		t.Fatalf("ciphertext leaked into dispatched payload: %#v", payload)
	}
}

func TestJobRedactionRecursivelyHidesSecretLocators(t *testing.T) {
	payload := map[string]any{
		"snapshot_path": "package-versions/private.bin",
		"nested": map[string]any{
			"storage_path": "package-versions/also-private.bin",
			"sha256":       strings.Repeat("a", 64),
		},
	}
	result := map[string]any{
		"previous_version": map[string]any{
			"storage_path": "package-versions/private.bin",
			"target_path":  "left4dead2/addons/example.vpk",
		},
		"pruned_snapshots": []any{"package-versions/old.bin"},
		"content_base64":   encryptedValuePlaceholder,
	}
	stripEncryptedJobFields(payload, result)
	if _, ok := payload["snapshot_path"]; ok {
		t.Fatal("snapshot locator was exposed in payload")
	}
	nested := payload["nested"].(map[string]any)
	if _, ok := nested["storage_path"]; ok || nested["sha256"] == nil {
		t.Fatal("recursive redaction removed safe metadata or retained storage locator")
	}
	previous := result["previous_version"].(map[string]any)
	if _, ok := previous["storage_path"]; ok || previous["target_path"] == nil {
		t.Fatal("nested version storage locator was not safely redacted")
	}
	if _, ok := result["pruned_snapshots"]; ok {
		t.Fatal("pruned snapshot locators were exposed")
	}
	if _, ok := result["content_base64"]; ok {
		t.Fatal("encrypted placeholder was exposed")
	}
}

func TestRestoreArchiveIsEncryptedAtRestAndBackupPathsAreRedacted(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	payload := map[string]any{"archive": "/var/lib/l4d2panel/backups/private.zip"}
	if err := sealJobPayloadSecrets(key, "restore", payload); err != nil {
		t.Fatal(err)
	}
	if payload["archive"] != encryptedValuePlaceholder || stringFromAny(payload["archive_ciphertext"]) == "" {
		t.Fatalf("restore archive locator was stored in clear text: %#v", payload)
	}
	if err := unsealJobPayloadSecrets(key, "restore", payload); err != nil {
		t.Fatal(err)
	}
	if payload["archive"] != "/var/lib/l4d2panel/backups/private.zip" {
		t.Fatalf("restore archive locator did not round trip: %#v", payload)
	}

	backupResult := map[string]any{"path": "/private/backup.zip", "weekly_path": "/private/weekly.zip", "pruned_paths": []any{"/private/old.zip"},
		"weekly": map[string]any{"path": "/private/weekly.zip", "name": "weekly.zip"}, "sha256": strings.Repeat("a", 64)}
	stripJobFields("backup", map[string]any{}, backupResult)
	if _, ok := backupResult["path"]; ok {
		t.Fatal("backup path was exposed")
	}
	if _, ok := backupResult["weekly_path"]; ok {
		t.Fatal("weekly backup path was exposed")
	}
	if _, ok := backupResult["pruned_paths"]; ok {
		t.Fatal("pruned backup paths were exposed")
	}
	if weekly, _ := backupResult["weekly"].(map[string]any); weekly["path"] != nil || weekly["name"] != "weekly.zip" {
		t.Fatalf("weekly backup locator was not selectively redacted: %#v", weekly)
	}
	if backupResult["sha256"] == nil {
		t.Fatal("safe backup metadata was removed")
	}

	restoreResult := map[string]any{"checkpoint": map[string]any{"path": "/private/checkpoint.zip", "sha256": "safe"}}
	stripJobFields("restore", map[string]any{"archive": encryptedValuePlaceholder}, restoreResult)
	checkpoint := restoreResult["checkpoint"].(map[string]any)
	if _, ok := checkpoint["path"]; ok || checkpoint["sha256"] == nil {
		t.Fatal("restore checkpoint path was not safely redacted")
	}

	deleteResult := map[string]any{"quarantine_path": `C:\private\instances\.quarantine\entry`, "quarantine_entry": "safe-20260724T123456Z"}
	stripJobFields("delete", map[string]any{}, deleteResult)
	if _, ok := deleteResult["quarantine_path"]; ok || deleteResult["quarantine_entry"] == nil {
		t.Fatal("delete result exposed an absolute quarantine path or removed its safe basename")
	}

	abortPayload := map[string]any{"upload_id": "upload-safe", "unexpected_body": "secret"}
	abortResult := map[string]any{"upload_id": "upload-safe", "path": "/private/backup-uploads/upload-safe.part", "diagnostic": "secret"}
	stripJobFields("backup.upload.abort", abortPayload, abortResult)
	if len(abortPayload) != 1 || abortPayload["upload_id"] != "upload-safe" || len(abortResult) != 1 || abortResult["upload_id"] != "upload-safe" {
		t.Fatalf("backup upload abort was not reduced to safe metadata: payload=%#v result=%#v", abortPayload, abortResult)
	}
	persistedAbortResult := map[string]any{"upload_id": "upload-safe", "path": "/private/staging", "diagnostic": "secret"}
	if err := sealJobResultSecrets([]byte("0123456789abcdef0123456789abcdef"), "backup.upload.abort", persistedAbortResult); err != nil {
		t.Fatal(err)
	}
	if len(persistedAbortResult) != 1 || persistedAbortResult["upload_id"] != "upload-safe" {
		t.Fatalf("backup upload abort persisted unsafe terminal fields: %#v", persistedAbortResult)
	}
}

func TestManagedFileContentIsEncryptedAtRest(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	payload := map[string]any{"operation": "write", "relative_path": "left4dead2/cfg/server.cfg", "content": "sv_password secret\n"}
	if err := sealJobPayloadSecrets(key, "file", payload); err != nil {
		t.Fatal(err)
	}
	if payload["content"] != encryptedValuePlaceholder || payload["content_ciphertext"] == "" {
		t.Fatalf("file payload was not protected: %#v", payload)
	}
	if err := unsealJobPayloadSecrets(key, "file", payload); err != nil {
		t.Fatal(err)
	}
	if payload["content"] != "sv_password secret\n" {
		t.Fatalf("round trip content = %#v", payload["content"])
	}

	result := map[string]any{"content_base64": "c3ZfcGFzc3dvcmQgc2VjcmV0Cg==", "sha256": "digest"}
	if err := sealJobResultSecrets(key, "file", result); err != nil {
		t.Fatal(err)
	}
	if result["content_base64"] != encryptedValuePlaceholder || result["content_base64_ciphertext"] == "" {
		t.Fatalf("file result was not protected: %#v", result)
	}
	unsealed, err := unsealFileJobResult(key, result)
	if err != nil {
		t.Fatal(err)
	}
	if unsealed["content_base64"] != "c3ZfcGFzc3dvcmQgc2VjcmV0Cg==" || unsealed["content_base64_ciphertext"] != nil {
		t.Fatalf("unsealed result = %#v", unsealed)
	}
}
