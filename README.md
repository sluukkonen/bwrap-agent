# bwrap-agent

`bwrap-agent` runs an AI coding agent with the host's normal development tools while hiding the user's home directory. By default, only the selected project, sandbox state, and (when needed) Git worktree metadata are writable.

This is an early Linux-only implementation. Its default container design is intentionally different from mounting the host Podman socket: the launcher enters a dedicated rootless Podman user namespace first, then applies network isolation and bubblewrap. Podman and its lazily activated API service run *inside* that boundary, so a malicious `podman run -v /:/host` can only see the synthetic sandbox root.

## Quick start

Runtime requirements are bubblewrap, pasta, and optionally Podman. Rootless Podman must be configured for the user with subordinate UID/GID ranges. Building from source requires Go 1.24 or newer.

```console
$ make build
```

Run `./bin/bwrap-agent` without arguments to display the available commands.

```console
$ ./bin/bwrap-agent run --project . opencode
```

The program and all following arguments are passed through. Any executable can be used, which is also useful for testing the sandbox directly:

```console
$ ./bin/bwrap-agent run --project . aider --model sonnet
$ ./bin/bwrap-agent run --project . podman info
$ ./bin/bwrap-agent run --project . bash
```

The `run` command requires a program; there is no implicit default agent. The launcher currently detects OpenCode and Pi by executable basename. Their user-managed host configuration, extensions, skills, and packages are exposed read-only, so host edits are visible on the next launch without letting the sandbox rewrite them. Credentials and mutable runtime data remain writable, persistent, and isolated per instance. Use `--no-agent-config` to suppress new host configuration exposure and credential seeding; it never deletes data already stored in an instance.

For OpenCode, `$XDG_CONFIG_HOME/opencode` is mounted read-only while `$XDG_DATA_HOME/opencode`, including a seed-once copy of `auth.json`, remains instance-local. Host `OPENCODE_CONFIG`, `OPENCODE_TUI_CONFIG`, and `OPENCODE_CONFIG_DIR` path overrides are mapped to read-only sandbox paths. Other OpenCode environment settings still require an explicit `--env` or `[env]` entry.

For Pi, the host directory selected by `PI_CODING_AGENT_DIR` (normally `~/.pi/agent`) is presented as a layered view. Configuration and resources are read-only; `auth.json`, `trust.json`, `models-store.json`, and sessions are seed-once or instance-local writable state. Host `~/.agents/skills` is also exposed read-only. Pi's documented terminal overrides are forwarded automatically. Global `/settings`, model-save, and package-management operations may fail because their host-owned targets are read-only; make those changes with host Pi. Project-local Pi changes still follow the selected workspace write policy.

Configuration references to arbitrary files outside an agent's configuration tree are not exposed automatically. Add a deliberate `--ro-bind` and ensure the configured sandbox path resolves to that mount when such a reference is required.

By default, the instance uses a readable name derived from the project directory (for example, `bwrap-agent`). Returning to the same canonical project reuses its sandbox home, agent credentials, dependency caches, images, containers, and volumes. If that readable name already belongs to another project, a short hash of the canonical path is appended. Different worktree directory names therefore normally receive distinct instances automatically.

Set `instance = "feature-payments"` in the project configuration to persist a different name, or use `--instance` for a command-line override. Instance names are project-only configuration: a fixed name in the user-wide file would collide across projects. An explicit or configured name is permanently associated with one canonical project:

```console
$ ./bin/bwrap-agent run --instance feature-payments --project ../feature-payments opencode
```

An instance is protected by a fail-fast exclusive lock. If it is already running, use a separate worktree (recommended) or a different `--instance`. Agent-native session switching remains available inside one sandbox process. The launcher holds a host-only advisory lock for the instance; the kernel releases it automatically when bwrap-agent exits or crashes.

Managed instances live under `~/.local/state/bwrap-agent/instances`. The host-only instance directory contains `metadata.json` plus the sandbox-writable `state/` directory. List instances, including their allocated disk usage and running status, with:

```console
$ ./bin/bwrap-agent instance list
$ ./bin/bwrap-agent instance list --json
```

Delete a stopped instance interactively, or use `--yes` for scripts:

