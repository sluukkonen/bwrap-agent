package app

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

type bubblewrapCapabilities struct {
	BubblewrapStatus
	Path string
}

func inspectBubblewrap(path string) (bubblewrapCapabilities, error) {
	info, err := os.Stat(path)
	if err != nil {
		return bubblewrapCapabilities{}, err
	}
	if err := validateBubblewrapFileMode(path, info.Mode()); err != nil {
		return bubblewrapCapabilities{}, err
	}
	output, err := exec.Command(path, "--version").CombinedOutput()
	if err != nil {
		return bubblewrapCapabilities{}, fmt.Errorf("query Bubblewrap version: %w", err)
	}
	fields := strings.Fields(string(output))
	if len(fields) == 0 {
		return bubblewrapCapabilities{}, fmt.Errorf("could not parse Bubblewrap version from %q", strings.TrimSpace(string(output)))
	}
	version := fields[len(fields)-1]
	parts := strings.Split(version, ".")
	if len(parts) < 2 {
		return bubblewrapCapabilities{}, fmt.Errorf("could not parse Bubblewrap version %q", version)
	}
	major, majorErr := strconv.Atoi(parts[0])
	minor, minorErr := strconv.Atoi(parts[1])
	if majorErr != nil || minorErr != nil {
		return bubblewrapCapabilities{}, fmt.Errorf("could not parse Bubblewrap version %q", version)
	}
	if major == 0 && minor < 11 {
		return bubblewrapCapabilities{}, fmt.Errorf("Bubblewrap %s is unsupported; version 0.11 or newer is required", version)
	}
	help, err := exec.Command(path, "--help").CombinedOutput()
	if err != nil {
		return bubblewrapCapabilities{}, fmt.Errorf("query Bubblewrap capabilities: %w", err)
	}
	for _, option := range []string{"--overlay-src", "--tmp-overlay", "--remount-ro", "--disable-userns", "--ro-bind-data", "--perms", "--seccomp"} {
		if !strings.Contains(string(help), option) {
			return bubblewrapCapabilities{}, fmt.Errorf("Bubblewrap %s lacks required option %s", version, option)
		}
	}
	return bubblewrapCapabilities{
		BubblewrapStatus: BubblewrapStatus{Version: version, Legacy: major == 0 && minor == 11},
		Path:             path,
	}, nil
}

func validateBubblewrapFileMode(path string, mode os.FileMode) error {
	if mode&os.ModeSetuid != 0 {
		return fmt.Errorf("refusing setuid Bubblewrap executable: %s", path)
	}
	return nil
}
