//go:build linux

package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"
)

// Dry-run plans describe bootstrap paths without allocating a store.
const podmanBootstrapPlaceholder = "<podman-bootstrap>"

func podmanBootstrapEnvironment(environment map[string]string, root string) map[string]string {
	result := cloneMap(environment)
	for _, name := range []string{
		"CONTAINER_HOST", "CONTAINER_CONNECTION", "DOCKER_HOST", "CONTAINERS_CONF_OVERRIDE",
		"CONTAINERS_CONF_MODULES", "STORAGE_DRIVER", "STORAGE_OPTS", "_CONTAINERS_USERNS_CONFIGURED",
		"CONTAINERS_GRAPHROOT", "CONTAINERS_RUNROOT",
	} {
		delete(result, name)
	}
	for name, relative := range podmanBootstrapPaths() {
		result[name] = filepath.Join(root, relative)
	}
	result["PODMAN_NO_PAUSE_PROCESS"] = "1"
	return result
}

func podmanBootstrapPaths() map[string]string {
	return map[string]string{
		"HOME": "home", "XDG_CONFIG_HOME": "config", "XDG_DATA_HOME": "data",
		"XDG_CACHE_HOME": "cache", "XDG_STATE_HOME": "state", "XDG_RUNTIME_DIR": "run",
		"TMPDIR": "tmp", "CONTAINERS_CONF": "containers.conf", "CONTAINERS_STORAGE_CONF": "storage.conf",
	}
}

// The bootstrap store only supplies a full rootless user namespace. It never
// opens the instance's container database, which belongs to the sandbox.
func createPodmanBootstrap(environment map[string]string, identity instanceIdentity) (root string, launchEnv map[string]string, err error) {
	base, err := filepath.Abs(os.TempDir())
	if err != nil {
		return "", nil, fmt.Errorf("resolve Podman bootstrap temporary directory: %w", err)
	}
	// A sandbox-writable parent could retarget cleanup; even a read-only bind
	// must not expose the bootstrap store as a second Podman configuration.
	candidates, err := pathResolutionCandidates(base)
	if err != nil {
		return "", nil, fmt.Errorf("resolve Podman bootstrap temporary directory: %w", err)
	}
	protected := []string{identity.Project, identity.State, identity.GitCommon}
	protected = append(protected, identity.RWBind...)
	protected = append(protected, identity.ROBind...)
	for _, candidate := range candidates {
		for _, path := range protected {
			if path != "" && pathWithin(path, candidate) {
				return "", nil, fmt.Errorf("Podman bootstrap temporary directory %s is inside sandbox-visible path %s; set host TMPDIR to a directory outside sandbox mounts", base, path)
			}
		}
	}
	root, err = os.MkdirTemp(base, "bwrap-agent-podman-")
	if err != nil {
		return "", nil, fmt.Errorf("create Podman bootstrap directory: %w", err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, removePodmanBootstrap(root))
		}
	}()
	if err = preparePodmanBootstrap(root); err != nil {
		return root, nil, err
	}
	return root, podmanBootstrapEnvironment(environment, root), nil
}

// preparePodmanBootstrap populates a caller-owned disposable directory.
// Allocation, placement validation, and cleanup remain with the caller.
func preparePodmanBootstrap(root string) error {
	for _, relative := range []string{"home", "config", "data", "cache", "state", "run", "tmp", "storage", "run/containers", "run/libpod/tmp"} {
		if _, err := ensureStateDirectory(root, relative, 0o700); err != nil {
			return fmt.Errorf("prepare Podman bootstrap: %w", err)
		}
	}
	configs := map[string]any{
		"storage.conf": map[string]any{"storage": map[string]any{
			"driver": "vfs", "graphroot": filepath.Join(root, "storage"), "runroot": filepath.Join(root, "run/containers"),
		}},
		"containers.conf": map[string]any{"engine": map[string]any{
			"cgroup_manager": "cgroupfs", "events_logger": "file", "remote": false,
			"tmp_dir":    filepath.Join(root, "run/libpod/tmp"),
			"static_dir": filepath.Join(root, "storage/libpod"), "volume_path": filepath.Join(root, "storage/volumes"),
		}},
	}
	for name, config := range configs {
		content, encodeErr := toml.Marshal(config)
		if encodeErr != nil {
			return fmt.Errorf("encode Podman bootstrap configuration: %w", encodeErr)
		}
		if _, err := writeStateFile(root, name, content, 0o600); err != nil {
			return fmt.Errorf("write Podman bootstrap configuration: %w", err)
		}
	}
	return nil
}

func removePodmanBootstrap(root string) error {
	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("remove Podman bootstrap directory %s: %w", root, err)
	}
	return nil
}
