//go:build linux

package app

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// podmanRuntime owns sandbox-local API resources. Its methods run only on the
// supervisor goroutine; the socket watcher reports readiness through a channel.
type podmanRuntime struct {
	path           string
	socket         *podmanSocket
	service        *managedProcess
	activation     <-chan struct{}
	finished       bool
	startService   func(*os.File) (*managedProcess, error)
	stopContainers func()
	stopService    func(*managedProcess)
}

func newPodmanRuntime(path string) (*podmanRuntime, error) {
	socket, err := newPodmanSocket(path)
	if err != nil {
		return nil, err
	}
	return &podmanRuntime{
		path: socket.path, socket: socket, activation: socket.activation,
		startService: startPodmanService, stopContainers: stopPodmanContainers,
		stopService: stopService,
	}, nil
}

func (runtime *podmanRuntime) Activation() <-chan struct{} {
	if runtime == nil {
		return nil
	}
	return runtime.activation
}

func (runtime *podmanRuntime) Activate() error {
	if runtime == nil || runtime.activation == nil {
		return nil
	}
	// A failed start consumes activation too: the target continues without an API.
	runtime.activation = nil
	service, err := runtime.startService(runtime.socket.listener)
	if err != nil {
		_ = runtime.socket.Close()
		runtime.socket = nil
		return err
	}
	runtime.service = service
	runtime.socket.handoff()
	return nil
}

// Close releases API resources without stopping containers. This also handles
// failures before the target launch stage, when container cleanup is not armed.
func (runtime *podmanRuntime) Close() error {
	if runtime == nil {
		return nil
	}
	runtime.activation = nil
	if runtime.service != nil {
		runtime.stopService(runtime.service)
		runtime.service = nil
	}
	if runtime.socket != nil {
		err := runtime.socket.Close()
		runtime.socket = nil
		return err
	}
	return nil
}

// Finish stops instance containers even if the API was never activated. Keep
// the service and socket alive until container cleanup has completed.
func (runtime *podmanRuntime) Finish() error {
	if runtime == nil || runtime.finished {
		return nil
	}
	runtime.finished = true
	runtime.stopContainers()
	return runtime.Close()
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

// Internal Podman processes use the private runtime directory even when the
// target command overrides or unsets XDG_RUNTIME_DIR.
func sandboxPodmanEnvironment(environment []string) []string {
	environment = withoutEnvironment(environment, "CONTAINER_HOST", "CONTAINER_CONNECTION", "XDG_RUNTIME_DIR")
	return append(environment, "XDG_RUNTIME_DIR="+sandboxRuntimeDirectory)
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
	environment := sandboxPodmanEnvironment(os.Environ())
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

func stopPodmanContainers() {
	stop := exec.Command("podman", "stop", "--all", "--ignore", "--time", "3")
	stop.Stdout, stop.Stderr = io.Discard, io.Discard
	stop.Env = sandboxPodmanEnvironment(os.Environ())
	_ = stop.Run()
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
