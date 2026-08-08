//go:build windows

package main

import (
	"slices"
	"testing"
)

func TestRestrictedACLArgumentsInheritTrustedDirectoryRights(t *testing.T) {
	args := restrictedDirectoryACLArguments(`C:\L4D2Agent\data`, `(OI)(CI)M`)
	for _, expected := range []string{
		`*S-1-5-18:(OI)(CI)F`,
		`*S-1-5-32-544:(OI)(CI)F`,
		`NT SERVICE\l4d2panel-agent:(OI)(CI)M`,
	} {
		if !slices.Contains(args, expected) {
			t.Fatalf("directory ACL arguments %#v omit %q", args, expected)
		}
	}
}

func TestRestrictedACLArgumentsKeepFileRightsNonInheritable(t *testing.T) {
	args := restrictedFileACLArguments(`C:\L4D2Agent\agent.yaml`, `(M)`)
	for _, expected := range []string{
		`*S-1-5-18:(F)`,
		`*S-1-5-32-544:(F)`,
		`NT SERVICE\l4d2panel-agent:(M)`,
	} {
		if !slices.Contains(args, expected) {
			t.Fatalf("file ACL arguments %#v omit %q", args, expected)
		}
	}
	for _, argument := range args {
		if argument == `*S-1-5-18:(OI)(CI)F` || argument == `*S-1-5-32-544:(OI)(CI)F` {
			t.Fatalf("file ACL unexpectedly contains inheritable directory rights: %#v", args)
		}
	}
}
