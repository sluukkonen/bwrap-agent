//go:build linux

package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func environmentList(environment map[string]string) []string {
	result := make([]string, 0, len(environment))
	for name, value := range environment {
		result = append(result, name+"="+value)
	}
	return result
}

func withoutEnvironment(environment []string, names ...string) []string {
	removed := make(map[string]bool, len(names))
	for _, name := range names {
		removed[name] = true
	}
	result := make([]string, 0, len(environment))
	for _, assignment := range environment {
		name, _, found := strings.Cut(assignment, "=")
		if !found || !removed[name] {
			result = append(result, assignment)
		}
	}
	return result
}

func launchErrorCode(err error) int {
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ENOENT) {
		return 127
	}
	return 126
}

func runDirect(argv []string, environment map[string]string, input, output, errorOutput *os.File) int {
	signals := make(chan os.Signal, 8)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)
	return runDirectSignals(argv, environment, input, output, errorOutput, signals)
}

func runDirectSignals(argv []string, environment map[string]string, input, output, errorOutput *os.File, signals <-chan os.Signal) int {
	if len(argv) == 0 {
		fmt.Fprintln(errorOutput, "bwrap-agent: failed to launch: empty command")
		return 126
	}
	command := exec.Command(argv[0], argv[1:]...)
	command.Env = environmentList(environment)
	command.Stdin, command.Stdout, command.Stderr = input, output, errorOutput
	if err := command.Start(); err != nil {
		fmt.Fprintf(errorOutput, "bwrap-agent: failed to launch: %v\n", err)
		return launchErrorCode(err)
	}
	done := make(chan struct{})
	go func() {
		for {
			select {
			case received := <-signals:
				_ = command.Process.Signal(received)
			case <-done:
				return
			}
		}
	}()
	err := command.Wait()
	close(done)
	return exitStatus(err)
}

func exitStatus(err error) int {
	if err == nil {
		return 0
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		if status, ok := exitError.Sys().(syscall.WaitStatus); ok {
			if status.Signaled() {
				return 128 + int(status.Signal())
			}
			return status.ExitStatus()
		}
	}
	return 126
}

func sandboxInit(argv []string, podmanEnabled bool) int {
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "bwrap-agent: internal init received no command")
		return 126
	}
	if podmanEnabled {
		return runLockedSandbox(argv)
	}
	return sandboxRuntime(argv, false)
}

func sandboxRuntime(argv []string, podmanEnabled bool) int {
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "bwrap-agent: internal runtime received no command")
		return 126
	}
	if podmanEnabled {
		if err := makeMountNamespacePrivate(); err != nil {
			fmt.Fprintf(os.Stderr, "bwrap-agent: make Podman mount-lock namespace private: %v\n", err)
			return 70
		}
	}
	var landlockWrites []string
	if encoded, found := os.LookupEnv(internalLandlockEnvironment); found {
		_ = os.Unsetenv(internalLandlockEnvironment)
		if err := json.Unmarshal([]byte(encoded), &landlockWrites); err != nil {
			fmt.Fprintf(os.Stderr, "bwrap-agent: invalid internal Landlock policy: %v\n", err)
			return 70
		}
	}
	var podman *podmanRuntime
	if podmanEnabled {
		var err error
		podman, err = newPodmanRuntime(filepath.Join(sandboxRuntimeDirectory, "podman", "podman.sock"))
		if err != nil {
			fmt.Fprintf(os.Stderr, "bwrap-agent: Podman API socket failed to initialize: %v\n", err)
			return 70
		}
		defer podman.Close()
		_ = os.Setenv("DOCKER_HOST", "unix://"+podman.path)
		_ = os.Setenv("TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE", podman.path)
	}
	if len(landlockWrites) > 0 {
		if err := applyLandlock(landlockWrites); err != nil {
			fmt.Fprintf(os.Stderr, "bwrap-agent: Landlock enforcement failed: %v\n", err)
			return 70
		}
		executable, err := exec.LookPath(argv[0])
		if err != nil {
			fmt.Fprintf(os.Stderr, "bwrap-agent: failed to launch: %v\n", err)
			return launchErrorCode(err)
		}
		if err := unix.Exec(executable, argv, os.Environ()); err != nil {
			fmt.Fprintf(os.Stderr, "bwrap-agent: failed to launch: %v\n", err)
			return launchErrorCode(err)
		}
	}

	command := exec.Command(argv[0], argv[1:]...)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	command.Env = os.Environ()
	signals := make(chan os.Signal, 8)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer podman.Finish()
	defer signal.Stop(signals)
	if err := command.Start(); err != nil {
		signal.Stop(signals)
		_ = podman.Finish()
		fmt.Fprintf(os.Stderr, "bwrap-agent: failed to launch: %v\n", err)
		return launchErrorCode(err)
	}
	commandDone := make(chan error, 1)
	go func() {
		commandDone <- command.Wait()
	}()
	for {
		select {
		case received := <-signals:
			_ = command.Process.Signal(received)
		case <-podman.Activation():
			if err := podman.Activate(); err != nil {
				fmt.Fprintf(os.Stderr, "bwrap-agent: Podman API service failed to activate: %v\n", err)
			}
		case err := <-commandDone:
			return exitStatus(err)
		}
	}
}
