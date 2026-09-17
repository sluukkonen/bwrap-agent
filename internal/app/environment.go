package app

import (
	"fmt"
	"path/filepath"
	"strings"
)

// environmentInputs contains resolved values; environment builders perform no
// host discovery and do not mutate their inputs.
type environmentInputs struct {
	home             homeLayout
	hostEnv          []string
	identity         instanceIdentity
	defaultAccount   string
	hostAccount      string
	podman           bool
	storageConfig    string
	containersConfig string
	agent            map[string]string
	command          map[string]string
}

func buildSandboxEnvironment(opts Options, input environmentInputs) (map[string]string, error) {
	state, instance := input.identity.State, input.identity.Instance
	workspaceMode := opts.WorkspaceMode
	environment := map[string]string{
		"HOME": input.home.home, "USER": envValue(input.hostEnv, "USER", input.defaultAccount),
		"LOGNAME":         envValue(input.hostEnv, "LOGNAME", envValue(input.hostEnv, "USER", input.defaultAccount)),
		"PATH":            "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"XDG_CONFIG_HOME": filepath.Join(input.home.home, ".config"), "XDG_CACHE_HOME": filepath.Join(input.home.home, ".cache"),
		"XDG_DATA_HOME": filepath.Join(input.home.home, ".local", "share"), "XDG_STATE_HOME": filepath.Join(input.home.home, ".local", "state"),
		"XDG_RUNTIME_DIR": sandboxRuntimeDirectory, "TMPDIR": filepath.Join(state, "tmp"),
		"BWRAP_AGENT_INSTANCE": instance,
	}
	if opts.Network == "private" {
		proxyURL := fmt.Sprintf("http://127.0.0.1:%d", proxyGuestPort)
		environment["HTTP_PROXY"], environment["http_proxy"] = proxyURL, proxyURL
		environment["HTTPS_PROXY"], environment["https_proxy"] = proxyURL, proxyURL
		environment["NO_PROXY"], environment["no_proxy"] = "localhost,127.0.0.1,::1", "localhost,127.0.0.1,::1"
	}
	if workspaceMode == "read-only" {
		environment["GIT_OPTIONAL_LOCKS"] = "0"
	}
	for name, value := range terminalEnv(input.hostEnv) {
		environment[name] = value
	}
	for name, value := range input.agent {
		environment[name] = input.home.destination(value)
	}
	for name, value := range input.command {
		environment[name] = value
	}
	for _, inherited := range []string{"LANG", "TZ"} {
		if value, found := hostEnvValue(input.hostEnv, inherited); found {
			environment[inherited] = value
		}
	}
	for _, assignment := range opts.Env {
		name, value, found := strings.Cut(assignment, "=")
		if !found || name == "" || strings.ContainsRune(value, 0) {
			return nil, fmt.Errorf("invalid --env assignment: %q", assignment)
		}
		environment[name] = value
	}
	for _, name := range opts.UnsetEnv {
		if name == "" || strings.Contains(name, "=") || strings.ContainsRune(name, 0) {
			return nil, fmt.Errorf("invalid --unsetenv name: %q", name)
		}
		delete(environment, name)
	}
	// This launcher-owned status value must reflect the resolved plan. Runtime
	// security and cleanup use an internal command mode rather than trusting it.
	environment["BWRAP_AGENT_PODMAN"] = boolString(input.podman)
	if input.podman {
		// The outer podman-unshare supervisor keeps the user and mount namespace
		// alive for the full launch, so neither it nor nested Podman commands
		// need a persistent pause process.
		environment["PODMAN_NO_PAUSE_PROCESS"] = "1"
		// These paths belong to the instance, regardless of host or --env settings.
		delete(environment, "CONTAINERS_CONF")
		environment["CONTAINERS_CONF_OVERRIDE"] = input.containersConfig
		environment["CONTAINERS_STORAGE_CONF"] = input.storageConfig
		// podman unshare exports its store paths; these are bootstrap-only.
		delete(environment, "CONTAINERS_GRAPHROOT")
		delete(environment, "CONTAINERS_RUNROOT")
	}
	// Never accept a policy payload from configuration or the host environment.
	// Plan construction replaces it with a validated allowlist when enabled;
	// otherwise the sandbox runtime must not see this internal control value.
	delete(environment, internalLandlockEnvironment)
	return environment, nil
}

func buildLauncherEnvironment(environment map[string]string, input environmentInputs) map[string]string {
	launchEnv := cloneMap(environment)
	// Podman uses USER to look up subordinate IDs before entering its user
	// namespace. Sandbox overrides must not change the invoking account.
	if input.podman {
		launchEnv["USER"], launchEnv["LOGNAME"] = input.hostAccount, input.hostAccount
		launchEnv["PATH"] = envValue(input.hostEnv, "PATH", "")
		launchEnv = podmanBootstrapEnvironment(launchEnv, podmanBootstrapPlaceholder)
	} else {
		// Only the sandbox needs this private path; pasta runs on the host.
		delete(launchEnv, "XDG_RUNTIME_DIR")
	}
	return launchEnv
}

func terminalEnv(source []string) map[string]string {
	selected := map[string]string{}
	for _, assignment := range source {
		name, value, found := strings.Cut(assignment, "=")
		if !found || strings.ContainsRune(name, 0) || strings.ContainsRune(value, 0) {
			continue
		}
		if terminalEnvironment[name] || strings.HasPrefix(name, "LC_") || strings.HasPrefix(name, "OPENTUI_") {
			selected[name] = value
		}
	}
	return selected
}
