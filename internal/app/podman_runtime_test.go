//go:build linux

package app

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestPodmanRuntimeLifecycle(t *testing.T) {
	for _, test := range []struct {
		name     string
		activate bool
		fail     bool
		finish   bool
		want     []string
	}{
		{name: "early close"},
		{name: "inactive finish", finish: true, want: []string{"containers"}},
		{name: "active finish", activate: true, finish: true, want: []string{"start", "containers", "service"}},
		{name: "failed activation", activate: true, fail: true, finish: true, want: []string{"start", "containers"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "podman.sock")
			runtime, err := newPodmanRuntime(path)
			if err != nil {
				t.Fatal(err)
			}
			defer runtime.Close()
			socket := runtime.socket
			var events []string
			var inherited net.Listener
			sentinel := errors.New("service start failed")
			service := &managedProcess{}
			runtime.startService = func(listener *os.File) (*managedProcess, error) {
				events = append(events, "start")
				if test.fail {
					return nil, sentinel
				}
				var err error
				// Duplicate the descriptor as the child would, so handoff must
				// close only the parent's copy and preserve queued connections.
				inherited, err = net.FileListener(listener)
				if err != nil {
					return nil, err
				}
				return service, nil
			}
			runtime.stopContainers = func() {
				events = append(events, "containers")
				if !test.fail {
					if _, err := os.Stat(path); err != nil {
						t.Errorf("API socket removed before container cleanup: %v", err)
					}
				}
			}
			runtime.stopService = func(got *managedProcess) {
				events = append(events, "service")
				if got != service {
					t.Error("wrong service stopped")
				}
				if _, err := os.Stat(path); err != nil {
					t.Errorf("API socket removed before service shutdown: %v", err)
				}
				_ = inherited.Close()
			}
			select {
			case <-runtime.Activation():
				t.Fatal("activated without a connection")
			default:
			}
			if test.activate {
				connection, err := net.Dial("unix", path)
				if err != nil {
					t.Fatal(err)
				}
				defer connection.Close()
				select {
				case <-runtime.Activation():
				case <-time.After(time.Second):
					t.Fatal("connection did not trigger activation")
				}
				err = runtime.Activate()
				if test.fail {
					if !errors.Is(err, sentinel) {
						t.Fatalf("activation error = %v", err)
					}
					if _, err := os.Lstat(path); !os.IsNotExist(err) {
						t.Fatalf("failed activation retained socket: %v", err)
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					listener := inherited.(*net.UnixListener)
					_ = listener.SetDeadline(time.Now().Add(time.Second))
					accepted, err := listener.Accept()
					if err != nil {
						t.Fatalf("handoff lost queued connection: %v", err)
					}
					_ = accepted.Close()
				}
				if runtime.Activation() != nil {
					t.Fatal("activation remains enabled")
				}
				if err := runtime.Activate(); err != nil {
					t.Fatalf("repeated activation: %v", err)
				}
			}
			if test.finish {
				if err := runtime.Finish(); err != nil {
					t.Fatal(err)
				}
				if err := runtime.Finish(); err != nil {
					t.Fatal(err)
				}
			}
			if err := runtime.Close(); err != nil {
				t.Fatal(err)
			}
			if err := runtime.Close(); err != nil {
				t.Fatal(err)
			}
			if err := runtime.Activate(); err != nil {
				t.Fatal(err)
			}
			if runtime.Activation() != nil {
				t.Fatal("closed runtime still exposes activation")
			}
			if !reflect.DeepEqual(events, test.want) {
				t.Fatalf("lifecycle events = %v, want %v", events, test.want)
			}
			select {
			case <-socket.watchDone:
			default:
				t.Fatal("socket watcher still running")
			}
			if socket.listener != nil || socket.wakeRead != nil || socket.wakeWrite != nil {
				t.Fatal("socket descriptors retained")
			}
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Fatalf("socket remains after cleanup: %v", err)
			}
		})
	}
}

func TestDisabledPodmanRuntime(t *testing.T) {
	var runtime *podmanRuntime
	if runtime.Activation() != nil {
		t.Fatal("disabled runtime exposes activation")
	}
	if err := runtime.Activate(); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Finish(); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
}
