package control

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
)

const requestFingerprintVersion = 1

// canonicalRequestFingerprint produces the identity for trusted, secret-free
// internal work. encoding/json sorts object keys, so equivalent objects have a
// stable representation regardless of map iteration or JSON field order. HTTP
// requests and any value that may contain a secret must use the keyed variant.
func canonicalRequestFingerprint(scope, target, operation string, request any) (string, error) {
	canonical, err := canonicalRequestEnvelope(scope, target, operation, request)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}

// keyedRequestFingerprint is used at HTTP trust boundaries. The deployment
// master key prevents a database snapshot containing fingerprints from being
// used to enumerate low-entropy passwords, commands, URLs, or file contents.
// It must still be called before those fields are encrypted so retries remain
// stable across fresh encryption nonces.
func keyedRequestFingerprint(masterKey []byte, scope, target, operation string, request any) (string, error) {
	canonical, err := canonicalRequestEnvelope(scope, target, operation, request)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, masterKey)
	_, _ = mac.Write([]byte("l4d2panel:idempotency:v1\x00"))
	_, _ = mac.Write(canonical)
	return hex.EncodeToString(mac.Sum(nil)), nil
}

func canonicalRequestEnvelope(scope, target, operation string, request any) ([]byte, error) {
	envelope := struct {
		Version   int    `json:"version"`
		Scope     string `json:"scope"`
		Target    string `json:"target"`
		Operation string `json:"operation"`
		Request   any    `json:"request"`
	}{
		Version: requestFingerprintVersion, Scope: scope, Target: target,
		Operation: operation, Request: request,
	}
	return json.Marshal(envelope)
}

func instanceJobRequestFingerprint(instanceID, action string, payload map[string]any) (string, error) {
	if payload == nil {
		payload = map[string]any{}
	}
	return canonicalRequestFingerprint("instance-job", instanceID, action, payload)
}

func nodeJobRequestFingerprint(nodeID, action string, payload map[string]any) (string, error) {
	if payload == nil {
		payload = map[string]any{}
	}
	return canonicalRequestFingerprint("node-job", nodeID, action, payload)
}

func validRequestFingerprint(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func requestFingerprintsEqual(stored, expected string) bool {
	if !validRequestFingerprint(stored) || !validRequestFingerprint(expected) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(stored), []byte(expected)) == 1
}

func requireRequestFingerprint(idempotencyKey, fingerprint string) error {
	if idempotencyKey == "" {
		return nil
	}
	if !validRequestFingerprint(fingerprint) {
		return errors.New("a valid request fingerprint is required with Idempotency-Key")
	}
	return nil
}
