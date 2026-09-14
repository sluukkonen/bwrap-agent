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
# This file controls host access and is read before the sandbox starts. Review
# it, then run bwrap-agent config trust. Every edit requires approval again.
#
# Settings are applied in this order: user configuration, this project file,
# then run-command options. Uncomment only the settings you want to override.
# Every setting below is commented out, so this file is behavior-neutral as-is.

# Persistent instance name for this project. The command-line --instance option
# takes precedence. This setting is not valid in the user configuration.
# Default: a name derived from the project directory.
# instance = "my-project"

` + commonConfigTemplate

const userConfigTemplate = `# bwrap-agent user configuration
#
# These settings apply to all projects for the current user.
# Settings are applied in this order: this user file, project configuration,
# then run-command options. Uncomment only the settings you want to override.
# Every setting below is commented out, so this file is behavior-neutral as-is.

` + commonConfigTemplate

const commonConfigTemplate = `# Expose detected host agent configuration read-only and seed mutable
# credentials into instance state. Existing instance data is never cleared.
# Default: true.
# agent_config = true

# Network mode: "private" (HTTP/HTTPS allowlist enforced), "host"
# (shared and unrestricted), or "none".
# Default: "private".
# network = "private"

# HTTP/HTTPS origins reachable in private mode. An empty combined list denies
# outbound HTTP/HTTPS access; DNS A/AAAA lookups remain available for any name.
# Exact hosts, leading *. wildcards, and a full * wildcard
# are accepted. A missing port means 80 for HTTP or 443 for HTTPS. Arrays from
# configuration layers and the CLI are combined and deduplicated.
# network_allow = ["https://registry.example.com", "https://*.example.com"]

# Publish private-network ports on host loopback. Entries use
# "[HOST_PORT:]GUEST_PORT[/tcp|udp]"; HOST_PORT 0 chooses a free port.
# Default: []. Arrays from configuration layers and the CLI are appended.
# publish = ["13000:3000"]

# Podman mode: "auto" (enable when compatible and found), "on" (require),
# or "off". Enabled modes provide a lazy API socket. A read-only workspace or
# required Landlock enforcement disables "auto". Default: "auto".
# podman = "auto"

# Workspace behavior: "write-through" persists changes, "copy-on-write"
# provides a writable disposable overlay, and "read-only" rejects writes.
# Default: "write-through".
# workspace_mode = "write-through"

# Landlock filesystem enforcement: "auto" enables it when compatible and
# Podman is off, "required" fails closed, and "off" disables it. Podman is
# incompatible with required Landlock enforcement. Default: "auto".
# landlock = "auto"

# Seccomp syscall filtering: "auto" enables a built-in workload profile when
# supported, "required" fails closed, and "off" disables filtering. The
# profile is selected automatically based on whether Podman is enabled.
# Default: "auto".
# seccomp = "auto"

# Additional existing host paths to bind read-only. Relative paths are
# resolved from this file's directory. Default: []. Arrays are appended.
# ro_bind = ["./toolchain"]

# Additional existing host paths to bind read-write. Default: []. Arrays are
# appended.
# rw_bind = ["/var/lib/example"]

# Remove environment variables from the sandbox. Default: [].
# unset_env = ["SSH_AUTH_SOCK"]

# Controlling PTY mode: "auto", "always", or "never". Default: "auto".
# tty = "auto"

# Host clipboard bridge: "off" (default) or "wayland". Requires a PTY,
# host wl-clipboard, and a Wayland session. Allows sandbox processes to replace
# the regular clipboard through OSC 52; provides no clipboard-reading operation.
# clipboard = "off"

# Environment entries merge by variable name. Strings are literal, including
# empty strings. { inherit = true } copies the host value when it exists.
# [env]
# LITERAL = "value"
# EMPTY = ""
# FROM_HOST = { inherit = true }

# Project selection, dry-run mode, the control-file protection escape hatch,
# and the target command are run-command options only.
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
