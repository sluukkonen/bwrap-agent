//go:build linux

package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"
)

const sandboxPodmanConfigDirectory = "/run/bwrap-agent/podman-config"

// User mounts must not replace any part of the instance-owned container store.
func validatePodmanStorageMounts(layout homeLayout, identity instanceIdentity) error {
	storage := layout.destination(filepath.Join(layout.state, podmanStorageRelative))
	paths := append([]string{identity.Project, identity.GitCommon}, identity.ROBind...)
	paths = append(paths, identity.RWBind...)
	for _, destination := range paths {
		if destination == "" {
			continue
		}
		if pathWithin(layout.home, destination) {
			relative, _ := filepath.Rel(layout.home, destination)
			destination = filepath.Join(layout.canonicalHome, relative)
		}
		if pathsOverlap(destination, storage) {
			return fmt.Errorf("mount %s overlaps private Podman storage %s; select a non-overlapping mount or disable Podman", destination, storage)
		}
	}
	return nil
}

func writeStorageConfig(generated string, layout homeLayout) (string, error) {
	backing, err := ensureStateDirectory(layout.state, podmanStorageRelative, 0o700)
	if err != nil {
		return "", err
	}
	graphJSON, _ := json.Marshal(layout.destination(backing))
	runJSON, _ := json.Marshal(filepath.Join(sandboxRuntimeDirectory, "containers"))
	content := fmt.Sprintf("[storage]\ndriver = \"overlay\"\ngraphroot = %s\nrootless_storage_path = %s\nrunroot = %s\n\n[storage.options.overlay]\nignore_chown_errors = \"false\"\n", graphJSON, graphJSON, runJSON)
	return writeStateFile(generated, "podman/storage.conf", []byte(content), 0o444)
}

// preparePodmanConfigMount keeps user configuration read-only while allowing
// Podman to load its own configuration files and drop-ins normally.
func preparePodmanConfigMount(context hostContext, generated string) (resourceMount, error) {
	configHome, err := os.UserConfigDir()
	if err != nil {
		return resourceMount{}, fmt.Errorf("locate Podman user configuration: %w", err)
	}
	source, found, err := context.sources.resolveSource(filepath.Join(configHome, "containers"), true, false)
	if err != nil {
		return resourceMount{}, fmt.Errorf("resolve Podman user configuration: %w", err)
	}
	if !found {
		// Keep user configuration read-only even when there is no host source.
		source, err = ensureStateDirectory(generated, "podman/empty-user-config", 0o700)
		if err != nil {
			return resourceMount{}, err
		}
	}
	mount := resourceMount{Source: source, Destination: filepath.Join(context.state, "home", ".config", "containers")}
	if err := validateStateMountpoint(context.state, mount.Destination, mount.Source); err != nil {
		return resourceMount{}, err
	}
	return mount, nil
}

func writeContainersConfig(generated string, privateNetwork, disableLabeling bool) (string, error) {
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
	return writeStateFile(generated, "podman/containers.conf", content, 0o444)
}
