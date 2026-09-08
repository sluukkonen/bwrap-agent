//go:build linux

package app

import (
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestParseIdentityMappingsPreservesNamespaceExtents(t *testing.T) {
	mappings, err := parseIdentityMappings("         0       1003          1\n         1     720896      65536\n")
	if err != nil {
		t.Fatal(err)
	}
	want := []syscall.SysProcIDMap{
		{ContainerID: 0, HostID: 0, Size: 1},
		{ContainerID: 1, HostID: 1, Size: 65536},
	}
	if len(mappings) != len(want) {
		t.Fatalf("mappings = %#v, want %#v", mappings, want)
	}
	for index := range want {
		if mappings[index] != want[index] {
			t.Fatalf("mapping %d = %#v, want %#v", index, mappings[index], want[index])
		}
	}
}

func TestParseIdentityMappingsRejectsUnsafeInput(t *testing.T) {
	tests := []struct {
		name    string
		content string
		message string
	}{
		{"empty", "", "does not cover"},
		{"missing-root", "1 100000 65536\n", "does not cover"},
		{"zero-size", "0 1000 0\n", "invalid ID count"},
		{"overlap", "0 1000 10\n5 2000 10\n", "overlapping"},
		{"overflow", "4294967295 1000 2\n", "overflowing"},
		{"malformed", "0 1000\n", "invalid ID map line"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseIdentityMappings(test.content)
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("error = %v, want %q", err, test.message)
			}
		})
	}
}

func TestLockedSandboxCommandUsesChildUserAndMountNamespaces(t *testing.T) {
	mapping := []syscall.SysProcIDMap{{ContainerID: 0, HostID: 0, Size: 65537}}
	command := newLockedSandboxCommand([]string{"/bin/true"}, mapping, mapping)
	if command.Path != "/run/bwrap-agent/init" || len(command.Args) < 3 || command.Args[1] != internalRuntimeMode {
		t.Fatalf("unexpected locked command: %#v", command)
	}
	wantFlags := uintptr(unix.CLONE_NEWUSER | unix.CLONE_NEWNS)
	if command.SysProcAttr == nil || command.SysProcAttr.Cloneflags&wantFlags != wantFlags {
		t.Fatalf("namespace flags = %#v, want %#x", command.SysProcAttr, wantFlags)
	}
	if len(command.SysProcAttr.UidMappings) == 0 || len(command.SysProcAttr.GidMappings) == 0 {
		t.Fatalf("identity mappings missing: %#v", command.SysProcAttr)
	}
	if command.SysProcAttr.GidMappingsEnableSetgroups {
		t.Fatal("setgroups unexpectedly enabled")
	}
}
