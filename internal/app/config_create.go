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
# Uncomment only the settings you need. This fully commented file changes no
# defaults. Example hostnames and paths must be replaced with your own values.
#
# This file can grant access to host files, services, and credentials. Review
# it on the host, then run: bwrap-agent config trust
# Every edit, including comments, requires approval again before the next run.
#
# Precedence: built-in defaults < user file < project file < command line.
# Scalar values override earlier values; network and bind lists accumulate.
# Project selection and the program to run are command-line options only.

# Instance
# --------
# A persistent home, credentials, caches, and container storage for this project.
# Default: a name derived from the project directory. Set a name here or override
# it with --instance. A name belongs to one project; only one run can use it at
# a time. This setting is not allowed in the user configuration.
# instance = "my-project"

` + commonConfigTemplate

const userConfigTemplate = `# bwrap-agent user configuration
#
# These settings apply across your projects and need no separate approval.
# Uncomment only the settings you need. This fully commented file changes no
# defaults. Example hostnames and paths must be replaced with your own values.
#
# Precedence: built-in defaults < user file < project file < command line.
# Scalar values override earlier values; network and bind lists accumulate.
# Use project configuration or --instance for an instance name, not this file.
# Project selection and the program to run are command-line options only.

` + commonConfigTemplate

const commonConfigTemplate = `# Workspace
# ---------
# What happens to project edits: "write-through" saves them to the host,
# "copy-on-write" discards them at exit, and "read-only" rejects writes.
# Instance state persists in every mode. Explicit rw_bind paths still write to
# the host. Default: "write-through".
# workspace_mode = "write-through"

# Agent and Git configuration
# ---------------------------
# Expose detected OpenCode or Pi host configuration read-only and seed their
# credentials into the instance once. The sandbox can read those credentials.
# Setting false prevents new exposure/seeding; it does not clear existing state.
# Default: true.
# agent_config = true

# Expose ~/.gitconfig and the XDG Git config file read-only, including any
# credentials they contain. Included files and other referenced resources need
# explicit binds. Setting false does not clear instance data. Default: true.
# git_config = true

# Networking
# ----------
# "private": separate local ports, with outside HTTP/HTTPS access controlled by
# network_allow. "host": shared, unrestricted host networking. "none": offline.
# A non-empty network_allow is rejected with "host" and ignored with "none".
# Default: "private".
# network = "private"

# Allowed HTTP/HTTPS origins in private mode. Default: [] (no outside access
# through the proxy). Add your agent's provider and any required registries.
# Programs must use the HTTP proxy variables; direct external sockets have no
# route. A missing port means 80 for HTTP or 443 for HTTPS.
# A leading *. matches subdomains, not the parent; a full * allows every host.
# Lists accumulate across files and CLI options; normalized duplicates are
# removed. DNS A/AAAA queries require an allowed hostname, regardless of port
# or scheme. Example values, not built-in permissions:
# network_allow = ["https://api.example.com", "https://*.packages.example.com"]

# Host -> sandbox: reach a sandbox server through a host loopback port.
# Format: "[HOST_PORT:]SANDBOX_PORT[/tcp|udp]". Defaults to the same port and TCP;
# host port 0 allocates an available port. Requires network = "private".
# Default: []. Lists accumulate. Example: host :13000 -> sandbox :3000.
# publish = ["13000:3000"]

# Sandbox -> host: reach a selected host loopback service from the sandbox.
# Format: "[SANDBOX_PORT:]HOST_PORT[/tcp|udp]". Defaults to the same port and TCP;
# both ports must be 1-65535. Requires network = "private".
# These services bypass the HTTP allowlist and retain their host privileges.
# Default: []. Lists accumulate; identical mappings are removed.
# Examples: sandbox :9222 -> host :9222; sandbox :15432 -> host :5432.
# host_port = ["9222", "15432:5432"]

# Containers and enforcement
# --------------------------
# "auto": enable Podman when installed and compatible. "on": require it.
# "off": disable it. Enabled modes provide a sandbox-local Docker-compatible
# API socket, started on demand. The host Podman socket is never exposed.
# A read-only workspace or required Landlock disables "auto" and rejects "on".
# Default: "auto".
# podman = "auto"

# Additional kernel filesystem enforcement. "auto": enable when supported and
# Podman is off. "required": refuse launch if unavailable; disables automatic
# Podman. "off": disable. Required Landlock and Podman are incompatible.
# Default: "auto".
# landlock = "auto"

# Kernel syscall filtering. "auto": enable a built-in profile when supported.
# "required": refuse launch if unavailable. "off": disable filtering.
# The profile depends on whether Podman is enabled. Default: "auto".
# seccomp = "auto"

# Additional host paths
# ---------------------
# Bind existing paths at the same absolute location inside the sandbox.
# Relative paths resolve from this configuration file's directory for both
# settings. Lists accumulate across files and CLI options. Default: [] each.

# Read-only access. Example:
# ro_bind = ["./toolchain"]

# Read-write access, including in copy-on-write and read-only workspace modes.
# Writes persist on the host. Example:
# rw_bind = ["/var/lib/example"]

# Terminal and clipboard
# ----------------------
# A private controlling terminal. "auto": use one when stdin and stdout are
# terminals. "always": require one. "never": disable. Default: "auto".
# tty = "auto"

# "wayland": let sandbox processes replace the host clipboard through terminal
# OSC 52 requests. Requires a PTY, host wl-clipboard, and a Wayland session.
# Provides no clipboard-reading operation. "off": no bridge. Default: "off".
# clipboard = "off"

# Environment
# -----------
# Most host environment variables, including secrets, are not inherited.
# Remove a variable from the sandbox environment. Default: [].
# unset_env = ["SSH_AUTH_SOCK"]

# Keep all other settings ABOVE [env]: TOML puts subsequent keys in that table.
# Uncomment the table header as well as the variables you need.
# Entries merge by name across layers. Strings set literal values (even "");
# { inherit = true } copies the named host variable, leaving it unset if absent.
# Use inheritance for tokens rather than storing secrets in this file.
# These variable names are examples; replace them with those your tools need.
# [env]
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
