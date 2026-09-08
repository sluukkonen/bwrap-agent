//go:build linux

package app

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func parseIdentityMappings(content string) ([]syscall.SysProcIDMap, error) {
	var mappings []syscall.SysProcIDMap
	coveredRoot := false
	previousEnd := uint64(0)
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 3 {
			return nil, fmt.Errorf("invalid ID map line %q", scanner.Text())
		}
		inner, err := strconv.ParseUint(fields[0], 10, 32)
		if err != nil {
			return nil, fmt.Errorf("invalid inner ID in %q", scanner.Text())
		}
		if _, err := strconv.ParseUint(fields[1], 10, 32); err != nil {
			return nil, fmt.Errorf("invalid outer ID in %q", scanner.Text())
		}
		size, err := strconv.ParseUint(fields[2], 10, 32)
		if err != nil || size == 0 {
			return nil, fmt.Errorf("invalid ID count in %q", scanner.Text())
		}
		end := inner + size
		if end > uint64(^uint32(0))+1 || len(mappings) > 0 && inner < previousEnd {
			return nil, fmt.Errorf("overlapping or overflowing ID map line %q", scanner.Text())
		}
		maxInt := uint64(^uint(0) >> 1)
		if inner > maxInt || size > maxInt {
			return nil, errors.New("ID map exceeds platform integer range")
		}
		mappings = append(mappings, syscall.SysProcIDMap{
			ContainerID: int(inner),
			HostID:      int(inner),
			Size:        int(size),
		})
		if inner == 0 {
			coveredRoot = true
		}
		previousEnd = end
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(mappings) == 0 || !coveredRoot {
		return nil, errors.New("ID map does not cover namespace root")
	}
	return mappings, nil
}

func readIdentityMappings(path string) ([]syscall.SysProcIDMap, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseIdentityMappings(string(content))
}

func lockedSandboxCommand(argv []string) (*exec.Cmd, error) {
	uidMappings, err := readIdentityMappings("/proc/self/uid_map")
	if err != nil {
		return nil, fmt.Errorf("read current UID map: %w", err)
	}
	gidMappings, err := readIdentityMappings("/proc/self/gid_map")
	if err != nil {
		return nil, fmt.Errorf("read current GID map: %w", err)
	}
	return newLockedSandboxCommand(argv, uidMappings, gidMappings), nil
}

func newLockedSandboxCommand(argv []string, uidMappings, gidMappings []syscall.SysProcIDMap) *exec.Cmd {
	arguments := append([]string{internalRuntimeMode}, argv...)
	command := exec.Command("/run/bwrap-agent/init", arguments...)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	command.Env = os.Environ()
	command.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:                 unix.CLONE_NEWUSER | unix.CLONE_NEWNS,
		UidMappings:                uidMappings,
		GidMappings:                gidMappings,
		GidMappingsEnableSetgroups: false,
		Pdeathsig:                  syscall.SIGKILL,
	}
	return command
}

func dropCurrentThreadCapabilities() error {
	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	data := [2]unix.CapUserData{}
	return unix.Capset(&header, &data[0])
}

func runLockedSandbox(argv []string) int {
	command, err := lockedSandboxCommand(argv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bwrap-agent: prepare Podman mount-lock namespace: %v\n", err)
		return 70
	}
	runtime.LockOSThread()
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		fmt.Fprintf(os.Stderr, "bwrap-agent: protect Podman namespace supervisor: %v\n", err)
		return 70
	}
	if err := command.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "bwrap-agent: create Podman mount-lock namespace: %v\n", err)
		return 70
	}
	if err := dropCurrentThreadCapabilities(); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		fmt.Fprintf(os.Stderr, "bwrap-agent: drop namespace supervisor capabilities: %v\n", err)
		return 70
	}

	signals := make(chan os.Signal, 8)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	for {
		select {
		case received := <-signals:
			_ = command.Process.Signal(received)
		case err := <-done:
			return exitStatus(err)
		}
	}
}

func makeMountNamespacePrivate() error {
	return unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, "")
}
