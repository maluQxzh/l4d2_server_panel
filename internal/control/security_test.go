package control

import (
	"strings"
	"testing"
)

func TestPasswordHashRoundTrip(t *testing.T) {
	encoded, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(encoded, "$argon2id$") {
		t.Fatalf("unexpected encoding: %s", encoded)
	}
	if !VerifyPassword(encoded, "correct horse battery staple") {
		t.Fatal("correct password was rejected")
	}
	if VerifyPassword(encoded, "incorrect horse battery staple") {
		t.Fatal("incorrect password was accepted")
	}
}

func TestPasswordHashRejectsWeakPassword(t *testing.T) {
	if _, err := HashPassword("short"); err == nil {
		t.Fatal("expected weak password to be rejected")
	}
}

func TestSecretEncryptionRoundTripAndAuthentication(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	ciphertext, err := encryptSecret(key, "rcon-secret")
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := decryptSecret(key, ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	if plaintext != "rcon-secret" {
		t.Fatalf("got %q", plaintext)
	}
	wrong := []byte("abcdef0123456789abcdef0123456789")
	if _, err := decryptSecret(wrong, ciphertext); err == nil {
		t.Fatal("ciphertext authenticated with the wrong key")
	}
}

func TestDerivedCSRFIsStableAndBoundToSession(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	one := deriveCSRFToken(key, "session-one")
	if one == "" || one != deriveCSRFToken(key, "session-one") {
		t.Fatal("CSRF token was not stable")
	}
	if one == deriveCSRFToken(key, "session-two") {
		t.Fatal("CSRF token was not bound to the session")
	}
}
