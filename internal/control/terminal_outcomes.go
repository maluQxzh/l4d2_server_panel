package control

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"strings"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
	"github.com/l4d2-panel/l4d2panel/internal/protocol"
)

const (
	terminalOutcomeFingerprintField = "_terminal_outcome_fingerprint"
	jobProjectionPendingField       = "_projection_pending"
)

var (
	errTerminalOutcomeConflict = errors.New("terminal job outcome does not match the persisted outcome")
	errReservedTerminalField   = errors.New("terminal job result contains a reserved control field")
)

func terminalJobUpdateFingerprint(masterKey []byte, jobID string, update protocol.JobUpdate) (string, error) {
	result := cloneMap(update.Result)
	delete(result, terminalOutcomeFingerprintField)
	delete(result, jobProjectionPendingField)
	outcome := struct {
		Status   domain.JobStatus `json:"status"`
		Progress int              `json:"progress"`
		Message  string           `json:"message"`
		Result   map[string]any   `json:"result"`
	}{Status: update.Status, Progress: update.Progress, Message: update.Message, Result: result}
	return keyedRequestFingerprint(masterKey, "agent-job-terminal", jobID, "outcome", outcome)
}

func settledTerminalJob(job domain.Job) bool {
	if !isTerminalJobStatus(job.Status) {
		return false
	}
	pending, _ := job.Result["cancel_pending"].(bool)
	return !pending
}

func requiresDeferredJobProjection(jobType string, payload map[string]any, status domain.JobStatus) bool {
	switch status {
	case domain.JobSucceeded:
		if strings.HasPrefix(jobType, "map.") {
			switch jobType {
			case "map.import.qq_flash", "map.assign", "map.unassign", "map.verify", "map.inventory", "map.delete":
				return true
			}
		}
		switch jobType {
		case "environment.inspect", "steamcmd.configure", "steamcmd.install", "steamcmd.repair", "steamcmd.clear",
			"configure", "game.install", "game.import", "game.validate", "game.update", "game.delete",
			"start", "restart", "stop", "install", "import", "update", "validate", "a2s.info", "check_update",
			"backup.upload.chunk", "backup.upload.abort", "backup", "plugin.backup", "backup.delete", "install_mod", "scan_packages",
			"rollback_package", "toggle_package", "rename_package", "delete_package", "ban_player", "unban_player", "delete", "detach",
			"purge_quarantine":
			return true
		case "file":
			return strings.EqualFold(stringFromAny(payload["operation"]), "smart_plugin_edit")
		case "qq_flash_import":
			return stringFromAny(payload["mode"]) == "managed_vpk_v2"
		}
	case domain.JobFailed:
		switch jobType {
		case "configure", "start", "stop", "restart", "install", "import", "update", "validate",
			"game.install", "game.import", "game.validate", "game.update", "game.delete",
			"steamcmd.configure", "steamcmd.install", "steamcmd.repair", "steamcmd.clear", "environment.inspect",
			"purge_quarantine", "map.delete", "map.verify", "map.inventory":
			return true
		}
	}
	return false
}

var terminalPayloadSecretFields = [][2]string{
	{"command_ciphertext", "command"},
	{"existing_rcon_password_ciphertext", "existing_rcon_password"},
	{"source_ciphertext", "source"},
	{"content_ciphertext", "content"},
	{"content_base64_ciphertext", "content_base64"},
	{"share_url_ciphertext", "share_url"},
	{"archive_ciphertext", "archive"},
	{"keyword_ciphertext", "keyword"},
}

// scrubTerminalJobPayload removes every one-shot secret needed only while a
// task can still be dispatched. It deliberately keeps non-secret routing and
// projection metadata. Doing this in Go avoids SQLite's JSON functions
// materializing a near-limit payload in a small container /tmp filesystem.
func scrubTerminalJobPayload(payload map[string]any) (map[string]any, bool) {
	cleaned := cloneMap(payload)
	changed := false
	for _, pair := range terminalPayloadSecretFields {
		if _, exists := cleaned[pair[0]]; exists {
			delete(cleaned, pair[0])
			changed = true
		}
		if plaintext, ok := cleaned[pair[1]].(string); ok && plaintext == encryptedValuePlaceholder {
			delete(cleaned, pair[1])
			changed = true
		}
	}
	return cleaned, changed
}

