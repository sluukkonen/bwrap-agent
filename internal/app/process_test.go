//go:build linux

package app

import (
	"bytes"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func capturePTY(t *testing.T, argv []string, input *os.File) (int, []byte) {
	t.Helper()
	readOutput, writeOutput, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	status := runWithPTY(argv, map[string]string{"PATH": "/usr/bin:/bin"}, input, writeOutput)
	if err := writeOutput.Close(); err != nil {
		t.Fatal(err)
	}
	output, err := io.ReadAll(readOutput)
	if err != nil {
		t.Fatal(err)
	}
	readOutput.Close()
	return status, output
}

func TestPodmanSocketActivatesOnFirstConnection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "podman", "podman.sock")
	socket, err := newPodmanSocket(path)
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	if info, err := os.Stat(path); err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("socket was not created securely: info=%v err=%v", info, err)
	}
	select {
	case <-socket.activation:
		t.Fatal("socket activated without a connection")
	default:
	}
	connection, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	select {
	case <-socket.activation:
	case <-time.After(time.Second):
		t.Fatal("socket did not activate after a connection")
	}
}

func TestPodmanSocketClosesWithoutActivation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "podman", "podman.sock")
	socket, err := newPodmanSocket(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := socket.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("socket remains after close: %v", err)
	}
}

func TestPodmanSocketRefusesRegularFile(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "podman")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "podman.sock")
	if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newPodmanSocket(path); err == nil {
		t.Fatal("regular socket path was replaced")
	}
}

func TestPTYRedirectedInputReceivesEOF(t *testing.T) {
	input, err := os.CreateTemp(t.TempDir(), "input")
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	if _, err := input.WriteString("unterminated-input"); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	status, output := capturePTY(t, []string{"/bin/cat"}, input)
	if status != 0 {
		t.Fatalf("status = %d, output = %q", status, output)
	}
	if !bytes.Equal(output, []byte("unterminated-input")) {
		t.Fatalf("output = %q", output)
	}
}

func TestPTYDoesNotEchoUnconsumedTerminalReports(t *testing.T) {
	readInput, writeInput, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	report := []byte("\x1b[<35;40;12M\x1b[1;1R")
	if _, err := writeInput.Write(report); err != nil {
		t.Fatal(err)
	}
	writeInput.Close()
	defer readInput.Close()
	status, output := capturePTY(t, []string{"/bin/sleep", "0.05"}, readInput)
	if status != 0 {
		t.Fatalf("status = %d", status)
	}
	if bytes.Contains(output, report) || bytes.Contains(output, []byte("^[")) {
		t.Fatalf("terminal report was echoed: %q", output)
	}
}

func TestTerminalResetDisablesInteractiveModes(t *testing.T) {
	for _, mode := range []string{"1000", "1002", "1003", "1005", "1006", "1015", "1004", "2004", "1049"} {
		if !bytes.Contains(terminalReset, []byte("\x1b[?"+mode+"l")) {
			t.Errorf("terminal reset does not disable mode %s", mode)
		}
	}
}

func TestRunDirectReturnsChildStatusAndOutput(t *testing.T) {
	input, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	readOutput, writeOutput, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	status := runDirect([]string{"/bin/sh", "-c", "printf direct-output; exit 7"}, map[string]string{"PATH": "/usr/bin:/bin"}, input, writeOutput, writeOutput)
	writeOutput.Close()
	output, err := io.ReadAll(readOutput)
	readOutput.Close()
	if err != nil {
		t.Fatal(err)
	}
	if status != 7 || string(output) != "direct-output" {
		t.Fatalf("status = %d, output = %q", status, output)
	}
}

func TestSandboxPodmanEnvironment(t *testing.T) {
	for _, assignment := range []string{"", "XDG_RUNTIME_DIR=", "XDG_RUNTIME_DIR=relative", "XDG_RUNTIME_DIR=/other/runtime"} {
		t.Run(assignment, func(t *testing.T) {
			environment := []string{"HOME=/instance/home", "CONTAINER_HOST=tcp://other", "CONTAINER_CONNECTION=other", "LISTEN_FDS=1"}
			if assignment != "" {
				environment = append(environment, assignment, assignment)
			}
			before := append([]string{}, environment...)
			got := sandboxPodmanEnvironment(environment)
			if !reflect.DeepEqual(environment, before) {
				t.Fatal("target environment was modified")
			}
			count := 0
			for _, entry := range got {
				if strings.HasPrefix(entry, "XDG_RUNTIME_DIR=") {
					count++
					if entry != "XDG_RUNTIME_DIR="+sandboxRuntimeDirectory {
						t.Fatalf("wrong runtime: %s", entry)
					}
				}
				if strings.HasPrefix(entry, "CONTAINER_HOST=") || strings.HasPrefix(entry, "CONTAINER_CONNECTION=") {
					t.Fatalf("remote override retained: %s", entry)
				}
			}
			if count != 1 {
				t.Fatalf("got %d runtime assignments", count)
			}
			for _, key := range []string{"HOME", "LISTEN_FDS"} {
				if envValue(got, key, "") != envValue(before, key, "") {
					t.Fatalf("lost %s", key)
				}
			}
		})
	}
}
