//go:build linux

package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

var terminalReset = []byte("\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1005l\x1b[?1006l\x1b[?1015l\x1b[?1004l\x1b[?2004l\x1b[?1049l\x1b[?25h\x1b[0m")

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

func runWithPTY(argv []string, environment map[string]string, input, output *os.File) int {
	signals := make(chan os.Signal, 8)
	signal.Notify(signals, syscall.SIGWINCH, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT)
	defer signal.Stop(signals)
	return runWithPTYSignals(argv, environment, input, output, signals)
}

func runWithPTYSignals(argv []string, environment map[string]string, input, output *os.File, signals <-chan os.Signal) int {
	interactive := isTerminal(input.Fd())
	terminal := input
	if !interactive {
		terminal = output
	}
	master, slave, err := pty.Open()
	if err != nil {
		fmt.Fprintf(os.Stderr, "bwrap-agent: failed to allocate PTY: %v\n", err)
		return 126
	}
	defer master.Close()

	var saved *unix.Termios
	if interactive {
		saved, err = unix.IoctlGetTermios(int(input.Fd()), unix.TCGETS)
		if err != nil {
			slave.Close()
			return 126
		}
		if err := setTermios(int(slave.Fd()), saved); err != nil {
			slave.Close()
			return 126
		}
	} else if err := noEcho(int(slave.Fd())); err != nil {
		slave.Close()
		return 126
	}
	if isTerminal(terminal.Fd()) {
		copyWindowSize(int(terminal.Fd()), int(master.Fd()))
	}
	if interactive {
		if _, err := makeRaw(int(input.Fd())); err != nil {
			slave.Close()
			return 126
		}
		defer func() { _ = setTermios(int(input.Fd()), saved) }()
	}
	if isTerminal(output.Fd()) {
		defer func() { _ = writeAll(int(output.Fd()), terminalReset) }()
	}

	command := exec.Command(argv[0], argv[1:]...)
	command.Env = environmentList(environment)
	command.Stdin, command.Stdout, command.Stderr = slave, slave, slave
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := command.Start(); err != nil {
		slave.Close()
		fmt.Fprintf(os.Stderr, "bwrap-agent: failed to launch: %v\n", err)
		return launchErrorCode(err)
	}
	slave.Close()

	doneSignals := make(chan struct{})
	go func() {
		for {
			select {
			case received := <-signals:
				sig := received.(syscall.Signal)
				if sig == syscall.SIGWINCH {
					copyWindowSize(int(terminal.Fd()), int(master.Fd()))
				} else {
					_ = syscall.Kill(-command.Process.Pid, sig)
				}
			case <-doneSignals:
				return
			}
		}
	}()
	relayErr := relayPTY(int(input.Fd()), int(output.Fd()), int(master.Fd()), interactive)
	if relayErr != nil {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGTERM)
	}
	waitErr := command.Wait()
	close(doneSignals)
	if relayErr != nil && !errors.Is(relayErr, unix.EIO) {
		fmt.Fprintf(os.Stderr, "bwrap-agent: PTY relay failed: %v\n", relayErr)
		return 126
	}
	return exitStatus(waitErr)
}

type placeholderIdentity struct {
	Path   string          `json:"path"`
	Kind   controlPathKind `json:"kind"`
	Device uint64          `json:"device"`
	Inode  uint64          `json:"inode"`
}

type launchControlFiles struct {
	root       string
	lifetimeFD int
	once       sync.Once
}