```console
$ ./bin/bwrap-agent instance delete feature-payments
$ ./bin/bwrap-agent instance delete feature-payments --yes
```

Running instances cannot be deleted. Automatic pruning is planned but not implemented yet. To relocate the complete managed store, set `BWRAP_AGENT_STATE_HOME`; otherwise the standard `XDG_STATE_HOME` location is honored. The store must remain separate from projects, external Git metadata, and explicit read-write binds.

## Configuration

Runtime behavior can be configured in TOML. The launcher loads these files in increasing precedence order:

1. `$XDG_CONFIG_HOME/bwrap-agent/config.toml`, or `~/.config/bwrap-agent/config.toml` when `XDG_CONFIG_HOME` is unset
2. `.bwrap-agent.toml` in the selected canonical project directory
3. explicit command-line options

Create a behavior-neutral, fully commented project configuration reference in the current directory with:

```console
$ ./bin/bwrap-agent config create
Created .bwrap-agent.toml
```

The command refuses to replace an existing file. It does not load configuration or start a sandbox.

Missing files are ignored. Existing files are parsed strictly: unknown keys, invalid values, and wrong types stop the launch instead of being silently ignored. `--no-project-config` skips the project file, while `--no-config` skips both files. `--help` remains available even when a config file is broken.

```toml
instance = "my-project" # project configuration only
agent_config = true
network = "private"
podman = "auto"
write_policy = "workspace"
tty = "auto"

publish = ["13000:3000"]
ro_bind = ["./toolchain"]
rw_bind = ["/var/lib/example"]
unset_env = ["SSH_AUTH_SOCK"]

[env]
LITERAL = "value"
EMPTY = ""
FROM_HOST = { inherit = true }
```

Scalar settings are replaced by higher-precedence layers. `instance` is accepted only in the project file, and `--instance` takes precedence over it. `publish`, `ro_bind`, and `rw_bind` arrays are appended; relative bind paths are resolved from the directory containing their config file. Environment entries merge by name. A string is literal, including an empty string, while `{ inherit = true }` deliberately copies the same-named variable from the launcher's host environment. If that variable is absent, it remains unset. The equivalent CLI forms are `--env NAME=value`, `--env NAME=`, and `--env NAME`.

> **Warning:** Project configuration is fully trusted and is evaluated before the sandbox starts. It can request arbitrary host bind mounts, select host networking, and expose sensitive host environment variables. Inspect `.bwrap-agent.toml` in an untrusted checkout or launch with `run --no-project-config`.

`project`, the target command, and action/recovery options are intentionally not accepted in TOML. Use `run --dry-run` to inspect the effective launch plan and the user/project config files that were loaded.

For inspection or verification without allowing project changes, use the `state-only` write policy:

```console
$ ./bin/bwrap-agent run --write-policy state-only --project . opencode
```

The project and linked-worktree Git metadata are then read-only. Sandbox-managed state remains writable and persistent so agents can retain sessions and credentials, development tools can cache dependencies, and Podman can store images and container layers. Private temporary filesystems also remain writable. `--rw-bind` is rejected in this mode. A future ephemeral-state option can provide disposable writable state as a separate lifetime policy.

## Networking and ports

The default `private` mode gives every invocation its own network namespace. Agents can independently bind `localhost:3000` or publish a Podman container on `localhost:5432` without colliding with another sandbox. Those ports are initially reachable only from that sandbox.

Expose a port to the host explicitly:

```console
# Fixed host mapping
$ ./bin/bwrap-agent run --publish 13000:3000 opencode

# Ask the launcher to choose an unused host port; the mapping is printed
$ ./bin/bwrap-agent run --publish 0:3000 opencode
```

`--network host` is available for compatibility but loses port isolation. `--network none` disables networking. In an air-gapped installation, a future HTTP allowlist mode can replace pasta without changing agent adapters.

## Podman and Testcontainers

When Podman is available, the local Podman CLI and Podman Compose work normally and a sandbox-local API socket is provided automatically. All containers in an instance are stopped when its agent exits. Override detection with `--podman on` or disable the integration with `--podman off`.

Docker-compatible clients and Testcontainers use the socket automatically:

