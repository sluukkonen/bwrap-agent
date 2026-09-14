//go:build linux

package app

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func testLaunchSession(t *testing.T) *launchSession {
	t.Helper()
	root := t.TempDir()
	identity := instanceIdentity{Instance: "launch-test", Root: root, State: filepath.Join(root, "state"), Project: t.TempDir()}
	lock, err := acquireInstanceLock(identity)
	if err != nil {
		t.Fatal(err)
	}
	session := &launchSession{identity: identity, lock: lock, signals: make(chan os.Signal, 8)}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func assertLaunchLock(t *testing.T, identity instanceIdentity, held bool) {
	t.Helper()
	lock, err := acquireInstanceLock(identity)
	if lock != nil {
		_ = lock.Close()
	}
	if held && !errors.Is(err, errInstanceBusy) {
		t.Fatalf("expected busy lock, got %v", err)
	}
	if !held && err != nil {
		t.Fatalf("lock was not released: %v", err)
	}
}

func assertLaunchProxyClosed(t *testing.T, proxyPort, dnsPort int) {
	t.Helper()
	for _, port := range []int{proxyPort, dnsPort} {
		connection, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
		if err == nil {
			connection.Close()
			t.Fatalf("listener on %d survived cleanup", port)
		}
	}
	listener, err := net.ListenPacket("udp", net.JoinHostPort("127.0.0.1", strconv.Itoa(dnsPort)))
	if err != nil {
		t.Fatalf("DNS UDP listener survived cleanup: %v", err)
	}
	listener.Close()
}

func TestLaunchSessionRuntimeLifecycle(t *testing.T) {
	for _, scenario := range []string{"success", "child failure", "launch failure", "pending signal", "invalid policy", "invalid runtime arguments", "bootstrap failure"} {
		t.Run(scenario, func(t *testing.T) {
			temporary := t.TempDir()
			t.Setenv("TMPDIR", temporary)
			session := testLaunchSession(t)
			marker := filepath.Join(t.TempDir(), "started")
			plan := LaunchPlan{LaunchEnv: map[string]string{"BWRAP_AGENT_PODMAN": "1", "HOME": podmanBootstrapPlaceholder + "/home"}, Bwrap: []string{"/bin/sh", "-c", "printf started >\"$1\"; exit \"$2\"", "sh", marker, "0"}}
			want := 0
			switch scenario {
			case "child failure":
				plan.Bwrap[len(plan.Bwrap)-1] = "17"
				want = 17
			case "launch failure":
				plan.Bwrap = []string{filepath.Join(t.TempDir(), "missing")}
				want = 127
			case "pending signal":
				session.signals <- syscall.SIGTERM
				want = 143
			case "invalid policy":
				plan.ProxyGuestPort = 65532
				plan.NetworkAllow = []string{"invalid"}
				want = 2
			case "invalid runtime arguments":
				plan.ProxyGuestPort = 65532
				want = 126
			case "bootstrap failure":
				t.Setenv("TMPDIR", filepath.Join(temporary, "missing"))
				want = 126
			}
			originalEnvironment := cloneMap(plan.LaunchEnv)
			originalArguments := append([]string(nil), plan.Bwrap...)
			if status := session.run(plan); status != want {
				t.Fatalf("status = %d, want %d", status, want)
			}
			assertLaunchLock(t, session.identity, true)
			if !reflect.DeepEqual(plan.LaunchEnv, originalEnvironment) || !reflect.DeepEqual(plan.Bwrap, originalArguments) {
				t.Fatal("runtime preparation modified the plan")
			}
			if _, err := os.Stat(marker); (err == nil) != (scenario == "success" || scenario == "child failure") {
				t.Fatalf("child execution mismatch: %v", err)
			}
			if scenario != "bootstrap failure" {
				if _, err := os.Stat(session.bootstrap); err != nil {
					t.Fatalf("bootstrap disappeared before cleanup: %v", err)
				}
			}
			if scenario == "invalid runtime arguments" && session.proxy == nil {
				t.Fatal("runtime argument failure did not reach proxy startup")
			}
			var proxyPort, dnsPort int
			if session.proxy != nil {
				proxyPort, dnsPort = session.proxy.port(), session.proxy.dnsPort()
			}
			if status := session.finish(want); status != want {
				t.Fatalf("cleanup changed status to %d", status)
			}
			assertLaunchLock(t, session.identity, false)
			entries, err := os.ReadDir(temporary)
			if err != nil || len(entries) != 0 {
				t.Fatalf("bootstrap storage survived cleanup: %v, %v", entries, err)
			}
			if proxyPort != 0 {
				assertLaunchProxyClosed(t, proxyPort, dnsPort)
			}
			if err := session.Close(); err != nil {
				t.Fatalf("repeated cleanup: %v", err)
			}
		})
	}
}

func TestLaunchSessionCleanupFailure(t *testing.T) {
	for _, status := range []int{0, 17, 143} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			session := testLaunchSession(t)
			policy, err := parseNetworkPolicy(nil)
			if err != nil {
				t.Fatal(err)
			}
			session.proxy, err = startNetworkProxy(policy)
			if err != nil {
				t.Fatal(err)
			}
			proxyPort, dnsPort := session.proxy.port(), session.proxy.dnsPort()
			// An invalid ancestor forces RemoveAll to fail without permission tricks.
			file := filepath.Join(t.TempDir(), "file")
			if err := os.WriteFile(file, nil, 0600); err != nil {
				t.Fatal(err)
			}
			session.bootstrap = filepath.Join(file, "bootstrap")
			want := status
			if status == 0 {
				want = 126
			}
			if got := session.finish(status); got != want {
				t.Fatalf("cleanup status = %d, want %d", got, want)
			}
			assertLaunchLock(t, session.identity, false)
			assertLaunchProxyClosed(t, proxyPort, dnsPort)
			if err := session.Close(); err != nil {
				t.Fatalf("repeated cleanup: %v", err)
			}
		})
	}
}

