//go:build linux

package app

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func TestPodmanBootstrapIsolation(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	environment := map[string]string{
		"HOME": "/persistent/home", "XDG_DATA_HOME": "/persistent/data",
		"CONTAINERS_CONF": "/host/config", "CONTAINERS_STORAGE_CONF": "/host/storage",
		"CONTAINERS_CONF_OVERRIDE": "/host/override", "CONTAINERS_CONF_MODULES": "host-module",
		"CONTAINER_HOST": "unix:///host.sock", "CONTAINER_CONNECTION": "host", "DOCKER_HOST": "unix:///docker.sock",
		"STORAGE_DRIVER": "overlay", "STORAGE_OPTS": "host-option", "_CONTAINERS_USERNS_CONFIGURED": "done",
		"CONTAINERS_GRAPHROOT": "/host/graph", "CONTAINERS_RUNROOT": "/host/run",
		"USER": "account", "LOGNAME": "account", "PODMAN_NO_PAUSE_PROCESS": "0",
		"UNRELATED": podmanBootstrapPlaceholder + "/literal",
	}
	first, launchEnv, err := createPodmanBootstrap(environment, instanceIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = removePodmanBootstrap(first) })
	second, _, err := createPodmanBootstrap(environment, instanceIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = removePodmanBootstrap(second) })
	if first == second || !filepath.IsAbs(first) {
		t.Fatalf("bootstrap paths must be unique and absolute: %q, %q", first, second)
	}
	info, err := os.Stat(first)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("bootstrap permissions: %v, %v", info, err)
	}
	for _, name := range []string{"CONTAINERS_CONF_OVERRIDE", "CONTAINERS_CONF_MODULES", "CONTAINER_HOST", "CONTAINER_CONNECTION", "DOCKER_HOST", "STORAGE_DRIVER", "STORAGE_OPTS", "_CONTAINERS_USERNS_CONFIGURED", "CONTAINERS_GRAPHROOT", "CONTAINERS_RUNROOT"} {
		if _, found := launchEnv[name]; found {
			t.Errorf("bootstrap inherited %s", name)
		}
	}
	for name := range podmanBootstrapPaths() {
		if !pathWithin(first, launchEnv[name]) {
			t.Errorf("bootstrap %s escapes temporary store: %s", name, launchEnv[name])
		}
	}
	if launchEnv["USER"] != "account" || launchEnv["LOGNAME"] != "account" || launchEnv["PODMAN_NO_PAUSE_PROCESS"] != "1" || launchEnv["UNRELATED"] != environment["UNRELATED"] {
		t.Fatalf("bootstrap environment lost identity or changed unrelated values: %#v", launchEnv)
	}
	if environment["HOME"] != "/persistent/home" || environment["CONTAINERS_CONF_OVERRIDE"] != "/host/override" {
		t.Fatal("bootstrap modified input environment")
	}
	for _, name := range []string{"storage.conf", "containers.conf"} {
		content, err := os.ReadFile(filepath.Join(first, name))
		if err != nil {
			t.Fatal(err)
		}
		var config map[string]any
		if err := toml.Unmarshal(content, &config); err != nil {
			t.Fatal(err)
		}
		if name == "storage.conf" {
			storage := config["storage"].(map[string]any)
			if storage["driver"] != "vfs" || !pathWithin(first, storage["graphroot"].(string)) || !pathWithin(first, storage["runroot"].(string)) {
				t.Fatalf("bootstrap store is not isolated: %s", content)
			}
		} else {
			engine := config["engine"].(map[string]any)
			for _, key := range []string{"tmp_dir", "static_dir", "volume_path"} {
				if !pathWithin(first, engine[key].(string)) {
					t.Errorf("bootstrap %s escapes temporary store", key)
				}
			}
		}
	}
	// Cleanup must not follow entries out of the disposable store.
	outside := t.TempDir()
	writeTestFile(t, filepath.Join(outside, "keep"), "retained")
	if err := os.Symlink(outside, filepath.Join(first, "outside")); err != nil {
		t.Fatal(err)
	}
	if err := removePodmanBootstrap(first); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(first); !os.IsNotExist(err) {
		t.Fatalf("bootstrap remains after cleanup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "keep")); err != nil {
		t.Fatalf("cleanup touched symlink target: %v", err)
	}
}

