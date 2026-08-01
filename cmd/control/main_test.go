package main

import (
	"strings"
	"testing"
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
