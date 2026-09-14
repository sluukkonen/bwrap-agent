//go:build linux

package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"
)

func writeStorageConfig(state string) (string, error) {
	if _, err := ensureStateDirectory(state, "podman/config", 0o700); err != nil {
		return "", err
	}
	graphRoot, err := ensureStateDirectory(state, "podman/storage", 0o700)
	if err != nil {
		return "", err
	}
	runRoot := filepath.Join(sandboxRuntimeDirectory, "containers")
	graphJSON, _ := json.Marshal(graphRoot)
	runJSON, _ := json.Marshal(runRoot)
	content := fmt.Sprintf("[storage]\ndriver = \"overlay\"\ngraphroot = %s\nrunroot = %s\n\n[storage.options.overlay]\nignore_chown_errors = \"false\"\n", graphJSON, runJSON)
	return writeStateFile(state, "podman/config/storage.conf", []byte(content), 0o600)
}

// preparePodmanConfigMount keeps user configuration read-only while allowing
// Podman to load its own configuration files and drop-ins normally.
func preparePodmanConfigMount(context hostContext) (resourceMount, error) {
	configHome, err := os.UserConfigDir()
	if err != nil {
		return resourceMount{}, fmt.Errorf("locate Podman user configuration: %w", err)
	}
	source, found, err := context.sources.resolveSource(filepath.Join(configHome, "containers"), true, false)
	if err != nil {
		return resourceMount{}, fmt.Errorf("resolve Podman user configuration: %w", err)
	}
	if !found {
		// Hide legacy generated configuration even on reused instances.
		source, err = ensureStateDirectory(context.state, "podman/config/empty-user-config", 0o700)
		if err != nil {
			return resourceMount{}, err
		}
	}
	mount := resourceMount{Source: source, Destination: filepath.Join(context.state, "config", "containers")}
	if err := validateStateMountpoint(context.state, mount.Destination, mount.Source); err != nil {
		return resourceMount{}, err
	}
	return mount, nil
}

func writeContainersConfig(state string, privateNetwork, disableLabeling bool) (string, error) {
	// Empty paths reset inherited host values and let Podman derive them from
	// its effective store. In particular, older storage libraries use the
	// instance XDG data directory even with an explicit graphroot setting.
	engine := map[string]any{
		"cgroup_manager": "cgroupfs", "events_logger": "file",
		"remote": false, "active_service": "",
		"static_dir": "", "volume_path": "",
	}
	engine["tmp_dir"] = filepath.Join(sandboxRuntimeDirectory, "libpod/tmp")
	network := map[string]any{"network_config_dir": ""}
	containers := map[string]any{}
	if disableLabeling {
		// The outer sandbox remains confined; its private overlay cannot be
		// relabeled by nested SELinux containers.
		containers["label"] = false
	}
	if privateNetwork {
		containers["base_hosts_file"] = "none"
		containers["http_proxy"] = false
		proxy := fmt.Sprintf("http://%s:%d", proxyContainerHostname, proxyContainerPort)
		containers["env"] = []any{
			"HTTP_PROXY=" + proxy, "HTTPS_PROXY=" + proxy,
			"http_proxy=" + proxy, "https_proxy=" + proxy,
			"NO_PROXY=localhost,127.0.0.1,::1", "no_proxy=localhost,127.0.0.1,::1",
			map[string]bool{"append": true},
		}
		network["pasta_options"] = []any{
			"--map-host-loopback", proxyContainerAddress, map[string]bool{"append": true},
		}
	}
	content, err := toml.Marshal(map[string]any{
		"containers": containers, "network": network, "engine": engine,
	})
	if err != nil {
		return "", fmt.Errorf("encode Podman sandbox configuration: %w", err)
	}
	return writeStateFile(state, "podman/config/containers.conf", content, 0o600)
}