func TestRunLaunchPlanningLifecycle(t *testing.T) {
	for _, scenario := range []string{"dry run", "plan failure"} {
		t.Run(scenario, func(t *testing.T) {
			t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
			temporary := t.TempDir()
			t.Setenv("TMPDIR", temporary)
			bin := filepath.Dir(fakeBubblewrap(t, "0.12.0", 0755))
			for _, name := range []string{"podman", "pasta"} {
				if err := os.Symlink("/bin/true", filepath.Join(bin, name)); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
			output, err := os.CreateTemp(t.TempDir(), "plan")
			if err != nil {
				t.Fatal(err)
			}
			defer output.Close()
			originalOutput := os.Stdout
			os.Stdout = output
			defer func() { os.Stdout = originalOutput }()
			opts := Options{Project: t.TempDir(), Instance: "planning", Network: "private", Podman: "on", TTY: "never", DryRun: true, Command: []string{"/bin/true"}}
			want := 0
			if scenario == "plan failure" {
				opts.WorkspaceMode = "invalid"
				opts.DryRun = false
				want = 2
			}
			if status := runLaunch(opts); status != want {
				t.Fatalf("status = %d, want %d", status, want)
			}
			identity, lock, err := resolveAndLockInstance(opts)
			if err != nil {
				t.Fatalf("planning retained lock: %v", err)
			}
			lock.Close()
			metadata, err := readInstanceMetadata(identity.Root)
			if err != nil || metadata.LastUsedAt.IsZero() {
				t.Fatalf("missing last-use update: %#v, %v", metadata, err)
			}
			entries, err := os.ReadDir(temporary)
			if err != nil || len(entries) != 0 {
				t.Fatalf("planning allocated runtime storage: %v, %v", entries, err)
			}
			if scenario == "dry run" {
				if _, err := output.Seek(0, 0); err != nil {
					t.Fatal(err)
				}
				var plan struct {
					Environment    map[string]string `json:"environment"`
					ProxyGuestPort int               `json:"proxy_guest_port"`
				}
				if err := json.NewDecoder(output).Decode(&plan); err != nil {
					t.Fatal(err)
				}
				if plan.Environment["HOME"] != podmanBootstrapPlaceholder+"/home" || plan.ProxyGuestPort == 0 {
					t.Fatalf("dry-run runtime description changed: %#v", plan)
				}
			}
		})
	}
}
