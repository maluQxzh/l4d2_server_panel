package control

import (
	"encoding/json"
	"errors"
	"net/url"
	"strings"
)

const encryptedValuePlaceholder = "[encrypted]"

var errTransferResultConsumed = errors.New("transfer result is missing or was already consumed")

// sealJobPayloadSecrets keeps commands and signed download URLs available to
// an offline Agent without storing their clear text in SQLite.
func sealJobPayloadSecrets(masterKey []byte, jobType string, payload map[string]any) error {
	switch jobType {
	case "rcon":
		command := stringFromAny(payload["command"])
		if command == "" || len(command) > 4096 || strings.ContainsAny(command, "\x00\r\n") {
			return errors.New("RCON command must contain 1 to 4096 characters without line breaks")
		}
		sealed, err := encryptSecret(masterKey, command)
		if err != nil {
			return err
		}
		payload["command_ciphertext"] = sealed
		payload["command"] = summarizeRCONCommand(command)
	case "restart":
		secret := stringFromAny(payload["existing_rcon_password"])
		if secret == "" {
			return nil
		}
		sealed, err := encryptSecret(masterKey, secret)
		if err != nil {
			return err
		}
		payload["existing_rcon_password_ciphertext"] = sealed
		payload["existing_rcon_password"] = encryptedValuePlaceholder
	case "qq_flash_import", "map.import.qq_flash":
		source := stringFromAny(payload["source"])
		if source == "" {
			return errors.New("QQ flash share URL is required")
		}
		sealed, err := encryptSecret(masterKey, source)
		if err != nil {
			return err
		}
		payload["source_ciphertext"] = sealed
		payload["source"] = encryptedValuePlaceholder
	case "install_mod", "install_package", "inspect_plugin_archive":
		source := stringFromAny(payload["source"])
		parsed, err := url.Parse(source)
		if err == nil && strings.EqualFold(parsed.Scheme, "https") && parsed.Host != "" {
			sealed, err := encryptSecret(masterKey, source)
			if err != nil {
				return err
			}
			payload["source_ciphertext"] = sealed
			parsed.RawQuery, parsed.Fragment, parsed.User = "", "", nil
			payload["source"] = parsed.String()
		}
		content := stringFromAny(payload["content_base64"])
		if content != "" {
			sealed, err := encryptSecret(masterKey, content)
			if err != nil {
				return err
			}
			payload["content_base64_ciphertext"] = sealed
			payload["content_base64"] = encryptedValuePlaceholder
		}
	case "file":
		operation := stringFromAny(payload["operation"])
		if strings.EqualFold(operation, "qq_flash") {
			shareURL := stringFromAny(payload["share_url"])
			if shareURL == "" {
				return errors.New("QQ flash share URL is required")
			}
			sealed, err := encryptSecret(masterKey, shareURL)
			if err != nil {
				return err
			}
			payload["share_url_ciphertext"] = sealed
			payload["share_url"] = encryptedValuePlaceholder
			return nil
		}
		if !strings.EqualFold(operation, "write") && !strings.EqualFold(operation, "smart_plugin_edit") {
			return nil
		}
		for _, field := range []string{"content", "content_base64"} {
			value := stringFromAny(payload[field])
			if value == "" {
				continue
			}
			sealed, err := encryptSecret(masterKey, value)
			if err != nil {
				return err
			}
			payload[field+"_ciphertext"] = sealed
			payload[field] = encryptedValuePlaceholder
		}
	case "backup.upload.chunk":
		content := stringFromAny(payload["content_base64"])
		if content == "" || len(content) > 2*maxHTTPTransferChunk {
			return errors.New("backup upload chunk content is required and exceeds the encrypted payload limit")
		}
		sealed, err := encryptSecret(masterKey, content)
		if err != nil {
			return err
		}
		payload["content_base64_ciphertext"] = sealed
		payload["content_base64"] = encryptedValuePlaceholder
	case "restore", "backup.chunk":
		archive := stringFromAny(payload["archive"])
		if jobType == "restore" && stringFromAny(payload["upload_id"]) != "" {
			return nil
		}
		if archive == "" {
			return errors.New("backup archive path is required")
		}
		sealed, err := encryptSecret(masterKey, archive)
		if err != nil {
			return err
		}
		payload["archive_ciphertext"] = sealed
		payload["archive"] = encryptedValuePlaceholder
	case "logs.search":
		keyword := stringFromAny(payload["keyword"])
		if keyword == "" {
			return nil
		}
		sealed, err := encryptSecret(masterKey, keyword)
		if err != nil {
			return err
		}
		payload["keyword_ciphertext"] = sealed
		payload["keyword"] = encryptedValuePlaceholder
	}
	return nil
}