func jobProjectionPending(job domain.Job) bool {
	pending, _ := job.Result[jobProjectionPendingField].(bool)
	return pending
}

func terminalFingerprintsEqual(first, second string) bool {
	if !validRequestFingerprint(first) || !validRequestFingerprint(second) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(first), []byte(second)) == 1
}

// persistedTerminalOutcomeEquivalent is the Store-level race guard. Current
// Control writes carry a keyed fingerprint. The structural fallback supports
// direct Store callers and legacy non-secret outcomes without weakening the
// rule that a settled terminal row is immutable.
func persistedTerminalOutcomeEquivalent(current domain.Job, status domain.JobStatus, progress int, message string, result map[string]any) bool {
	if !settledTerminalJob(current) {
		return false
	}
	storedFingerprint := stringFromAny(current.Result[terminalOutcomeFingerprintField])
	incomingFingerprint := stringFromAny(result[terminalOutcomeFingerprintField])
	if storedFingerprint != "" || incomingFingerprint != "" {
		return terminalFingerprintsEqual(storedFingerprint, incomingFingerprint)
	}
	if current.Status != status || current.Progress != progress || current.Message != message {
		return false
	}
	storedResult := cloneMap(current.Result)
	incomingResult := cloneMap(result)
	delete(storedResult, terminalOutcomeFingerprintField)
	delete(incomingResult, terminalOutcomeFingerprintField)
	delete(storedResult, jobProjectionPendingField)
	delete(incomingResult, jobProjectionPendingField)
	storedRaw, storedErr := json.Marshal(storedResult)
	incomingRaw, incomingErr := json.Marshal(incomingResult)
	return storedErr == nil && incomingErr == nil && bytes.Equal(storedRaw, incomingRaw)
}

// terminalReplayEquivalent validates a replay before a fresh randomized
// encryption nonce is produced. Fingerprinted rows compare in constant time;
// legacy rows are decrypted in memory and fingerprinted in their original
// plaintext form.
func terminalReplayEquivalent(masterKey []byte, job domain.Job, update protocol.JobUpdate, incomingFingerprint string) (bool, error) {
	if !settledTerminalJob(job) {
		return false, nil
	}
	storedFingerprint := stringFromAny(job.Result[terminalOutcomeFingerprintField])
	if storedFingerprint != "" {
		return terminalFingerprintsEqual(storedFingerprint, incomingFingerprint), nil
	}
	plaintext, err := plaintextTerminalResult(masterKey, job.Type, job.Result)
	if err != nil {
		return false, err
	}
	storedFingerprint, err = terminalJobUpdateFingerprint(masterKey, job.ID, protocol.JobUpdate{
		Status: job.Status, Progress: job.Progress, Message: job.Message, Result: plaintext,
	})
	if err != nil {
		return false, err
	}
	return terminalFingerprintsEqual(storedFingerprint, incomingFingerprint), nil
}

func plaintextTerminalResult(masterKey []byte, jobType string, persisted map[string]any) (map[string]any, error) {
	result := cloneMap(persisted)
	delete(result, terminalOutcomeFingerprintField)
	delete(result, jobProjectionPendingField)
	decryptField := func(field string) error {
		ciphertext := stringFromAny(result[field+"_ciphertext"])
		if ciphertext == "" {
			if result[field] == encryptedValuePlaceholder {
				return errors.New("persisted terminal secret was already consumed")
			}
			return nil
		}
		plaintext, err := decryptSecret(masterKey, ciphertext)
		if err != nil {
			return err
		}
		result[field] = plaintext
		delete(result, field+"_ciphertext")
		return nil
	}
	switch jobType {
	case "rcon":
		if err := decryptField("response"); err != nil {
			return nil, err
		}
	case "file", "logs.chunk", "backup.chunk":
		if err := decryptField("content_base64"); err != nil {
			return nil, err
		}
	case "logs.search":
		ciphertext := stringFromAny(result["entries_ciphertext"])
		if ciphertext == "" {
			if _, exists := result["entries"]; !exists {
				return nil, errors.New("persisted terminal log search result was already consumed")
			}
			break
		}
		plaintext, err := decryptSecret(masterKey, ciphertext)
		if err != nil {
			return nil, err
		}
		var entries any
		if err := json.Unmarshal([]byte(plaintext), &entries); err != nil {
			return nil, err
		}
		result["entries"] = entries
		delete(result, "entries_ciphertext")
	}
	return result, nil
}
