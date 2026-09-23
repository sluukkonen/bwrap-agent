package app

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

const projectConfigName = ".bwrap-agent.toml"

const projectConfigTemplate = `# bwrap-agent project configuration
#
# Uncomment only the settings you need. Commented settings change nothing.
# Replace example hostnames and paths with your own values.
#
# This file can grant access to host files, services, and credentials. After
# reviewing it on the host, run: bwrap-agent config trust
# Every edit, including comments, requires trusting the file again.
#
# Settings apply in this order: built-in defaults, user file, project file, CLI.
# Later scalar values win; network and bind lists add to earlier lists.
# The project and the program to run are command-line options only.

# Instance
# --------
# Choose the persistent home, credentials, caches, and container storage to use.
# Default: a name derived from the project directory.
# A name belongs to one project and can be used by only one run at a time.
# Project configuration only; --instance overrides this setting.
# Example:
# instance = "my-project"

` + commonConfigTemplate

const userConfigTemplate = `# bwrap-agent user configuration
#
# These settings apply across your projects and need no separate approval.
# Uncomment only the settings you need. Commented settings change nothing.
# Replace example hostnames and paths with your own values.
#
# Settings apply in this order: built-in defaults, user file, project file, CLI.
# Later scalar values win; network and bind lists add to earlier lists.
# Set an instance name in project configuration or with --instance.
# The project and the program to run are command-line options only.

` + commonConfigTemplate