func unsealJobPayloadSecrets(masterKey []byte, jobType string, payload map[string]any) error {
	var pairs [][2]string
	switch jobType {
	case "rcon":
		pairs = append(pairs, [2]string{"command_ciphertext", "command"})
	case "restart":
		pairs = append(pairs, [2]string{"existing_rcon_password_ciphertext", "existing_rcon_password"})
	case "install_mod", "install_package", "inspect_plugin_archive", "qq_flash_import", "map.import.qq_flash":
		pairs = append(pairs, [2]string{"source_ciphertext", "source"}, [2]string{"content_base64_ciphertext", "content_base64"})
	case "file":
		pairs = append(pairs, [2]string{"content_ciphertext", "content"}, [2]string{"content_base64_ciphertext", "content_base64"},
			[2]string{"share_url_ciphertext", "share_url"})
	case "backup.upload.chunk":
		pairs = append(pairs, [2]string{"content_base64_ciphertext", "content_base64"})
	case "restore", "backup.chunk":
		pairs = append(pairs, [2]string{"archive_ciphertext", "archive"})
	case "logs.search":
		pairs = append(pairs, [2]string{"keyword_ciphertext", "keyword"})
	}
	for _, pair := range pairs {
		ciphertext := stringFromAny(payload[pair[0]])
		if ciphertext == "" {
			continue
		}
		plaintext, err := decryptSecret(masterKey, ciphertext)
		if err != nil {
			return err
		}
		payload[pair[1]] = plaintext
		delete(payload, pair[0])
	}
	return nil
}

func sealJobResultSecrets(masterKey []byte, jobType string, result map[string]any) error {
	if result == nil {
		return nil
	}
	if jobType == "backup.upload.abort" {
		// Persist only the protocol field this operation is defined to return.
		// In particular, never retain an Agent filesystem path or diagnostic.
		retainOnlyJobField(result, "upload_id")
		return nil
	}
	field := ""
	switch jobType {
	case "rcon":
		field = "response"
	case "file", "logs.chunk", "backup.chunk":
		field = "content_base64"
	case "logs.search":
		raw, err := json.Marshal(result["entries"])
		if err != nil {
			return err
		}
		if len(raw) == 0 {
			raw = []byte("[]")
		}
		sealed, err := encryptSecret(masterKey, string(raw))
		if err != nil {
			return err
		}
		result["entries_ciphertext"] = sealed
		delete(result, "entries")
		return nil
	}
	value := stringFromAny(result[field])
	if field == "" || value == "" {
		return nil
	}
	sealed, err := encryptSecret(masterKey, value)
	if err != nil {
		return err
	}
	result[field+"_ciphertext"] = sealed
	result[field] = encryptedValuePlaceholder
	return nil
}

func unsealFileJobResult(masterKey []byte, result map[string]any) (map[string]any, error) {
	copyResult := cloneMap(result)
	ciphertext := stringFromAny(copyResult["content_base64_ciphertext"])
	if ciphertext == "" {
		return copyResult, nil
	}
	plaintext, err := decryptSecret(masterKey, ciphertext)
	if err != nil {
		return nil, err
	}
	copyResult["content_base64"] = plaintext
	delete(copyResult, "content_base64_ciphertext")
	return copyResult, nil
}

