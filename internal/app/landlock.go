//go:build linux

package app

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

const minimumLandlockABI = 3

const landlockFileWriteAccess = uint64(
	unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
		unix.LANDLOCK_ACCESS_FS_TRUNCATE)

const landlockWriteAccess = uint64(
	landlockFileWriteAccess |
		unix.LANDLOCK_ACCESS_FS_REMOVE_DIR |
		unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
		unix.LANDLOCK_ACCESS_FS_MAKE_CHAR |
		unix.LANDLOCK_ACCESS_FS_MAKE_DIR |
		unix.LANDLOCK_ACCESS_FS_MAKE_REG |
		unix.LANDLOCK_ACCESS_FS_MAKE_SOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_FIFO |
		unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_SYM |
		unix.LANDLOCK_ACCESS_FS_REFER)

func queryLandlockABI() (int, error) {
	value, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if errno == 0 {
		return int(value), nil
	}
	if errors.Is(errno, unix.ENOSYS) || errors.Is(errno, unix.EOPNOTSUPP) || errors.Is(errno, unix.ENOMSG) {
		return 0, nil
	}
	return 0, errno
}

func applyLandlock(writePaths []string) error {
	runtime.LockOSThread()
	// Keep this goroutine on the restricted thread until command.Start has
	// forked the target. sandboxInit intentionally never unlocks it.
	attr := unix.LandlockRulesetAttr{Access_fs: landlockWriteAccess}
	ruleset, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET,
		uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr), 0)
	if errno != 0 {
		return fmt.Errorf("create Landlock ruleset: %w", errno)
	}
	defer unix.Close(int(ruleset))
	for _, path := range writePaths {
		fd, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
		if err != nil {
			return fmt.Errorf("open Landlock write path %s: %w", path, err)
		}
		var stat unix.Stat_t
		if err := unix.Fstat(fd, &stat); err != nil {
			_ = unix.Close(fd)
			return fmt.Errorf("inspect Landlock write path %s: %w", path, err)
		}
		allowedAccess := landlockFileWriteAccess
		if stat.Mode&unix.S_IFMT == unix.S_IFDIR {
			allowedAccess = landlockWriteAccess
		}
		rule := unix.LandlockPathBeneathAttr{Allowed_access: allowedAccess, Parent_fd: int32(fd)}
		_, _, errno = unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, ruleset,
			unix.LANDLOCK_RULE_PATH_BENEATH, uintptr(unsafe.Pointer(&rule)), 0, 0, 0)
		_ = unix.Close(fd)
		if errno != 0 {
			return fmt.Errorf("add Landlock write path %s: %w", path, errno)
		}
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("set no_new_privs for Landlock: %w", err)
	}
	_, _, errno = unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, ruleset, 0, 0)
	if errno != 0 {
		return fmt.Errorf("enforce Landlock ruleset: %w", errno)
	}
	return nil
}

func determineLandlockStatus(mode string, podmanEnabled bool, query func() (int, error)) (LandlockStatus, error) {
	status := LandlockStatus{Requested: mode}
	if mode == "off" {
		status.Effective = "off"
		return status, nil
	}
	if podmanEnabled {
		status.Effective = "skipped-podman"
		return status, nil
	}
	abi, err := query()
	if err != nil {
		if mode == "required" {
			return LandlockStatus{}, fmt.Errorf("query Landlock support: %w", err)
		}
		status.Effective = "unsupported"
		return status, nil
	}
	status.ABI = abi
	if abi >= minimumLandlockABI {
		status.Effective = "enabled"
		return status, nil
	}
	if mode == "required" {
		return LandlockStatus{}, fmt.Errorf("Landlock ABI %d is unavailable; ABI %d or newer is required", abi, minimumLandlockABI)
	}
	status.Effective = "unsupported"
	return status, nil
}

func existingLandlockPaths(paths []string) ([]string, error) {
	result := make([]string, 0, len(paths))
	seen := map[string]bool{}
	for _, path := range paths {
		if path == "" || seen[path] {
			continue
		}
		if _, err := os.Stat(path); err != nil {
			return nil, err
		}
		seen[path] = true
		result = append(result, path)
	}
	return result, nil
}

func prepareLandlockWritePaths(hostPaths, syntheticPaths []string) ([]string, error) {
	result, err := existingLandlockPaths(hostPaths)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(result)+len(syntheticPaths))
	for _, path := range result {
		seen[path] = true
	}
	for _, path := range syntheticPaths {
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		result = append(result, path)
	}
	return result, nil
}
