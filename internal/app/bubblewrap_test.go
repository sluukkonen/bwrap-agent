package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeBubblewrap(t *testing.T, version string, mode os.FileMode) string {
	return fakeBubblewrapWithHelp(t, version, mode, "--overlay-src --tmp-overlay --remount-ro --disable-userns --ro-bind-data --perms --seccomp")
}

func fakeBubblewrapWithHelp(t *testing.T, version string, mode os.FileMode, help string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bwrap")
	content := "#!/bin/sh\nif [ \"$1\" = --version ]; then echo bubblewrap " + version + "; else echo " + help + "; fi\n"
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestInspectBubblewrapCompatibility(t *testing.T) {
	legacy, err := inspectBubblewrap(fakeBubblewrap(t, "0.11.2", 0o755))
	if err != nil || !legacy.Legacy || legacy.Version != "0.11.2" {
		t.Fatalf("legacy inspection = %#v, %v", legacy, err)
	}
	current, err := inspectBubblewrap(fakeBubblewrap(t, "0.12.0", 0o755))
	if err != nil || current.Legacy || current.Version != "0.12.0" {
		t.Fatalf("current inspection = %#v, %v", current, err)
	}
}

func TestInspectBubblewrapRejectsOldAndSetuid(t *testing.T) {
	if _, err := inspectBubblewrap(fakeBubblewrap(t, "0.10.0", 0o755)); err == nil || !strings.Contains(err.Error(), "0.11 or newer") {
		t.Fatalf("old Bubblewrap error = %v", err)
	}
	if err := validateBubblewrapFileMode("/usr/bin/bwrap", os.ModeSetuid|0o755); err == nil || !strings.Contains(err.Error(), "setuid") {
		t.Fatalf("setuid Bubblewrap error = %v", err)
	}
	missingReadOnlyData := fakeBubblewrapWithHelp(t, "0.12.0", 0o755, "--overlay-src --tmp-overlay --remount-ro --disable-userns --file --perms")
	if _, err := inspectBubblewrap(missingReadOnlyData); err == nil || !strings.Contains(err.Error(), "lacks required option --ro-bind-data") {
		t.Fatalf("missing --ro-bind-data error = %v", err)
	}
	missingSeccomp := fakeBubblewrapWithHelp(t, "0.12.0", 0o755, "--overlay-src --tmp-overlay --remount-ro --disable-userns --ro-bind-data --perms")
	if _, err := inspectBubblewrap(missingSeccomp); err == nil || !strings.Contains(err.Error(), "lacks required option --seccomp") {
		t.Fatalf("missing --seccomp error = %v", err)
	}
}
