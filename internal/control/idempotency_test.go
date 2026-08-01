package control

import (
	"testing"
)

func TestCanonicalRequestFingerprintIsStableAndRequestBound(t *testing.T) {
	t.Parallel()
	first, err := canonicalRequestFingerprint("instance-job", "instance-a", "rcon", map[string]any{
		"command": "status", "nested": map[string]any{"b": float64(2), "a": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	orderedDifferently, err := canonicalRequestFingerprint("instance-job", "instance-a", "rcon", map[string]any{
		"nested": map[string]any{"a": true, "b": float64(2)}, "command": "status",
	})
	if err != nil {
		t.Fatal(err)
	}
	if first != orderedDifferently || !validRequestFingerprint(first) {
		t.Fatalf("canonical fingerprints differ or are invalid: %q %q", first, orderedDifferently)
	}
	for name, candidate := range map[string]struct {
		target, operation string
		payload           map[string]any
	}{
		"target":    {"instance-b", "rcon", map[string]any{"command": "status", "nested": map[string]any{"a": true, "b": float64(2)}}},
		"operation": {"instance-a", "broadcast", map[string]any{"command": "status", "nested": map[string]any{"a": true, "b": float64(2)}}},
		"payload":   {"instance-a", "rcon", map[string]any{"command": "quit", "nested": map[string]any{"a": true, "b": float64(2)}}},
	} {
		t.Run(name, func(t *testing.T) {
			got, fingerprintErr := canonicalRequestFingerprint("instance-job", candidate.target, candidate.operation, candidate.payload)
			if fingerprintErr != nil {
				t.Fatal(fingerprintErr)
			}
			if got == first {
				t.Fatalf("changed %s retained request fingerprint %q", name, got)
			}
		})
	}
}

func TestSecretRequestFingerprintPrecedesRandomEncryption(t *testing.T) {
	t.Parallel()
	key := []byte("0123456789abcdef0123456789abcdef")
	plaintext := map[string]any{"command": "sm_cvar rcon_password hunter2"}
	want, err := keyedRequestFingerprint(key, "instance-action", "instance-a", "rcon", plaintext)
	if err != nil {
		t.Fatal(err)
	}
	first := cloneMap(plaintext)
	second := cloneMap(plaintext)
	if err := sealJobPayloadSecrets(key, "rcon", first); err != nil {
		t.Fatal(err)
	}
	if err := sealJobPayloadSecrets(key, "rcon", second); err != nil {
		t.Fatal(err)
	}
	if first["command_ciphertext"] == second["command_ciphertext"] {
		t.Fatal("test encryption unexpectedly reused a nonce")
	}
	retry, err := keyedRequestFingerprint(key, "instance-action", "instance-a", "rcon", plaintext)
	if err != nil || retry != want {
		t.Fatalf("plaintext retry fingerprint = %q, want %q (err=%v)", retry, want, err)
	}
	sealed, err := keyedRequestFingerprint(key, "instance-action", "instance-a", "rcon", first)
	if err != nil {
		t.Fatal(err)
	}
	if sealed == want {
		t.Fatal("post-encryption payload unexpectedly has the plaintext request identity")
	}
	if len(want) != 64 || want == stringFromAny(plaintext["command"]) {
		t.Fatalf("fingerprint leaked plaintext or had an invalid shape: %q", want)
	}
	otherKeyFingerprint, err := keyedRequestFingerprint([]byte("abcdef0123456789abcdef0123456789"),
		"instance-action", "instance-a", "rcon", plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if otherKeyFingerprint == want {
		t.Fatal("different deployment master keys produced the same secret request fingerprint")
	}
}

func TestMissingOrMalformedStoredFingerprintNeverMatches(t *testing.T) {
	t.Parallel()
	valid, err := instanceJobRequestFingerprint("instance-a", "start", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	for _, stored := range []string{"", "legacy", valid[:63], valid + "0"} {
		if requestFingerprintsEqual(stored, valid) {
			t.Fatalf("unsafe stored fingerprint %q matched", stored)
		}
	}
}

func TestKeyedCreateAndConfigFingerprintsBindSecretsAndVersion(t *testing.T) {
	t.Parallel()
	key := []byte("0123456789abcdef0123456789abcdef")
	create := map[string]any{
		"node_id": "node-a", "name": "server", "operation": "install",
		"config": map[string]any{"hostname": "server", "server_password": "create-secret", "max_players": 8},
	}
	createFingerprint, err := keyedRequestFingerprint(key, "instance-create", "node-a", "create", create)
	if err != nil {
		t.Fatal(err)
	}
	changedCreate := cloneMap(create)
	changedCreate["config"] = map[string]any{"hostname": "server", "server_password": "other-secret", "max_players": 8}
	changedCreateFingerprint, err := keyedRequestFingerprint(key, "instance-create", "node-a", "create", changedCreate)
	if err != nil {
		t.Fatal(err)
	}
	if changedCreateFingerprint == createFingerprint {
		t.Fatal("create fingerprint did not bind the plaintext server password")
	}

	config := map[string]any{
		"name": "server", "version": 7, "server_password": "patch-secret",
		"config": map[string]any{"hostname": "updated", "max_players": 8},
	}
	configFingerprint, err := keyedRequestFingerprint(key, "instance-config", "instance-a", "configure", config)
	if err != nil {
		t.Fatal(err)
	}
	changedConfig := cloneMap(config)
	changedConfig["version"] = 8
	changedConfigFingerprint, err := keyedRequestFingerprint(key, "instance-config", "instance-a", "configure", changedConfig)
	if err != nil {
		t.Fatal(err)
	}
	if changedConfigFingerprint == configFingerprint {
		t.Fatal("config fingerprint did not bind the optimistic-lock version")
	}
}
