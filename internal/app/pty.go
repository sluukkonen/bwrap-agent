//go:build linux

package app

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// ptySession owns PTY descriptors and terminal restoration, but borrows the
// caller's input, output, and signal channel. Only the launch goroutine calls
// its methods; Close joins signal forwarding before releasing descriptors.
type ptySession struct {
	master, slave           *os.File
	input, output, terminal *os.File
	interactive             bool
	saved                   *unix.Termios
	resetOutput             bool
	signalStop              chan struct{}
	signalDone              chan struct{}
}

func newPTYSession(input, output *os.File) (*ptySession, error) {
	interactive := isTerminal(input.Fd())
	terminal := input
	if !interactive {
		terminal = output
	}
	master, slave, err := pty.Open()
	if err != nil {
		return nil, err
	}
	return &ptySession{master: master, slave: slave, input: input, output: output, terminal: terminal, interactive: interactive}, nil
}

func (session *ptySession) prepare() error {
	var saved *unix.Termios
	if session.interactive {
		var err error
		saved, err = unix.IoctlGetTermios(int(session.input.Fd()), unix.TCGETS)
		if err != nil {
			return err
		}
		if err := setTermios(int(session.slave.Fd()), saved); err != nil {
			return err
		}
	} else if err := noEcho(int(session.slave.Fd())); err != nil {
		return err
	}
	if isTerminal(session.terminal.Fd()) {
		copyWindowSize(int(session.terminal.Fd()), int(session.master.Fd()))
	}
	if session.interactive {
		if _, err := makeRaw(int(session.input.Fd())); err != nil {
			return err
		}
		session.saved = saved
	}
	session.resetOutput = isTerminal(session.output.Fd())
	return nil
}

func (session *ptySession) closeSlave() error {
	if session.slave == nil {
		return nil
	}
	err := session.slave.Close()
	session.slave = nil
	return err
}

func (session *ptySession) forwardSignals(pid int, signals <-chan os.Signal) {
	session.signalStop = make(chan struct{})
	session.signalDone = make(chan struct{})
	go func() {
		defer close(session.signalDone)
		for {
			select {
			case received, ok := <-signals:
				if !ok {
					return
				}
				sig := received.(syscall.Signal)
				if sig == syscall.SIGWINCH {
					copyWindowSize(int(session.terminal.Fd()), int(session.master.Fd()))
				} else {
					_ = syscall.Kill(-pid, sig)
				}
			case <-session.signalStop:
				return
			}
		}
	}()
}

func (session *ptySession) stopSignals() {
	if session.signalStop != nil {
		close(session.signalStop)
		<-session.signalDone
		session.signalStop, session.signalDone = nil, nil
	}
}

func (session *ptySession) Close() error {
	if session == nil {
		return nil
	}
	session.stopSignals()
	err := session.closeSlave()
	if session.resetOutput {
		err = errors.Join(err, writeAll(int(session.output.Fd()), terminalReset))
		session.resetOutput = false
	}
	if session.saved != nil {
		err = errors.Join(err, setTermios(int(session.input.Fd()), session.saved))
		session.saved = nil
	}
	if session.master != nil {
		err = errors.Join(err, session.master.Close())
		session.master = nil
	}
	return err
}

var terminalReset = []byte("\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1005l\x1b[?1006l\x1b[?1015l\x1b[?1004l\x1b[?2004l\x1b[?1049l\x1b[?25h\x1b[0m")

func isTerminal(fd uintptr) bool {
	_, err := unix.IoctlGetTermios(int(fd), unix.TCGETS)
	return err == nil
}

func makeRaw(fd int) (*unix.Termios, error) {
	old, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return nil, err
	}
	raw := *old
	raw.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	raw.Oflag &^= unix.OPOST
	raw.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	raw.Cflag &^= unix.CSIZE | unix.PARENB
	raw.Cflag |= unix.CS8
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &raw); err != nil {
		return nil, err
	}
	return old, nil
}

func setTermios(fd int, value *unix.Termios) error {
	return unix.IoctlSetTermios(fd, unix.TCSETS, value)
}

func noEcho(fd int) error {
	attributes, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return err
	}
	attributes.Lflag &^= unix.ECHO | unix.ECHONL
	return unix.IoctlSetTermios(fd, unix.TCSETS, attributes)
}

