//go:build linux

package app

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

func testTerminal(t *testing.T) (*os.File, *os.File, *unix.Termios) {
	t.Helper()
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = slave.Close(); _ = master.Close() })
	saved, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	return master, slave, saved
}

func assertTerminalRestored(t *testing.T, terminal *os.File, saved *unix.Termios) {
	t.Helper()
	got, err := unix.IoctlGetTermios(int(terminal.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, saved) {
		t.Fatalf("terminal settings changed: got %#v, want %#v", got, saved)
	}
}

func TestPTYRestoresTerminalAfterExecution(t *testing.T) {
	for _, test := range []struct {
		name   string
		argv   []string
		status int
	}{
		{"child exit", []string{"/bin/sh", "-c", "exit 7"}, 7},
		{"launch failure", []string{"/nonexistent/bwrap-agent-test-command"}, 127},
	} {
		t.Run(test.name, func(t *testing.T) {
			master, terminal, saved := testTerminal(t)
			status := runWithPTYSignals(test.argv, nil, terminal, terminal, make(chan os.Signal))
			if status != test.status {
				t.Fatalf("status = %d, want %d", status, test.status)
			}
			assertTerminalRestored(t, terminal, saved)
			poll := []unix.PollFd{{Fd: int32(master.Fd()), Events: unix.POLLIN}}
			if n, err := unix.Poll(poll, 1000); err != nil || n != 1 {
				t.Fatalf("missing terminal reset: n=%d err=%v", n, err)
			}
			data := make([]byte, 1024)
			n, err := unix.Read(int(master.Fd()), data)
			if err != nil || !bytes.Equal(data[:n], terminalReset) {
				t.Fatalf("terminal output = %q, err=%v", data[:max(n, 0)], err)
			}
		})
	}
}

func TestPTYSessionPartialPreparationAndRepeatedClose(t *testing.T) {
	for _, mode := range []string{"allocated", "failed", "prepared"} {
		t.Run(mode, func(t *testing.T) {
			_, terminal, saved := testTerminal(t)
			session, err := newPTYSession(terminal, terminal)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			master, slave := session.master, session.slave
			if mode == "failed" {
				_ = slave.Close()
				if err := session.prepare(); err == nil {
					t.Fatal("preparation accepted a closed slave")
				}
				if session.saved != nil || session.resetOutput {
					t.Fatal("failed setup armed terminal restoration")
				}
			} else if mode == "prepared" {
				if err := session.prepare(); err != nil {
					t.Fatal(err)
				}
				raw, err := unix.IoctlGetTermios(int(terminal.Fd()), unix.TCGETS)
				if err != nil {
					t.Fatal(err)
				}
				if raw.Lflag&(unix.ICANON|unix.ECHO|unix.ISIG) != 0 {
					t.Fatal("input was not made raw")
				}
			}
			err = session.Close()
			if mode == "failed" {
				if !errors.Is(err, os.ErrClosed) {
					t.Fatalf("close error = %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if err := session.Close(); err != nil {
				t.Fatal(err)
			}
			assertTerminalRestored(t, terminal, saved)
			for _, file := range []*os.File{master, slave} {
				if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
					t.Fatalf("PTY descriptor remains open: %v", err)
				}
			}
		})
	}
}

func TestPTYSessionForwardsSignalsAndJoins(t *testing.T) {
	_, terminal, _ := testTerminal(t)
	session, err := newPTYSession(terminal, terminal)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	initial := &unix.Winsize{Row: 24, Col: 80}
	if err := unix.IoctlSetWinsize(int(terminal.Fd()), unix.TIOCSWINSZ, initial); err != nil {
		t.Fatal(err)
	}
	if err := session.prepare(); err != nil {
		t.Fatal(err)
	}
	got, err := unix.IoctlGetWinsize(int(session.master.Fd()), unix.TIOCGWINSZ)
	if err != nil || *got != *initial {
		t.Fatalf("initial window size = %v, err=%v", got, err)
	}

	command := exec.Command("/bin/sleep", "30")
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	commandDone := make(chan struct{})
	var waitErr error
	go func() { waitErr = command.Wait(); close(commandDone) }()
	defer func() { _ = command.Process.Kill(); <-commandDone }()
	signals := make(chan os.Signal, 2)
	session.forwardSignals(command.Process.Pid, signals)
	forwardingDone := session.signalDone
	resized := &unix.Winsize{Row: 40, Col: 120}
	if err := unix.IoctlSetWinsize(int(terminal.Fd()), unix.TIOCSWINSZ, resized); err != nil {
		t.Fatal(err)
	}
	signals <- syscall.SIGWINCH
	deadline := time.Now().Add(time.Second)
	for {
		got, err := unix.IoctlGetWinsize(int(session.master.Fd()), unix.TIOCGWINSZ)
		if err != nil {
			t.Fatal(err)
		}
		if *got == *resized {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("resize not forwarded: %v", got)
		}
		time.Sleep(time.Millisecond)
	}
	signals <- syscall.SIGTERM
	select {
	case <-commandDone:
		if status := exitStatus(waitErr); status != 128+int(syscall.SIGTERM) {
			t.Fatalf("child status = %d", status)
		}
	case <-time.After(time.Second):
		t.Fatal("termination not forwarded")
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-forwardingDone:
	default:
		t.Fatal("Close returned before signal forwarding finished")
	}
	// Closing the borrowed channel remains the caller's responsibility.
	close(signals)
}