func createControlPlaceholder(cleanup controlCleanup) (placeholderIdentity, error) {
	if cleanup.kind == controlDirectory {
		if err := os.Mkdir(cleanup.path, 0o700); err != nil {
			return placeholderIdentity{}, err
		}
	} else {
		fd, err := unix.Open(cleanup.path, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		if err != nil {
			return placeholderIdentity{}, err
		}
		file := os.NewFile(uintptr(fd), cleanup.path)
		if cleanup.kind == controlJSONFile {
			if _, err := file.Write([]byte("{}\n")); err != nil {
				_ = file.Close()
				_ = os.Remove(cleanup.path)
				return placeholderIdentity{}, err
			}
		}
		if err := file.Close(); err != nil {
			_ = os.Remove(cleanup.path)
			return placeholderIdentity{}, err
		}
	}
	info, err := os.Lstat(cleanup.path)
	if err != nil {
		return placeholderIdentity{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return placeholderIdentity{}, fmt.Errorf("could not identify control placeholder %s", cleanup.path)
	}
	return placeholderIdentity{Path: cleanup.path, Kind: cleanup.kind, Device: uint64(stat.Dev), Inode: stat.Ino}, nil
}

func removeControlPlaceholder(placeholder placeholderIdentity) error {
	info, err := os.Lstat(placeholder.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || uint64(stat.Dev) != placeholder.Device || stat.Ino != placeholder.Inode {
		return fmt.Errorf("control placeholder changed before cleanup: %s", placeholder.Path)
	}
	if (placeholder.Kind == controlDirectory) != info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("control placeholder type changed before cleanup: %s", placeholder.Path)
	}
	return os.Remove(placeholder.Path)
}

func controlCoordinationRoot(project string) (string, error) {
	base, err := stateBase()
	if err != nil {
		return "", err
	}
	base, err = resolveState(base)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(project))
	return filepath.Join(base, "control-projects", hex.EncodeToString(digest[:16])), nil
}

func readControlRegistry(root string) ([]placeholderIdentity, error) {
	content, err := readSmallRegularFile(filepath.Join(root, "placeholders.json"), 1024*1024)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var placeholders []placeholderIdentity
	if err := json.Unmarshal([]byte(content), &placeholders); err != nil {
		return nil, err
	}
	return placeholders, nil
}

func writeControlRegistry(root string, placeholders []placeholderIdentity) error {
	content, err := json.Marshal(placeholders)
	if err != nil {
		return err
	}
	_, err = writeStateFile(root, "placeholders.json", append(content, '\n'), 0o600)
	return err
}

func placeholderMatches(placeholder placeholderIdentity) bool {
	info, err := os.Lstat(placeholder.Path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || (placeholder.Kind == controlDirectory) != info.IsDir() {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && uint64(stat.Dev) == placeholder.Device && stat.Ino == placeholder.Inode
}

func newLaunchControlFiles(project string) (*launchControlFiles, error) {
	root, err := controlCoordinationRoot(project)
	if err != nil {
		return nil, err
	}
	if err := secureMkdir(root, 0o700); err != nil {
		return nil, err
	}
	fd, err := unix.Open(filepath.Join(root, "lifetime.lock"), unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(fd, unix.LOCK_SH); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	return &launchControlFiles{root: root, lifetimeFD: fd}, nil
}

func (control *launchControlFiles) prepare(cleanup []controlCleanup) error {
	mutation, err := acquireDirectoryLock(control.root, false)
	if err != nil {
		return err
	}
	defer mutation.Close()
	registry, err := readControlRegistry(control.root)
	if err != nil {
		return err
	}
	valid := registry[:0]
	for _, placeholder := range registry {
		if placeholderMatches(placeholder) {
			valid = append(valid, placeholder)
		}
	}
	registry = valid
	byPath := make(map[string]placeholderIdentity, len(registry))
	for _, placeholder := range registry {
		byPath[placeholder.Path] = placeholder
	}
	for _, candidate := range cleanup {
		if existing, found := byPath[candidate.path]; found {
			if existing.Kind != candidate.kind {
				return fmt.Errorf("managed control placeholder has unexpected type: %s", candidate.path)
			}
			continue
		}
		if _, err := os.Lstat(candidate.path); err == nil {
			return fmt.Errorf("control path appeared during launch planning: %s", candidate.path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	var created []placeholderIdentity
	rollback := func() {
		for index := len(created) - 1; index >= 0; index-- {
			_ = removeControlPlaceholder(created[index])
		}
	}
	for _, candidate := range cleanup {
		if _, found := byPath[candidate.path]; found {
			continue
		}
		placeholder, err := createControlPlaceholder(candidate)
		if err != nil {
			rollback()
			return fmt.Errorf("create temporary control placeholder %s: %w", candidate.path, err)
		}
		registry = append(registry, placeholder)
		created = append(created, placeholder)
	}
	if err := writeControlRegistry(control.root, registry); err != nil {
		rollback()
		return err
	}
	return nil
}

func (control *launchControlFiles) Close() error {
	if control == nil {
		return nil
	}
	var result error
	control.once.Do(func() {
		mutation, err := acquireDirectoryLock(control.root, false)
		if err != nil {
			result = err
			_ = unix.Flock(control.lifetimeFD, unix.LOCK_UN)
			_ = unix.Close(control.lifetimeFD)
			control.lifetimeFD = -1
			return
		}
		defer mutation.Close()
		_ = unix.Flock(control.lifetimeFD, unix.LOCK_UN)
		if err := unix.Flock(control.lifetimeFD, unix.LOCK_EX|unix.LOCK_NB); err != nil {
			_ = unix.Close(control.lifetimeFD)
			control.lifetimeFD = -1
			if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
				result = err
			}
			return
		}
		registry, err := readControlRegistry(control.root)
		if err != nil {
			result = err
		} else {
			sort.Slice(registry, func(i, j int) bool { return len(registry[i].Path) > len(registry[j].Path) })
			remaining := registry[:0]
			for _, placeholder := range registry {
				if !placeholderMatches(placeholder) {
					continue
				}
				if err := removeControlPlaceholder(placeholder); err != nil {
					remaining = append(remaining, placeholder)
					if result == nil {
						result = err
					}
				}
			}
			if err := writeControlRegistry(control.root, remaining); err != nil && result == nil {
				result = err
			}
		}
		_ = unix.Flock(control.lifetimeFD, unix.LOCK_UN)
		_ = unix.Close(control.lifetimeFD)
		control.lifetimeFD = -1
	})
	return result
}

func relayPTY(input, output, master int, interactive bool) error {
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
				if writeErr := writeAll(output, buffer[:count]); writeErr != nil {
					return writeErr
				}
			}
			if count == 0 || errors.Is(err, unix.EIO) {
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

func sandboxInit(argv []string) int {
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "bwrap-agent: internal init received no command")
		return 126
	}
	podmanEnabled := os.Getenv("BWRAP_AGENT_PODMAN") == "1"
	var socket *podmanSocket
	var service *managedProcess
	if podmanEnabled {
		var err error
		socket, err = newPodmanSocket(filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "podman", "podman.sock"))
		if err != nil {
			fmt.Fprintf(os.Stderr, "bwrap-agent: Podman API socket failed to initialize: %v\n", err)
			return 70
		}
		defer socket.Close()
		_ = os.Setenv("DOCKER_HOST", "unix://"+socket.path)
		_ = os.Setenv("TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE", socket.path)
	}

	command := exec.Command(argv[0], argv[1:]...)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	command.Env = os.Environ()
	signals := make(chan os.Signal, 8)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	if err := command.Start(); err != nil {
		signal.Stop(signals)
		cleanupPodman(podmanEnabled, service)
		fmt.Fprintf(os.Stderr, "bwrap-agent: failed to launch: %v\n", err)
		return launchErrorCode(err)
	}
	commandDone := make(chan error, 1)
	go func() {
		commandDone <- command.Wait()
	}()
	var activation <-chan struct{}
	if socket != nil {
		activation = socket.activation
	}
	for {
		select {
		case received := <-signals:
			_ = command.Process.Signal(received)
		case <-activation:
			activation = nil
			var err error
			service, err = startPodmanService(socket.listener)
			if err != nil {
				fmt.Fprintf(os.Stderr, "bwrap-agent: Podman API service failed to activate: %v\n", err)
				socket.Close()
				socket = nil
				continue
			}
			socket.handoff()
		case err := <-commandDone:
			signal.Stop(signals)
			cleanupPodman(podmanEnabled, service)
			return exitStatus(err)
		}
	}
}

type podmanSocket struct {
	path       string
	listener   *os.File
	wakeRead   *os.File
	wakeWrite  *os.File
	activation chan struct{}
	watchDone  chan struct{}
}

func newPodmanSocket(path string) (*podmanSocket, error) {
	if path == "" || !filepath.IsAbs(path) {
		return nil, errors.New("Podman API socket path must be absolute")
	}
	directory := filepath.Dir(path)
	if err := secureMkdir(directory, 0o700); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 && info.Mode()&os.ModeSymlink == 0 {
			return nil, fmt.Errorf("refusing to replace non-socket path: %s", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	listener.SetUnlinkOnClose(false)
	if err := os.Chmod(path, 0o600); err != nil {
		listener.Close()
		os.Remove(path)
		return nil, err
	}
	listenerFile, err := listener.File()
	if err != nil {
		listener.Close()
		os.Remove(path)
		return nil, err
	}
	if err := listener.Close(); err != nil {
		listenerFile.Close()
		os.Remove(path)
		return nil, err
	}
	wakeRead, wakeWrite, err := os.Pipe()
	if err != nil {
		listenerFile.Close()
		os.Remove(path)
		return nil, err
	}
	socket := &podmanSocket{
		path: path, listener: listenerFile, wakeRead: wakeRead, wakeWrite: wakeWrite,
		activation: make(chan struct{}), watchDone: make(chan struct{}),
	}
	go socket.watch()
	return socket, nil
}

func (socket *podmanSocket) watch() {
	defer close(socket.watchDone)
	poll := []unix.PollFd{
		{Fd: int32(socket.listener.Fd()), Events: unix.POLLIN},
		{Fd: int32(socket.wakeRead.Fd()), Events: unix.POLLIN},
	}
	for {
		if _, err := unix.Poll(poll, -1); err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return
		}
		if poll[1].Revents != 0 {
			return
		}
		if poll[0].Revents&(unix.POLLIN|unix.POLLHUP|unix.POLLERR) != 0 {
			close(socket.activation)
			return
		}
	}
}

func (socket *podmanSocket) stopWatcher() {
	if socket == nil || socket.wakeWrite == nil {
		return
	}
	select {
	case <-socket.watchDone:
	default:
		_, _ = socket.wakeWrite.Write([]byte{1})
		<-socket.watchDone
	}
	socket.wakeRead.Close()
	socket.wakeWrite.Close()
	socket.wakeRead, socket.wakeWrite = nil, nil
}

func (socket *podmanSocket) handoff() {
	socket.stopWatcher()
	if socket.listener != nil {
		socket.listener.Close()
		socket.listener = nil
	}
}

func (socket *podmanSocket) Close() error {
	if socket == nil {
		return nil
	}
	socket.handoff()
	err := os.Remove(socket.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func startPodmanService(listener *os.File) (*managedProcess, error) {
	if listener == nil {
		return nil, errors.New("Podman API listener is closed")
	}
	command := exec.Command("/run/bwrap-agent/init", internalPodmanServiceMode)
	command.ExtraFiles = []*os.File{listener}
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	if err := command.Start(); err != nil {
		return nil, err
	}
	service := &managedProcess{command: command, done: make(chan struct{})}
	go func() {
		_ = command.Wait()
		close(service.done)
	}()
	return service, nil
}

func podmanServiceExec() int {
	podman, err := exec.LookPath("podman")
	if err != nil {
		fmt.Fprintf(os.Stderr, "bwrap-agent: Podman API service executable not found: %v\n", err)
		return 70
	}
	_ = os.Setenv("LISTEN_PID", fmt.Sprintf("%d", os.Getpid()))
	_ = os.Setenv("LISTEN_FDS", "1")
	_ = os.Setenv("LISTEN_FDNAMES", "podman.socket")
	environment := withoutEnvironment(os.Environ(), "CONTAINER_HOST", "CONTAINER_CONNECTION")
	if err := unix.Exec(podman, []string{"podman", "system", "service", "--time=0"}, environment); err != nil {
		fmt.Fprintf(os.Stderr, "bwrap-agent: Podman API service failed to exec: %v\n", err)
		return 70
	}
	return 0
}

type managedProcess struct {
	command *exec.Cmd
	done    chan struct{}
}

func cleanupPodman(enabled bool, service *managedProcess) {
	if enabled {
		stop := exec.Command("podman", "stop", "--all", "--ignore", "--time", "3")
		stop.Stdout, stop.Stderr = io.Discard, io.Discard
		stop.Env = withoutEnvironment(os.Environ(), "CONTAINER_HOST", "CONTAINER_CONNECTION")
		_ = stop.Run()
	}
	stopService(service)
}

func stopService(service *managedProcess) {
	if service == nil || service.command.Process == nil {
		return
	}
	_ = service.command.Process.Signal(syscall.SIGTERM)
	select {
	case <-service.done:
	case <-time.After(2 * time.Second):
		_ = service.command.Process.Kill()
		<-service.done
	}
}