func unsealTransferJobResult(masterKey []byte, jobType string, result map[string]any) (map[string]any, error) {
	copyResult := cloneMap(result)
	field := "content_base64"
	if jobType == "logs.search" {
		field = "entries"
	}
	ciphertext := stringFromAny(copyResult[field+"_ciphertext"])
	if ciphertext == "" {
		return nil, errTransferResultConsumed
	}
	plaintext, err := decryptSecret(masterKey, ciphertext)
	if err != nil {
		return nil, err
	}
	if jobType == "logs.search" {
		var entries []map[string]any
		if err := json.Unmarshal([]byte(plaintext), &entries); err != nil {
			return nil, errors.New("log search result is corrupted")
		}
		copyResult[field] = entries
	} else {
		copyResult[field] = plaintext
	}
	delete(copyResult, field+"_ciphertext")
	return copyResult, nil
}

func stripEncryptedJobFields(jobPayload, jobResult map[string]any) {
	scrubJobMap(jobPayload)
	scrubJobMap(jobResult)
}

func stripJobFields(jobType string, jobPayload, jobResult map[string]any) {
	stripEncryptedJobFields(jobPayload, jobResult)
	delete(jobPayload, jobScopeInstancePayloadField)
	switch jobType {
	case "delete", "detach":
		delete(jobResult, "quarantine_path")
	case "backup":
		delete(jobResult, "path")
		delete(jobResult, "weekly_path")
		delete(jobResult, "pruned_paths")
		if weekly, ok := jobResult["weekly"].(map[string]any); ok {
			delete(weekly, "path")
		}
	case "plugin.backup":
		delete(jobPayload, "plugin_packages")
		delete(jobResult, "path")
	case "restore":
		delete(jobPayload, "archive")
		if checkpoint, ok := jobResult["checkpoint"].(map[string]any); ok {
			delete(checkpoint, "path")
			delete(checkpoint, "weekly_path")
		}
	case "backup.upload.abort":
		// The Agent operation only has one safe input/output field. Retain that
		// identifier and fail closed against an unexpected path or diagnostic
		// field from a compromised or newer Agent.
		retainOnlyJobField(jobPayload, "upload_id")
		retainOnlyJobField(jobResult, "upload_id")
	case "map.import.qq_flash":
		delete(jobResult, "poster_base64")
		delete(jobResult, "poster_mime")
	}
}

func retainOnlyJobField(values map[string]any, field string) {
	for key := range values {
		if key != field {
			delete(values, key)
		}
	}
}

func scrubJobMap(values map[string]any) {
	for key, value := range values {
		lower := strings.ToLower(key)
		switch lower {
		case "rcon_password", "existing_rcon_password", "server_password", "storage_path", "snapshot_path", "snapshot_paths", "consumed_snapshot", "pruned_snapshots", "pruned_paths", terminalOutcomeFingerprintField, jobProjectionPendingField:
			delete(values, key)
			continue
		}
		if strings.HasSuffix(lower, "_ciphertext") || value == encryptedValuePlaceholder {
			delete(values, key)
			continue
		}
		scrubJobValue(value)
	}
}

func scrubJobValue(value any) {
	switch nested := value.(type) {
	case map[string]any:
		scrubJobMap(nested)
	case []any:
		for _, item := range nested {
			scrubJobValue(item)
		}
	case []map[string]any:
		for _, item := range nested {
			scrubJobMap(item)
		}
	}
}

func summarizeRCONCommand(command string) string {
	trimmed := strings.TrimSpace(command)
	lower := strings.ToLower(trimmed)
	for _, marker := range []string{"rcon_password", "sv_password", "password ", "token ", "secret ", "auth "} {
		if strings.Contains(lower, marker) {
			fields := strings.Fields(trimmed)
			if len(fields) == 0 {
				return "[REDACTED]"
			}
			return fields[0] + " [REDACTED]"
		}
	}
	return trimmed
}
