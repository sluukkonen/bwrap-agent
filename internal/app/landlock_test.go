//go:build linux

package app

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestLandlockWriteAllowlist(t *testing.T) {
	abi, err := queryLandlockABI()
	if err != nil {
		t.Fatal(err)
	}
	if abi < minimumLandlockABI {
		t.Skipf("Landlock ABI %d is below required ABI %d", abi, minimumLandlockABI)
	}
	root := t.TempDir()
	allowed := filepath.Join(root, "allowed")
	allowedFile := filepath.Join(root, "allowed-file")
	denied := filepath.Join(root, "denied")
	if err := os.Mkdir(allowed, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(denied, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(allowedFile, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=TestLandlockEnforcementHelper")
	command.Env = append(os.Environ(), "BWRAP_AGENT_LANDLOCK_TEST=1", "BWRAP_AGENT_LANDLOCK_ALLOWED="+allowed,
		"BWRAP_AGENT_LANDLOCK_ALLOWED_FILE="+allowedFile, "BWRAP_AGENT_LANDLOCK_DENIED="+denied)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Landlock helper: %v: %s", err, output)
	}
}

func TestLandlockEnforcementHelper(t *testing.T) {
	if os.Getenv("BWRAP_AGENT_LANDLOCK_TEST") != "1" {
		return
	}
	allowed := os.Getenv("BWRAP_AGENT_LANDLOCK_ALLOWED")
	allowedFile := os.Getenv("BWRAP_AGENT_LANDLOCK_ALLOWED_FILE")
	denied := os.Getenv("BWRAP_AGENT_LANDLOCK_DENIED")
	if err := applyLandlock([]string{allowed, allowedFile}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(allowed, "created"), []byte("ok"), 0o600); err != nil {
		t.Fatalf("allowed write failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(denied, "created"), []byte("blocked"), 0o600); !errors.Is(err, unix.EACCES) {
		t.Fatalf("denied write = %v, want EACCES", err)
	}
	if err := os.WriteFile(allowedFile, []byte("after"), 0o600); err != nil {
		t.Fatalf("allowed file write failed: %v", err)
	}
}

func TestDetermineLandlockStatusSkipsUnusedProbe(t *testing.T) {
	probe := func() (int, error) {
		t.Fatal("unused Landlock probe was called")
		return 0, nil
	}
	for _, test := range []struct {
		mode      string
		podman    bool
		effective string
	}{
		{mode: "off", effective: "off"},
		{mode: "auto", podman: true, effective: "skipped-podman"},
	} {
		status, err := determineLandlockStatus(test.mode, test.podman, probe)
		if err != nil || status.Effective != test.effective || status.ABI != 0 {
			t.Fatalf("status for %q/Podman=%t = %#v, %v", test.mode, test.podman, status, err)
		}
	}
}

func TestDetermineLandlockStatusHandlesBlockedProbe(t *testing.T) {
	probe := func() (int, error) { return 0, unix.EPERM }
	status, err := determineLandlockStatus("auto", false, probe)
	if err != nil || status.Effective != "unsupported" {
		t.Fatalf("automatic blocked-probe status = %#v, %v", status, err)
	}
	if _, err := determineLandlockStatus("required", false, probe); err == nil || !errors.Is(err, unix.EPERM) {
		t.Fatalf("required blocked-probe error = %v", err)
	}
}

func TestDetermineLandlockStatusRequiredWithSupportedABI(t *testing.T) {
	status, err := determineLandlockStatus("required", false, func() (int, error) {
		return minimumLandlockABI, nil
	})
	if err != nil || status.Effective != "enabled" || status.ABI != minimumLandlockABI {
		t.Fatalf("required supported status = %#v, %v", status, err)
	}
}

func TestPrepareLandlockWritePathsDoesNotStatSyntheticPaths(t *testing.T) {
	hostPath := t.TempDir()
	syntheticPath := filepath.Join(t.TempDir(), "created-inside-sandbox")
	paths, err := prepareLandlockWritePaths([]string{hostPath}, []string{syntheticPath, hostPath, syntheticPath})
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 || paths[0] != hostPath || paths[1] != syntheticPath {
		t.Fatalf("prepared Landlock paths = %#v", paths)
	}
	if _, err := os.Stat(syntheticPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("synthetic test path unexpectedly exists: %v", err)
	}
}

func TestPrepareLandlockWritePathsRequiresHostPaths(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing-host-path")
	if _, err := prepareLandlockWritePaths([]string{missing}, nil); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing host path error = %v, want ENOENT", err)
	}
}