```console
$ ./bin/bwrap-agent run --project . opencode
```

The listener exists before the target starts, but `podman system service` is activated only when a client first connects. The launcher sets `DOCKER_HOST` and `TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE` inside the sandbox. It deliberately leaves `CONTAINER_HOST` unset so the Podman CLI retains access to local-only operations. It never exposes the host Podman socket.

Both root and non-root container processes are supported. Container root remains rootless with respect to the host. A non-root container can read ordinary project bind mounts, but writing host-user-owned files requires the normal Podman mapping, for example:

```console
$ bwrap-agent run podman run --user 1000:1000 \
    --userns keep-id:uid=1000,gid=1000 \
    -v "$PWD:/workspace" IMAGE COMMAND
```

The equivalent Compose service can use `user: "1000:1000"` and `userns: "keep-id:uid=1000,gid=1000"`. This is standard rootless Podman ownership behavior, not an additional sandbox restriction. A root/non-root Compose fixture lives at `tests/fixtures/compose-root-and-user.yml`.

Do not bind the host Podman socket into this sandbox. Podman's API is deliberately equivalent to arbitrary execution as the socket owner, including host path mounts; a simple socket mount would defeat the filesystem boundary.

## Useful `run` options

```text
--project PATH             expose PATH as the policy-controlled project directory
--instance NAME            select a managed instance name
--network private|host|none
                           choose isolated (default), shared, or disabled networking
--publish PORT             publish a private-network port on host loopback
--dry-run                 print the full launch plan as JSON
--ro-bind PATH            expose one additional host path read-only
--rw-bind PATH            expose one additional host path read-write
--env NAME[=VALUE]        set a literal value, or inherit NAME from the host
--unsetenv NAME           remove an environment variable
--tty auto|always|never   isolated controlling PTY policy (default: auto)
--podman auto|on|off      detect Podman, require it, or disable it (default: auto)
--write-policy workspace|state-only
                           allow workspace writes (default), or only instance-state writes
--[no-]agent-config        expose detected host agent config read-only and seed credentials
--no-project-config       skip .bwrap-agent.toml
--no-config               skip user and project configuration
```

The launcher clears the inherited environment. It keeps terminal and locale settings, but credentials, SSH agent sockets, cloud variables, and tokens are not forwarded unless explicitly requested with `--env NAME` or an `{ inherit = true }` config entry.

### Terminal multiplexers

Terminal capability and multiplexer hints are forwarded through a strict allowlist: `TERM`, `TERMCAP`, terminfo paths, `COLORTERM`, `COLORFGBG`, `TERM_PROGRAM*`, VTE/Konsole/Kitty/Windows Terminal identifiers, `TMUX`, `TMUX_PANE`, `STY`, `WINDOW`, ncurses and color-control variables, dimensions, locale categories, and `OPENTUI_*` overrides. This lets OpenCode distinguish GNU Screen and tmux while still excluding general host environment variables.

Some Screen setups inherit `COLORTERM=truecolor` from the outer terminal even though the Screen terminfo only advertises 256 colors. If colors or escape sequences remain broken, try:

```console
$ ./bin/bwrap-agent run --unsetenv COLORTERM opencode
```

Use `./bin/bwrap-agent run --dry-run opencode` to inspect exactly which environment values will be exposed.

Interactive launches use a dedicated PTY automatically. The sandbox receives a real controlling foreground terminal without access to the host terminal device. Non-interactive pipelines keep separate stdout/stderr and bubblewrap's `--new-session` isolation. If detection is wrong, force the behavior with `--tty always` or `--tty never`.

## Development

```console
$ make build
$ make test
$ make test-race
$ make vet
$ make integration
$ make dist
$ ./bin/bwrap-agent run --podman off --network host --dry-run /usr/bin/id
```

Set `BWRAP_AGENT_TEST_IMAGE` to an Alpine-compatible image available from the test environment to include root and non-root container execution in `make integration`.

The launcher is a CGO-free Go executable. Standard Go modules are used through `go.mod` and `go.sum`; dependencies are not vendored.

See [the architecture](docs/architecture.md), [threat model](docs/threat-model.md), and [roadmap](docs/roadmap.md) before relying on this for hostile workloads.