func copyWindowSize(source, destination int) {
	size, err := unix.IoctlGetWinsize(source, unix.TIOCGWINSZ)
	if err == nil {
		_ = unix.IoctlSetWinsize(destination, unix.TIOCSWINSZ, size)
	}
}

func writeAll(fd int, data []byte) error {
	for len(data) > 0 {
		written, err := unix.Write(fd, data)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		data = data[written:]
	}
	return nil
}

func runWithPTY(argv []string, environment map[string]string, input, output *os.File) int {
	signals := make(chan os.Signal, 8)
	signal.Notify(signals, syscall.SIGWINCH, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT)
	defer signal.Stop(signals)
	return runWithPTYSignals(argv, environment, input, output, signals)
}

func runWithPTYSignals(argv []string, environment map[string]string, input, output *os.File, signals <-chan os.Signal) int {
	return runWithPTYClipboard(argv, environment, input, output, signals, nil)
}

func runWithPTYClipboard(argv []string, environment map[string]string, input, output *os.File, signals <-chan os.Signal, clipboard *clipboardBridgeConfig) int {
	session, err := newPTYSession(input, output)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bwrap-agent: failed to allocate PTY: %v\n", err)
		return 126
	}
	defer session.Close()
	if err := session.prepare(); err != nil {
		return 126
	}

	command := exec.Command(argv[0], argv[1:]...)
	command.Env = environmentList(environment)
	command.Stdin, command.Stdout, command.Stderr = session.slave, session.slave, session.slave
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := command.Start(); err != nil {
		session.closeSlave()
		fmt.Fprintf(os.Stderr, "bwrap-agent: failed to launch: %v\n", err)
		return launchErrorCode(err)
	}
	session.closeSlave()

	session.forwardSignals(command.Process.Pid, signals)
	var parser *clipboardParser
	var worker *clipboardWorker
	if clipboard != nil {
		worker = newClipboardWorker(clipboard.copy, func() { fmt.Fprintln(os.Stderr, "bwrap-agent: warning: clipboard write failed or timed out") })
		parser = &clipboardParser{copy: worker.submit}
	}
	relayErr := relayPTYClipboard(int(input.Fd()), int(output.Fd()), int(session.master.Fd()), session.interactive, parser)
	if relayErr != nil {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGTERM)
	}
	if worker != nil {
		worker.close()
	}
	waitErr := command.Wait()
	session.stopSignals()
	if relayErr != nil && !errors.Is(relayErr, unix.EIO) {
		fmt.Fprintf(os.Stderr, "bwrap-agent: PTY relay failed: %v\n", relayErr)
		return 126
	}
	return exitStatus(waitErr)
}

func relayPTY(input, output, master int, interactive bool) error {
	return relayPTYClipboard(input, output, master, interactive, nil)
}

func relayPTYClipboard(input, output, master int, interactive bool, parser *clipboardParser) error {
	pollFDs := []unix.PollFd{{Fd: int32(master), Events: unix.POLLIN}, {Fd: int32(input), Events: unix.POLLIN}}
	buffer := make([]byte, 65536)
	inputOpen := true
	for {
		if _, err := unix.Poll(pollFDs, -1); err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return err
		}
		if pollFDs[0].Revents&(unix.POLLIN|unix.POLLHUP|unix.POLLERR) != 0 {
			count, err := unix.Read(master, buffer)
			if count > 0 {
				data := buffer[:count]
				if parser != nil {
					data = parser.feed(data)
				}
				if writeErr := writeAll(output, data); writeErr != nil {
					return writeErr
				}
			}
			if count == 0 || errors.Is(err, unix.EIO) {
				if parser != nil {
					return writeAll(output, parser.finish())
				}
				return nil
			}
			if err != nil && !errors.Is(err, unix.EINTR) {
				return err
			}
		}
		if inputOpen && pollFDs[1].Revents&(unix.POLLIN|unix.POLLHUP|unix.POLLERR) != 0 {
			count, err := unix.Read(input, buffer)
			if count > 0 {
				if writeErr := writeAll(master, buffer[:count]); writeErr != nil {
					return writeErr
				}
			}
			if count == 0 {
				inputOpen = false
				pollFDs[1].Fd = -1
				if !interactive {
					if err := writeAll(master, []byte{4, 4}); err != nil {
						return err
					}
				}
			} else if err != nil && !errors.Is(err, unix.EINTR) {
				return err
			}
		}
	}
}
