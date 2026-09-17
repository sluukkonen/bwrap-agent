//go:build linux

package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func TestPodmanConfigSource(t *testing.T) {
	for _, scenario := range []string{"xdg", "home", "missing", "legacy", "symlink", "dangling", "file", "project", "unreadable", "destination-symlink"} {
		t.Run(scenario, func(t *testing.T) {
			root, context := testHostContext(t)
			home := t.TempDir()
			t.Setenv("HOME", home)
			config := filepath.Join(home, ".config")
			t.Setenv("XDG_CONFIG_HOME", config)
			if scenario == "home" {
				t.Setenv("XDG_CONFIG_HOME", "")
			}
			source := filepath.Join(config, "containers")
			if err := os.MkdirAll(config, 0o700); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "symlink", "project", "dangling":
				target := t.TempDir()
				if scenario == "project" {
					target = filepath.Join(root, "project")
				}
				if scenario == "dangling" {
					target = filepath.Join(target, "missing")
				}
				if err := os.Symlink(target, source); err != nil {
					t.Fatal(err)
				}
			case "file":
				if err := os.WriteFile(source, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			case "missing", "legacy":
			default:
				if err := os.Mkdir(source, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "legacy" {
				if _, err := writeStateFile(context.state, "config/containers/containers.conf", []byte("invalid legacy config"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "unreadable" {
				if os.Geteuid() == 0 {
					t.Skip("requires an unprivileged user")
				}
				if err := os.Chmod(config, 0); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(config, 0o700) })
			}
			if scenario == "destination-symlink" {
				if err := os.Mkdir(filepath.Join(context.state, "config"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(t.TempDir(), filepath.Join(context.state, "config", "containers")); err != nil {
					t.Fatal(err)
				}
			}
			mount, err := preparePodmanConfigMount(context)
			wantError := scenario == "unreadable" || scenario == "file" || scenario == "project" || scenario == "dangling" || scenario == "destination-symlink"
			if wantError {
				if err == nil {
					t.Fatalf("unsafe source/destination accepted: %#v", mount)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			expected := source
			if scenario == "missing" || scenario == "legacy" {
				expected = filepath.Join(context.state, "podman", "config", "empty-user-config")
			}
			expected, err = filepath.EvalSymlinks(expected)
			if err != nil {
				t.Fatal(err)
			}
			if mount.Source != expected || mount.Destination != filepath.Join(context.state, "config", "containers") {
				t.Fatalf("unexpected config mount: %#v", mount)
			}
		})
	}
}

func TestPodmanConfigPlan(t *testing.T) {
	config := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", config)
	source := filepath.Join(config, "containers")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	hostConfig := []byte("[network]\npasta_options = ['--ipv4-only']\n")
	if err := os.WriteFile(filepath.Join(source, "containers.conf"), hostConfig, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, network := range []string{"private", "host", "none"} {
		t.Run(network, func(t *testing.T) {
			t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
			plan, err := BuildPlan(Options{Project: ".", Instance: "config", Network: network, Podman: "on", TTY: "never", Command: []string{"/bin/true"},
				Env:      []string{"CONTAINERS_CONF=/bad", "CONTAINERS_CONF_OVERRIDE=/bad", "CONTAINERS_STORAGE_CONF=/bad"},
				UnsetEnv: []string{"CONTAINERS_STORAGE_CONF", "CONTAINERS_CONF_OVERRIDE"}})
			if err != nil {
				t.Fatal(err)
			}
			generated := filepath.Join(plan.State, "podman", "config", "containers.conf")
			storage := filepath.Join(plan.State, "podman", "config", "storage.conf")
			if plan.LaunchEnv["CONTAINERS_CONF"] != filepath.Join(podmanBootstrapPlaceholder, "containers.conf") || plan.LaunchEnv["CONTAINERS_STORAGE_CONF"] != filepath.Join(podmanBootstrapPlaceholder, "storage.conf") {
				t.Fatalf("supervisor config not isolated: %#v", plan.LaunchEnv)
			}
			if _, exists := plan.LaunchEnv["CONTAINERS_CONF_OVERRIDE"]; exists {
				t.Fatal("supervisor loads override twice")
			}
			joined := strings.Join(plan.Bwrap, "\x00")
			resolved, err := filepath.EvalSymlinks(source)
			if err != nil {
				t.Fatal(err)
			}
			for _, expected := range []string{
				"--ro-bind\x00" + resolved + "\x00" + filepath.Join(plan.State, "config", "containers"),
				"--ro-bind\x00" + resolved + "\x00" + filepath.Join(plan.State, "home", ".config", "containers"),
				"--unsetenv\x00CONTAINERS_CONF",
				"--setenv\x00CONTAINERS_CONF_OVERRIDE\x00" + generated,
				"--setenv\x00CONTAINERS_STORAGE_CONF\x00" + storage,
			} {
				if !strings.Contains(joined, expected) {
					t.Fatalf("sandbox missing %q", expected)
				}
			}
			if strings.Index(joined, "--bind\x00"+plan.State) > strings.Index(joined, "--ro-bind\x00"+resolved) {
				t.Fatal("state hides config mount")
			}
			data, err := os.ReadFile(generated)
			if err != nil {
				t.Fatal(err)
			}
			var conf map[string]any
			if err := toml.Unmarshal(data, &conf); err != nil {
				t.Fatal(err)
			}
			netConfig := conf["network"].(map[string]any)
			if network == "private" {
				options := netConfig["pasta_options"].([]any)
				if len(options) != 3 || options[2].(map[string]any)["append"] != true {
					t.Fatalf("pasta options replace host settings: %s", data)
				}
			} else if _, exists := netConfig["pasta_options"]; exists {
				t.Fatalf("unexpected pasta override: %s", data)
			}
			engine := conf["engine"].(map[string]any)
			if engine["remote"] != false || engine["active_service"] != "" {
				t.Fatalf("remote host defaults retained: %s", data)
			}
			if engine["static_dir"] != "" || engine["volume_path"] != "" || netConfig["network_config_dir"] != "" {
				t.Fatalf("host paths are not reset to store-derived defaults: %s", data)
			}
			if engine["tmp_dir"] != filepath.Join(sandboxRuntimeDirectory, "libpod/tmp") {
				t.Fatalf("temporary directory is not sandbox-private: %s", data)
			}
		})
	}
	t.Run("disabled", func(t *testing.T) {
		t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
		plan, err := BuildPlan(Options{Project: ".", Instance: "off", Network: "host", Podman: "off", TTY: "never", Command: []string{"/bin/true"}})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(strings.Join(plan.Bwrap, "\x00"), source) {
			t.Fatal("host config exposed with Podman disabled")
		}
	})
	got, err := os.ReadFile(filepath.Join(source, "containers.conf"))
	if err != nil || string(got) != string(hostConfig) {
		t.Fatalf("host config changed: %q, %v", got, err)
	}
}

func TestStorageConfigKeepsRootlessBackingPath(t *testing.T) {
	state := t.TempDir()
	path, err := writeStorageConfig(state)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Storage struct {
			GraphRoot           string `toml:"graphroot"`
			RootlessStoragePath string `toml:"rootless_storage_path"`
		} `toml:"storage"`
	}
	if err := toml.Unmarshal(content, &config); err != nil {
		t.Fatal(err)
	}
	if config.Storage.GraphRoot != filepath.Join(state, "podman", "storage") || config.Storage.RootlessStoragePath != filepath.Join(state, "data", "containers", "storage") {
		t.Fatalf("storage paths do not preserve existing backing directories: %s", content)
	}
}