const commonConfigTemplate = `# Workspace
# ---------
# Control what happens to project edits. Default: "write-through".
#   "write-through" - save edits to the host
#   "copy-on-write" - discard edits when the sandbox exits
#   "read-only"     - reject writes
# Instance state persists in every mode. Explicit rw_bind paths still write
# to the host in every mode.
# workspace_mode = "write-through"

# Agent and Git configuration
# ---------------------------
# Share host agent configuration read-only and copy credentials into the
# instance once. The sandbox can read these credentials. Default: "auto".
#   "auto"     - detect OpenCode or Pi from the first program's basename
#   "opencode" - use OpenCode configuration, including with wrappers
#   "pi"       - use Pi configuration, including with wrappers
#   "off"      - stop new sharing and copying; keep existing instance data
# Auto does not inspect arguments or launcher scripts. For tmux or a custom
# launcher, select the agent explicitly. This does not install the agent or
# change the command being run.
# agent_config = "auto"

# Share ~/.gitconfig and the XDG Git config file read-only. Default: true.
# The sandbox can read any credentials in these files. Included files and
# other referenced resources need explicit binds. Setting false leaves
# existing instance data in place.
# git_config = true

# Networking
# ----------
# Choose how the sandbox connects to the network. Default: "private".
#   "private" - separate local ports; outside HTTP/HTTPS uses network_allow
#   "host"    - share the host network with unrestricted access
#   "none"    - offline
# A non-empty network_allow is rejected with "host" and ignored with "none".
# network = "private"

# Allow HTTP/HTTPS origins in private mode. Default: [] (no outside proxy access).
# Add your agent's provider and any required package or container registries.
# Programs must use the HTTP proxy environment variables; direct external
# sockets have no route. Omitted ports mean 80 for HTTP and 443 for HTTPS.
# A leading *. matches subdomains only; a full * allows every host.
# Lists accumulate across files and CLI options; normalized duplicates are
# removed. DNS A/AAAA queries require an allowed hostname, regardless of the
# origin's scheme or port.
# Example origins (replace with the services you need):
# network_allow = [
#     "https://api.example.com",
#     "https://*.packages.example.com",
# ]

# Reach a sandbox server through a host loopback port. Default: [].
# Requires "private" network.
# Format: "[HOST_PORT:]SANDBOX_PORT[/tcp|udp]".
# Omitting HOST_PORT uses the sandbox port; omitting the protocol uses TCP.
# Host port 0 chooses an available port. Lists accumulate.
# Example: host localhost:13000 connects to sandbox port 3000.
# publish = [
#     "13000:3000",
# ]

# Reach a host loopback service from the sandbox. Default: [].
# Requires "private" network. These services bypass the HTTP allowlist and
# retain their host privileges.
# Format: "[SANDBOX_PORT:]HOST_PORT[/tcp|udp]".
# Omitting SANDBOX_PORT uses the host port; omitting the protocol uses TCP.
# Both ports must be 1-65535. Lists accumulate; identical mappings are removed.
# Examples: sandbox port 9222 connects to host port 9222;
# sandbox port 15432 connects to host port 5432.
# host_port = [
#     "9222",
#     "15432:5432",
# ]

# Containers and enforcement
# --------------------------
# Run containers with a sandbox-local Podman service. Default: "auto".
#   "auto" - enable when installed and compatible
#   "on"   - require Podman
#   "off"  - disable Podman
# When enabled, a Docker-compatible API starts on demand. The host Podman
# socket is never exposed. A read-only workspace or required Landlock
# disables "auto" and rejects "on".
# podman = "auto"

# Add kernel filesystem enforcement with Landlock. Default: "auto".
#   "auto"     - enable when supported and Podman is off
#   "required" - refuse launch if unavailable; disable automatic Podman
#   "off"      - disable Landlock
# Required Landlock and enabled Podman are incompatible.
# landlock = "auto"

# Filter kernel system calls with seccomp. Default: "auto".
#   "auto"     - enable a built-in profile when supported
#   "required" - refuse launch if unavailable
#   "off"      - disable filtering
# The profile depends on whether Podman is enabled.
# seccomp = "auto"

# Additional host paths
# ---------------------
# Share existing host paths at the same absolute location inside the sandbox.
# Relative paths resolve from this configuration file's directory, including
# in user configuration. Lists accumulate across files and CLI options.

# Grant read-only access to host paths. Default: [].
# Example:
# ro_bind = [
#     "./toolchain",
# ]

# Grant read-write access to host paths. Default: [].
# Writes persist on the host, even in copy-on-write and read-only workspaces.
# Example:
# rw_bind = [
#     "/var/lib/example",
# ]

# Terminal and clipboard
# ----------------------
# Use a private controlling terminal (PTY). Default: "auto".
#   "auto"   - use one when stdin and stdout are terminals
#   "always" - require one
#   "never"  - disable it
# tty = "auto"

# Allow writes to the host clipboard. Default: "off".
#   "off"     - no clipboard bridge
#   "wayland" - accept terminal OSC 52 clipboard requests
# Requires a PTY, host wl-clipboard, and a Wayland session. Sandbox processes
# can replace the host clipboard; the bridge provides no clipboard reading.
# clipboard = "off"

# Environment
# -----------
# Most host environment variables, including secrets, are not inherited.

# Remove named variables from the sandbox environment. Default: [].
# Removal wins over [env] entries in the same file. Later layers can set them
# again. Example:
# unset_env = [
#     "SSH_AUTH_SOCK",
# ]

# Set or inherit variables. Default: no overrides. Entries merge by name;
# later layers override earlier ones.
# Strings set literal values, including an empty string. { inherit = true }
# copies the host variable; if absent on the host, it is unset in the sandbox.
# Inherit tokens instead of storing secrets in this file.
#
# Keep all other settings ABOVE [env]: TOML puts subsequent keys in that table.
# Uncomment the variables you need. Replace these example names with the
# variables your tools use. An empty [env] table changes no settings.
[env]
# LITERAL = "value"
# EMPTY = ""
# FROM_HOST = { inherit = true }
`

func createProjectConfig(directory string) error {
	return createConfigFile(filepath.Join(directory, projectConfigName), projectConfigTemplate, 0o644)
}

func createConfigFile(path, template string, mode fs.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%s already exists", path)
		}
		return fmt.Errorf("create %s: %w", path, err)
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.WriteString(template); err != nil {
		_ = file.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	complete = true
	return nil
}

func createProjectConfigAndReport(directory string, stdout io.Writer) error {
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return fmt.Errorf("locate project configuration: %w", err)
	}
	if err := createProjectConfig(absolute); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "Created %s\nReview the file, then run bwrap-agent config trust.\n", filepath.Join(absolute, projectConfigName))
	return err
}

func createUserConfigAndReport(stdout io.Writer) error {
	path, err := userConfigPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create parent directories for %s: %w", path, err)
	}
	if err := createConfigFile(path, userConfigTemplate, 0o600); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "Created %s\n", path)
	return err
}
