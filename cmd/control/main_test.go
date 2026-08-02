package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"path/filepath"
	"strings"
	"testing"

	"github.com/l4d2-panel/l4d2panel/internal/control"
)

func TestReadBootstrapPasswordAcceptsSingleLineFromStdin(t *testing.T) {
	for _, test := range []struct {
		input string
		want  string
	}{
		{"correct horse battery staple\n", "correct horse battery staple"},
		{"correct horse battery staple\r\n", "correct horse battery staple"},
		{"  surrounding spaces stay  \n", "  surrounding spaces stay  "},
	} {
		got, err := readBootstrapPassword(strings.NewReader(test.input))
		if err != nil || got != test.want {
			t.Fatalf("readBootstrapPassword(%q) = %q, %v; want %q", test.input, got, err, test.want)
		}
	}
}

func TestRunSetupKeyWritesOnlyRotatingKey(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "control.sqlite3")
	t.Setenv("DATABASE_PATH", databasePath)
	t.Setenv("MASTER_KEY", base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))

	ctx := context.Background()
	store, err := control.OpenStore(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx, filepath.Join("..", "..", "migrations", "sqlite")); err != nil {
		store.Close()
		t.Fatal(err)
	}
	store.Close()

	var firstOutput bytes.Buffer
	if err := runSetupKey([]string{"issue"}, &firstOutput); err != nil {
		t.Fatal(err)
	}
	first := strings.TrimSuffix(firstOutput.String(), "\n")
	if !control.ValidSetupKey(first) || firstOutput.String() != first+"\n" {
		t.Fatalf("setup-key output was not exactly one valid key: %q", firstOutput.String())
	}

	var secondOutput bytes.Buffer
	if err := runSetupKey([]string{"issue"}, &secondOutput); err != nil {
		t.Fatal(err)
	}
	second := strings.TrimSuffix(secondOutput.String(), "\n")
	if !control.ValidSetupKey(second) || second == first {
		t.Fatalf("setup key was not rotated: first=%q second=%q", first, second)
	}
}

func TestControlServiceConfigurationContainsOnlyConfigPath(t *testing.T) {
	configPath := filepath.Join(`C:\ProgramData`, "L4D2Panel", "control.json")
	cfg := controlServiceConfiguration(configPath)
	if cfg.Name != controlServiceName || cfg.UserName != `NT SERVICE\l4d2panel-control` {
		t.Fatalf("unexpected service identity: %+v", cfg)
	}
	want := []string{"serve", "--config", configPath}
	if strings.Join(cfg.Arguments, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("unexpected service arguments: %#v", cfg.Arguments)
	}
	for _, argument := range cfg.Arguments {
		if strings.Contains(argument, "master_key") {
			t.Fatalf("secret leaked into service arguments: %q", argument)
		}
	}
}

func TestDefaultControlConfigPathUsesProgramDataConfigDirectory(t *testing.T) {
	t.Setenv("ProgramData", filepath.Join("C:", "ProgramData-Test"))
	want := filepath.Join("C:", "ProgramData-Test", "L4D2Panel", "config", "control.json")
	if got := defaultControlConfigPath(); got != want {
		t.Fatalf("defaultControlConfigPath() = %q; want %q", got, want)
	}
}

func TestReadBootstrapPasswordRejectsMultilineNULAndOversize(t *testing.T) {
	for _, input := range []string{
		"first\nsecond\n",
		"unsafe\x00password\n",
		strings.Repeat("a", 1025) + "\n",
	} {
		if _, err := readBootstrapPassword(strings.NewReader(input)); err == nil {
			t.Fatalf("unsafe bootstrap password was accepted (length %d)", len(input))
		}
	}
}
