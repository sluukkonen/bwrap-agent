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
# This file is fully trusted and is read before the sandbox starts. Inspect it
# before running bwrap-agent in an untrusted checkout.
#
# Settings are applied in this order: user configuration, this project file,
# then run-command options. Uncomment only the settings you want to override.
# Every setting below is commented out, so this file is behavior-neutral as-is.

# Persistent instance name for this project. The command-line --instance option
# takes precedence. This setting is not valid in the user configuration.
# Default: a name derived from the project directory.
# instance = "my-project"

# Expose detected host agent configuration read-only and seed mutable
# credentials into instance state. Existing instance data is never cleared.
# Default: true.
# agent_config = true

# Network mode: "private" (isolated via pasta), "host", or "none".
# Default: "private".
# network = "private"

# Publish private-network ports on host loopback. Entries use
# "[HOST_PORT:]GUEST_PORT[/tcp|udp]"; HOST_PORT 0 chooses a free port.
# Default: []. Arrays from configuration layers and the CLI are appended.
# publish = ["13000:3000"]

# Podman mode: "auto" (enable when found), "on" (require), or "off".
# Enabled modes provide a lazy API socket. Default: "auto".
# podman = "auto"

# Host write policy: "workspace" or "state-only".
# Default: "workspace".
# write_policy = "workspace"

# Additional existing host paths to bind read-only. Relative paths are
# resolved from this file's directory. Default: []. Arrays are appended.
# ro_bind = ["./toolchain"]

# Additional existing host paths to bind read-write. Incompatible with
# write_policy = "state-only". Default: []. Arrays are appended.
# rw_bind = ["/var/lib/example"]

# Remove environment variables from the sandbox. Default: [].
# unset_env = ["SSH_AUTH_SOCK"]

# Controlling PTY mode: "auto", "always", or "never". Default: "auto".
# tty = "auto"

# Environment entries merge by variable name. Strings are literal, including
# empty strings. { inherit = true } copies the host value when it exists.
# [env]
# LITERAL = "value"
# EMPTY = ""
# FROM_HOST = { inherit = true }

# Project selection, dry-run mode, and the target command are run-command
# options only.
`

func createProjectConfig(directory string) error {
	path := filepath.Join(directory, projectConfigName)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%s already exists", projectConfigName)
		}
		return fmt.Errorf("create %s: %w", projectConfigName, err)
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.WriteString(projectConfigTemplate); err != nil {
		_ = file.Close()
		return fmt.Errorf("write %s: %w", projectConfigName, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", projectConfigName, err)
	}
	complete = true
	return nil
}

func createProjectConfigAndReport(directory string, stdout io.Writer) error {
	if err := createProjectConfig(directory); err != nil {
		return err
	}
	_, err := fmt.Fprintf(stdout, "Created %s\n", projectConfigName)
	return err
}