func TestPodmanBootstrapUnavailableTemporaryDirectory(t *testing.T) {
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))
	if _, _, err := createPodmanBootstrap(nil, instanceIdentity{}); err == nil {
		t.Fatal("bootstrap accepted an unavailable temporary directory")
	}
}

func TestPodmanBootstrapRejectsExposedTemporaryDirectory(t *testing.T) {
	for _, mode := range []string{"project", "state", "git", "rw", "ro", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			exposed := t.TempDir()
			base := filepath.Join(exposed, "tmp")
			if err := os.Mkdir(base, 0o700); err != nil {
				t.Fatal(err)
			}
			identity := instanceIdentity{}
			switch mode {
			case "project", "symlink":
				identity.Project = exposed
			case "state":
				identity.State = exposed
			case "git":
				identity.GitCommon = exposed
			case "rw":
				identity.RWBind = []string{exposed}
			case "ro":
				identity.ROBind = []string{exposed}
			}
			if mode == "symlink" {
				alias := filepath.Join(t.TempDir(), "alias")
				if err := os.Symlink(base, alias); err != nil {
					t.Fatal(err)
				}
				base = alias
			}
			t.Setenv("TMPDIR", base)
			if _, _, err := createPodmanBootstrap(nil, identity); err == nil {
				t.Fatal("bootstrap accepted sandbox-visible temporary directory")
			}
			entries, err := os.ReadDir(base)
			if err != nil || len(entries) != 0 {
				t.Fatalf("rejected bootstrap left files: %v, %v", entries, err)
			}
		})
	}
}

func TestPodmanBootstrapPlanBoundary(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	temporary := t.TempDir()
	t.Setenv("TMPDIR", temporary)
	plan, err := BuildPlan(Options{Project: t.TempDir(), Network: "host", Podman: "on", TTY: "never", DryRun: true,
		Command: []string{"/bin/true"}, Env: []string{"HOME=/custom/home", "TMPDIR=/custom/tmp", "CONTAINERS_GRAPHROOT=/bad", "CONTAINERS_RUNROOT=/bad"}})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(temporary)
	if err != nil || len(entries) != 0 {
		t.Fatalf("planning allocated bootstrap storage: %v, %v", entries, err)
	}
	if _, err := os.Lstat(filepath.Join(plan.State, "run")); !os.IsNotExist(err) {
		t.Fatalf("planning created persistent runtime state: %v", err)
	}
	var output bytes.Buffer
	if err := writePlanJSON(&output, plan); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "podman-bootstrap") {
		t.Fatal("dry run omitted bootstrap placeholder")
	}
	joined := strings.Join(plan.Bwrap, "\x00")
	for _, fragment := range []string{
		"--perms\x000700\x00--tmpfs\x00" + sandboxRuntimeDirectory,
		"--setenv\x00HOME\x00/custom/home", "--setenv\x00TMPDIR\x00/custom/tmp",
		"--unsetenv\x00CONTAINERS_GRAPHROOT", "--unsetenv\x00CONTAINERS_RUNROOT", "--unsetenv\x00CONTAINERS_CONF",
		"--setenv\x00CONTAINERS_STORAGE_CONF\x00" + filepath.Join(plan.State, "podman/config/storage.conf"),
	} {
		if !strings.Contains(joined, fragment) {
			t.Errorf("sandbox boundary missing %q", fragment)
		}
	}
	if strings.Contains(joined, podmanBootstrapPlaceholder) {
		t.Fatal("bootstrap paths entered sandbox arguments")
	}
}
