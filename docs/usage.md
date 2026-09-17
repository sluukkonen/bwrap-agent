# Usage guide

Start with the [README](../README.md) for prerequisites and a first run.
Examples below use `./bin/bwrap-agent` from the repository root; substitute
the installed binary or its absolute path when working in another project.
Networked agent examples assume you have configured access to your provider.
Run `./bin/bwrap-agent run --help` for the complete command-line option
reference.

- [Configuration and approval](#configuration)
- [Workspace modes and protection](#workspace-modes-and-protection)
- [Agent and Git configuration](#agent-and-git-configuration)
- [Persistent instances](#persistent-instances)
- [Networking and ports](#networking-and-ports)
- [Podman and Testcontainers](#podman-and-testcontainers)
- [Environment and terminal](#environment-and-terminal)
- [Troubleshooting and upgrades](#troubleshooting-and-upgrades)

## Sandbox home

The sandbox uses the same `$HOME` path as the host, with private contents stored per instance. For example, `/home/alice` inside the sandbox is backed by the instance's `state/home` directory. Host home files are visible only through the project, explicit binds, or automatic configuration mounts. A configuration entry such as `ro_bind = ["~/tools"]` therefore appears at sandbox `~/tools` as well.

Sandbox XDG directories use the standard home layout: `~/.config`, `~/.cache`, `~/.local/share`, and `~/.local/state`. Existing `state/config` and `state/data` directories back `~/.config` and `~/.local/share`, so existing settings, credentials, and sessions require no migration. These mounts cover any old entries at `state/home/.config` and `state/home/.local/share` without deleting them. Host XDG settings still select the configuration sources to expose. Explicit environment overrides retain their normal precedence.

## Configuration

Runtime behavior can be configured in TOML. Starting from built-in defaults,
the launcher applies these sources in increasing precedence order:

1. `$XDG_CONFIG_HOME/bwrap-agent/config.toml`, or
   `~/.config/bwrap-agent/config.toml` when `XDG_CONFIG_HOME` is unset or empty
2. `.bwrap-agent.toml` in the selected canonical project directory
3. explicit command-line options

Create a behavior-neutral, fully commented project configuration reference in
the current directory with:

```console
$ ./bin/bwrap-agent config create project
Created /path/to/project/.bwrap-agent.toml
Review the file, then run bwrap-agent config trust.
```

Create a user configuration reference that applies across all your projects
with:

```console
$ ./bin/bwrap-agent config create user
Created /home/alice/.config/bwrap-agent/config.toml
```

The user command honors `XDG_CONFIG_HOME` and creates missing parent
directories. Its template omits the project-only `instance` setting. Both
commands refuse to replace an existing file and do not load configuration or
start a sandbox. Bare `config create` requires a scope and creates nothing.

Before a project configuration can be loaded, review it and approve its exact
contents on the host:

```console
$ ./bin/bwrap-agent config trust --project /path/to/project
$ ./bin/bwrap-agent config untrust --project /path/to/project
```

Both commands default to the current directory. `trust` validates the
configuration and records its canonical project path and SHA-256 digest;
`untrust` removes that approval. No launch-time confirmation is shown. New,
changed (including comments), or unapproved configuration stops launches and
dry runs before its settings are applied, with instructions to review and
approve it. Existing configurations require initial approval after upgrading,
and moving a project requires approval at its new location. The file must be
regular, must not be a symlink, and must be at most 1 MiB. `config create
project` does not automatically approve its output.

Approvals are stored outside the sandbox in `trusted-projects` under the host
state directory (`BWRAP_AGENT_STATE_HOME`, otherwise
`$XDG_STATE_HOME/bwrap-agent`, otherwise `~/.local/state/bwrap-agent`).
Changing the state directory selects a different approval store.
Sandbox-writable paths must not overlap this store or its path aliases. User
configuration is trusted without a separate approval step.

Missing files are ignored. Existing files are parsed strictly: unknown keys,
invalid values, and wrong types stop the launch instead of being silently
ignored. `--no-project-config` skips the project file, while `--no-config`
skips both files. `--help` remains available even when a config file is
broken.

This example illustrates the syntax; replace the hostnames and paths with
resources you actually need. Settings you omit retain their defaults.

```toml
instance = "my-project" # project configuration only
agent_config = true
git_config = true
network = "private"
network_allow = ["https://registry.example.com", "https://*.packages.example.com"]
podman = "auto"
workspace_mode = "write-through"
landlock = "auto"
seccomp = "auto"
tty = "auto"

publish = ["13000:3000"]
host_port = ["9222"]
ro_bind = ["./toolchain"]
rw_bind = ["/var/lib/example"]
unset_env = ["SSH_AUTH_SOCK"]

[env]
LITERAL = "value"
EMPTY = ""
FROM_HOST = { inherit = true }
```

Scalar settings are replaced by higher-precedence layers. `instance` is
accepted only in the project file, and `--instance` takes precedence over it.
`network_allow`, `publish`, `host_port`, `ro_bind`, and `rw_bind` arrays are
appended; duplicate normalized network origins are removed, and relative bind
paths are resolved from the directory containing their config file.
Environment entries merge by name. A string is literal, including an empty
string, while `{ inherit = true }` deliberately copies the same-named variable
from the launcher's host environment. If that variable is absent, it remains
unset. The equivalent CLI forms are `--env NAME=value`, `--env NAME=`, and
`--env NAME`.

> **Warning:** Approved project configuration is evaluated before the sandbox
> starts. It can request arbitrary host bind mounts, select host networking,
> and expose host environment variables. Review `.bwrap-agent.toml` before
> approving it, or launch with `run --no-project-config` to ignore it.
> Approval authorizes the exact configuration bytes, not the contents of
> referenced paths.

Keep top-level settings above `[env]`; entries after that header belong to the
environment table.

`project`, the target command, and action/recovery options are intentionally
not accepted in TOML. Use `run --dry-run` to inspect the effective launch plan
and the user/project config files that were loaded.

## Workspace modes and protection

By default, project edits write through to the host. Workspace behavior is
independent of persistent instance state. For a writable view whose changes
are discarded at exit, use `copy-on-write`:

```console
$ ./bin/bwrap-agent run --workspace-mode copy-on-write --project . opencode
```

The project and linked-worktree Git metadata are temporary overlays. This is
useful for destructive builds and tests without persistent workspace damage.
On SELinux hosts, nested per-container labeling is disabled in this mode
because the private overlay cannot be relabeled; the outer bwrap-agent process
remains confined. Use `read-only` when writes must fail; automatic Podman
integration is disabled in that mode, and explicit `--podman=on` is rejected.
Explicit `--rw-bind` paths remain deliberate writable exceptions in every
mode.

With Podman enabled, the launcher creates the Bubblewrap mount hierarchy in
the outer rootless Podman user namespace, then runs the agent and Podman in a
less-privileged child user/mount namespace with the same subordinate-ID range.
Linux locks the inherited mounts as one unit: ordinary workspace writes still
reach the host immediately, while even privileged containers cannot detach
read-only control paths or reveal their underlying files. This also preserves
direct read-only binds of Unix sockets and filesystems that cannot be
OverlayFS lower layers.

Landlock filesystem enforcement defaults to `auto`. It is applied when Podman
is disabled and a sufficiently recent kernel is available. `--landlock
required` disables automatic Podman and fails closed unless Landlock ABI 3 or
newer is usable; `--landlock off` is available for compatibility diagnostics.
Instance state remains writable and persistent in all workspace modes; private
temporary filesystems are discarded at exit.

### Protected project files

In the default `write-through` workspace mode, existing high-impact project
control paths are read-only for every target program: `.bwrap-agent.toml`; Git
configuration, hooks, and linked-worktree metadata; OpenCode project
configuration, plugins, and tools; and Pi project settings, extensions, and
package directories. Missing paths stay absent and writable; the launcher
creates no placeholders. New control files become eligible for protection on
the next launch. Existing protected directories also block new entries inside
them. OpenCode writes to an existing `.opencode/.gitignore` through a private
backing file, preserving host contents with a 1 MiB copy limit. If absent,
OpenCode may create its own housekeeping file normally. Prompts, skills,
themes, commands, agent definitions, `.mcp.json`, and ordinary source files
remain writable. This is defense in depth around conventional paths, not a
complete executable-content policy.

New agent configurations, plugins, and Git hooks can affect later agent
sessions or commands you run on the host. Review generated project content
before host execution. The [configuration approval](#configuration) covers
`.bwrap-agent.toml` only; protecting a newly created file on the next launch
does not establish that it is trustworthy.

For an exceptional trusted workflow that must edit the built-in control paths,
use the CLI-only `run --allow-control-file-writes` escape hatch. It disables
the complete built-in control-path policy and prints a warning in
`write-through`; it has no additional effect in the other workspace modes. It
does not bypass configuration approval or permit symlinked project
configuration. The escape hatch is deliberately unavailable in TOML.

### Additional host files

The sandbox builds `/etc` from a small runtime allowlist instead of exposing
the host directory. Account and NSS files are generated for the sandbox;
loader configuration, selected Java and Maven runtime configuration, public CA
trust, timezone and operating-system identification are mounted read-only when
present. Distribution CA roots linked from those paths, including
`/var/lib/ca-certificates`, are also mounted without exposing the surrounding
hierarchy. Podman adds only its registry/signature policy and required host
security configuration. Host account inventories, machine identity, SSH
configuration, system-wide Git configuration, unrelated package-manager
configuration, and registry client credentials are not exposed by default.
`/opt` and `/nix/store` are also absent unless selected explicitly with
`--ro-bind` or `ro_bind`.

Use `--ro-bind PATH` for an additional existing host path the sandbox needs to
read, or `--rw-bind PATH` if it must also write there. These paths are exposed
at the same absolute location inside the sandbox. In configuration files,
relative paths are resolved from the file's directory. Writable binds are
persistent exceptions even when the workspace is disposable or read-only.

### Kernel enforcement

Seccomp filtering defaults to `auto`. Supported amd64 and arm64 hosts use a
conservative `development` denylist when Podman is disabled and a smaller
`podman` denylist that preserves container namespace and mount operations when
it is enabled. `--seccomp required` fails closed when filtering is
unavailable, while `--seccomp off` is the compatibility escape hatch for
software that needs a blocked syscall or a non-native ABI. Filtered runs
deliberately reject 32-bit and x32 syscall ABIs; ptrace, perf, io_uring,
userfaultfd, Landlock, and application-installed seccomp filters remain
available.

Both profiles deny `acct`, kernel module loading and deletion, kexec,
`lookup_dcookie`, swapping, and the kernel `syslog` syscall; amd64 also denies
`ioperm`, `iopl`, `modify_ldt`, and `uselib`. The `development` profile
additionally denies BPF loading, system-clock changes, reboot, kernel keyring
operations, quota and file-handle interfaces, chroot, namespace entry and
creation, and mount operations. New mount API calls and `clone3` return
`ENOSYS` so software can use older fallbacks; `clone` returns `EPERM` only
when a `CLONE_NEW*` namespace flag is present. Other denied calls return
`EPERM`.

See [kernel enforcement](architecture.md#kernel-enforcement) for
implementation details.

## Agent and Git configuration

The `run` command requires a program; there is no implicit default agent. The
launcher currently detects OpenCode and Pi by executable basename. Their
user-managed host configuration, extensions, skills, and packages are exposed
read-only, so host edits are visible on the next launch without letting the
sandbox rewrite them. Credentials and mutable runtime data remain writable,
persistent, and isolated per instance. Use `--no-agent-config` to suppress new
host configuration exposure and credential seeding; it never deletes data
already stored in an instance.

For OpenCode, `$XDG_CONFIG_HOME/opencode` is mounted read-only while
`$XDG_DATA_HOME/opencode`, including a seed-once copy of `auth.json`, remains
instance-local. Host `OPENCODE_CONFIG`, `OPENCODE_TUI_CONFIG`, and
`OPENCODE_CONFIG_DIR` path overrides are mapped to read-only sandbox paths.
Other OpenCode environment settings still require an explicit `--env` or
`[env]` entry.

For Pi, the host directory selected by `PI_CODING_AGENT_DIR` (normally
`~/.pi/agent`) is presented as a layered view. Configuration and resources are
read-only; `auth.json`, `trust.json`, `models-store.json`, and sessions are
seed-once or instance-local writable state. Host `~/.agents/skills` is also
exposed read-only. Pi's documented terminal overrides are forwarded
automatically. Global `/settings`, model-save, and package-management
operations may fail because their host-owned targets are read-only; make those
changes with host Pi. Project-local Pi changes follow the selected workspace
mode.

Configuration references to arbitrary files outside an agent's configuration
tree are not exposed automatically. Add a deliberate `--ro-bind` and ensure
the configured sandbox path resolves to that mount when such a reference is
required.

User Git configuration is exposed read-only by default for every command: host
`~/.gitconfig` is mounted at sandbox `$HOME/.gitconfig`, and
`$XDG_CONFIG_HOME/git/config` (falling back to `~/.config/git/config` when XDG
is unset or empty) is mounted at sandbox `$XDG_CONFIG_HOME/git/config`. Git
retains its normal configuration precedence, including repository overrides.
Host edits are picked up on the next launch; existing instance files are
covered by mounts without being overwritten. Disable automatic exposure with
`--no-git-config` or `git_config = false`; this is independent of
`--no-agent-config` and never deletes instance data.

These files are exposed in full, including any embedded credentials and
settings for signing, hooks, and credential helpers. Referenced resources and
included files are not automatically mounted, and signing settings are not
disabled. Relative includes resolve from the sandbox config file location, and
`~/` references use the sandbox home. Add explicit mounts at paths those
references can reach. Custom `GIT_CONFIG_GLOBAL` files require explicit
`--ro-bind` and `--env GIT_CONFIG_GLOBAL=...` settings; host Git environment
overrides are not inherited automatically. Only the two standard config files
are mounted, not the whole XDG Git directory.

## Persistent instances

By default, the instance uses a readable name derived from the project
directory (for example, `bwrap-agent`). Returning to the same canonical
project reuses its sandbox home, agent credentials, dependency caches, images,
containers, and volumes. If that readable name already belongs to another
project, a short hash of the canonical path is appended. Different worktree
directory names therefore normally receive distinct instances automatically.

Podman runtime files live in sandbox tmpfs and start fresh on every
invocation. The outer namespace bootstrap uses a separate disposable store, so
no host runtime-directory setup is required. Existing instances created with
persistent `state/run` paths may need manual replacement or repair because
Podman records runtime paths in its database; this change does not migrate or
reset them automatically.

Set `instance = "feature-payments"` in the project configuration to persist a
different name, or use `--instance` for a command-line override. Instance
names are project-only configuration: a fixed name in the user-wide file would
collide across projects. An explicit or configured name is permanently
associated with one canonical project:

```console
$ ./bin/bwrap-agent run --instance feature-payments --project ../feature-payments opencode
```

An instance is protected by a fail-fast exclusive lock. If it is already
running, use a separate worktree (recommended) or a different `--instance`.
Agent-native session switching remains available inside one sandbox process.
The launcher holds a host-only advisory lock for the instance; the kernel
releases it automatically when bwrap-agent exits or crashes.

Managed instances live under `~/.local/state/bwrap-agent/instances`. The
host-only instance directory contains `metadata.json` plus the
sandbox-writable `state/` directory. List instances, including their allocated
disk usage and running status, with:

```console
$ ./bin/bwrap-agent instance list
$ ./bin/bwrap-agent instance list --json
```

Delete a stopped instance interactively, or use `--yes` for scripts:

```console
$ ./bin/bwrap-agent instance delete feature-payments
$ ./bin/bwrap-agent instance delete feature-payments --yes
```

Running instances cannot be deleted. Automatic pruning is planned but not
implemented yet. To relocate the complete managed store, set
`BWRAP_AGENT_STATE_HOME`; otherwise the standard `XDG_STATE_HOME` location is
honored. The store must remain separate from projects, external Git metadata,
and explicit read-write binds.

Deletion enters the instance's rootless Podman user namespace when container
storage requires it. If cleanup is interrupted after the instance has been
removed from the listing, repeat the same `instance delete NAME` command to
finish deleting its pending data; no second confirmation is required.

## Networking and ports

The default `private` mode gives every invocation its own network namespace.
Agents can independently bind `localhost:3000` or publish a Podman container
on `localhost:5432` without colliding with another sandbox. Those ports are
initially reachable only from that sandbox.

`--network host` is available for compatibility but loses port isolation and
is unrestricted; combining it with a non-empty allowlist is rejected.
`--network none` disables networking and ignores the allowlist.

### Outbound access

Private mode has no direct outbound interface. Standard `HTTP_PROXY`,
`HTTPS_PROXY`, and lowercase equivalents point to a launcher-owned enforcing
proxy, and the combined allowlist is empty by default, so outside access is
denied. Allow exact or wildcard HTTP/HTTPS origins with configuration or a
repeatable option:

```console
$ ./bin/bwrap-agent run \
    --network-allow https://registry.npmjs.org \
    --network-allow 'https://*.example.com' \
    bash
```

An omitted port means 80 for HTTP or 443 for HTTPS; specify another port
explicitly. `http://*` and `https://*` allow every valid hostname on their
respective default port. A leading `*.` matches subdomains but not the parent
name. Paths, credentials, queries, and fragments are rejected. Loopback,
link-local, unspecified, and multicast destinations are never dialed by the
host-side proxy, including when a broad rule is present.

The proxy accepts ordinary HTTP and HTTPS `CONNECT`. HTTPS remains end-to-end
encrypted, but the initial TLS server name must match the CONNECT destination;
encrypted ClientHello is rejected because its destination cannot be verified.
Software must honor the standard proxy variables in this first version. Direct
sockets and non-HTTP protocols have no outside route. Podman passes the proxy
variables into containers by default, so image pulls, package managers, and
Testcontainers use the same policy.

Ordinary DNS lookup remains available for allowlisted hostnames. A
launcher-owned resolver answers A and AAAA queries only when the name matches
at least one configured origin, and removes loopback, link-local, unspecified,
and multicast answers. DNS for other names is refused without an upstream
lookup. DNS matching ignores origin schemes and ports. Other record types are
unsupported, and resolved addresses still have no direct route from the
sandbox. Permitted wildcard names can still carry data through DNS; use
`--network none` to disable external communication.

Regular rootless Podman networks receive a working `host.containers.internal`
proxy endpoint automatically. A container started with Podman's `--network
host` shares the sandbox network namespace instead; override its proxy URL to
`http://127.0.0.1:65532` if that uncommon mode is needed. This still reaches
the same allowlist-enforcing proxy.

### Publishing sandbox services

Use `--publish [HOST_PORT:]GUEST_PORT[/tcp|udp]` to expose a sandbox service
on host loopback. The default is the same port on both sides and TCP.

Expose a port to the host explicitly:

```console
# Fixed host mapping
$ ./bin/bwrap-agent run --publish 13000:3000 bash

# Ask the launcher to choose an unused host port; the mapping is printed
$ ./bin/bwrap-agent run --publish 0:3000 bash
```

### Accessing host services

Use `--host-port [SANDBOX_PORT:]HOST_PORT[/tcp|udp]` to reach selected host
loopback services from a private sandbox. A single port uses the same number
on both sides; the default protocol is TCP. For example:

```bash
# Sandbox localhost:9222 connects to host localhost:9222.
./bin/bwrap-agent run --host-port 9222 bash

# Sandbox localhost:15432 connects to host localhost:5432.
./bin/bwrap-agent run --host-port 15432:5432 --host-port 18000:8000/udp bash
```

The equivalent user or approved project configuration is:

```toml
host_port = ["9222", "15432:5432", "18000:8000/udp"]
```

Lists append across configuration layers and repeated identical mappings are
removed. Both ports must be between 1 and 65535; automatic allocation, address
arguments, and ranges are unsupported. Conflicting mappings, published guest
ports, sandbox DNS port 53 (TCP/UDP), and proxy port 65532 (TCP) are rejected.
Host services can start after the sandbox. Mappings end with that launch and
appear separately as `host_ports` in dry-run JSON.

These are direct TCP/UDP connections, including WebSockets, independent of
`--network-allow`. All sandbox processes can access the selected services;
unrequested host ports remain unavailable. The default `NO_PROXY` includes
loopback, so use `127.0.0.1` to reach these services directly. Explicit proxy
environment overrides remain your responsibility. Nested-container networking
is unchanged; these examples connect directly from the sandbox.

For Chrome DevTools MCP, run Chrome on the host with a dedicated debugging
profile, forward port 9222, and run the MCP server inside the sandbox with
`--browser-url=http://127.0.0.1:9222`. Install the MCP package beforehand or
allow its package registry through the normal network policy. Chrome's
[connection
guide](https://github.com/ChromeDevTools/chrome-devtools-mcp/blob/main/docs/advanced-usage.md#manual-connection-using-port-forwarding)
explains browser setup. A stdio MCP server running on the host would still
need a network transport bridge; port forwarding does not carry stdio by
itself.

Exposing a service grants its capabilities, including whatever host resources
it can access. A host browser uses its own profile and network access;
requests made through that browser are outside the sandbox's HTTP allowlist.

## Podman and Testcontainers

When Podman is available, the local Podman CLI and Podman Compose work
normally and a sandbox-local API socket is provided automatically. Protected
`write-through` remains the default, so ordinary project and container writes
persist immediately. All containers in an instance are stopped when its agent
exits. Require Podman with `--podman on` or disable the integration with
`--podman off`.

When Podman integration is enabled, the host's `$XDG_CONFIG_HOME/containers`
directory (or `~/.config/containers`) is mounted read-only at the sandbox's
corresponding configuration path and `$HOME/.config/containers` for tools that
use the home path. Podman reads user settings and drop-ins there, including
`registries.conf` and `network.pasta_options` such as `["--ipv4-only"]`. The
entire directory is readable, including any credentials stored there; commands
cannot update these host files from the sandbox. Referenced files outside the
directory are not automatically exposed.

A generated configuration override keeps storage, engine state, volumes, and
network configuration inside the instance, selects local Podman with cgroupfs
and file events, and preserves copy-on-write labeling behavior. Private
networking appends the required proxy environment and pasta loopback mapping
to user settings. `CONTAINERS_CONF`, `CONTAINERS_CONF_OVERRIDE`, and
`CONTAINERS_STORAGE_CONF` are launcher-owned during startup; `--env` and
`--unsetenv` cannot replace these settings. The outer namespace supervisor
uses only generated configuration. If the host directory is absent, Podman
uses defaults plus the sandbox override.

Docker-compatible clients and Testcontainers use the socket automatically. For
example, start a shell with Podman required, then run your test command inside
it. Allow any image and package registries it needs through `network_allow`
first:

```console
$ ./bin/bwrap-agent run --podman on --project . bash
```

The listener exists before the target starts, but `podman system service` is
activated only when a client first connects. The launcher sets `DOCKER_HOST`
and `TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE` inside the sandbox. It
deliberately leaves `CONTAINER_HOST` unset so the Podman CLI retains access to
local-only operations. It never exposes the host Podman socket.

Both root and non-root container processes are supported. Container root
remains rootless with respect to the host. A non-root container can read
ordinary project bind mounts, but writing host-user-owned files requires the
normal Podman mapping, for example:

```console
$ ./bin/bwrap-agent run podman run --user 1000:1000 \
    --userns keep-id:uid=1000,gid=1000 \
    -v "$PWD:/workspace" IMAGE COMMAND
```

The equivalent Compose service can use `user: "1000:1000"` and `userns:
"keep-id:uid=1000,gid=1000"`. This is standard rootless Podman ownership
behavior, not an additional sandbox restriction. A root/non-root Compose
fixture lives at `tests/fixtures/compose-root-and-user.yml`.

Do not bind the host Podman socket into this sandbox. Podman's API is
deliberately equivalent to arbitrary execution as the socket owner, including
host path mounts; a simple socket mount would defeat the filesystem boundary.

## Environment and terminal

The launcher clears the inherited environment. It keeps terminal and locale
settings, but credentials, SSH agent sockets, cloud variables, and tokens are
not forwarded unless explicitly requested with `--env NAME` or an `{ inherit =
true }` config entry. `BWRAP_AGENT_PODMAN` is launcher-owned and always
reports the resolved integration state as `0` or `1`; environment
configuration cannot redefine or remove it.

### Terminal multiplexers

Terminal capability and multiplexer hints are forwarded through a strict
allowlist: `TERM`, `TERMCAP`, terminfo paths, `COLORTERM`, `COLORFGBG`,
`TERM_PROGRAM*`, VTE/Konsole/Kitty/Windows Terminal identifiers, `TMUX`,
`TMUX_PANE`, `STY`, `WINDOW`, ncurses and color-control variables, dimensions,
locale categories, and `OPENTUI_*` overrides. This lets OpenCode distinguish
GNU Screen and tmux while still excluding general host environment variables.

Some Screen setups inherit `COLORTERM=truecolor` from the outer terminal even
though the Screen terminfo only advertises 256 colors. If colors or escape
sequences remain broken, try:

```console
$ ./bin/bwrap-agent run --unsetenv COLORTERM opencode
```

Use `./bin/bwrap-agent run --dry-run opencode` to inspect exactly which
environment values will be exposed.

Interactive launches use a dedicated PTY automatically. The sandbox receives a
real controlling foreground terminal without access to the host terminal
device. Non-interactive pipelines keep separate stdout/stderr and bubblewrap's
`--new-session` isolation. If detection is wrong, force the behavior with
`--tty always` or `--tty never`.

### Clipboard bridge

For terminals such as Ptyxis that do not handle OSC 52 clipboard writes,
install `wl-clipboard` on the **host** and enable the Wayland bridge:

```console
$ ./bin/bwrap-agent run --clipboard wayland opencode
```

To persist the setting, add `clipboard = "wayland"` at the top level of the
user or trusted project configuration. `--clipboard off` overrides it for a
run. The default is `off`. The bridge requires a PTY and a host Wayland
session; missing prerequisites fail launch. `--dry-run --tty always` can
inspect the configuration without copying anything.

OpenCode's copy-on-selection sends text through the terminal output to a host
`wl-copy` process. No Wayland socket or desktop environment variables are
added to the sandbox. Ordinary text pasted through the terminal needs no
bridge. Successful copies remain available after the sandbox exits through
wl-copy's background clipboard owner, until replaced or the desktop session
ends.

Enabling the bridge allows **any sandbox process to replace your clipboard**;
the launcher cannot verify that a write came from a user selection. It
provides no clipboard-reading operation. The host helper and Wayland socket
must be outside project and sandbox-writable paths. Project configuration
remains fully trusted, including its ability to enable this feature.

The bridge accepts regular-clipboard UTF-8 text up to 1 MiB, preserving
newlines. It starts at most five copies per second and coalesces pending
writes to the latest selection. Helper startup times out after five seconds;
failures produce a rate-limited launcher warning without terminating the
agent. OpenCode may still show its own success notification when a clipboard
operation fails.

V1 handles seven-bit OSC 52 with BEL or ST termination. It consumes recognized
clipboard requests, rejecting queries, other selections, invalid data, and
oversized payloads. It does not bridge images, X11, eight-bit OSC, or
multiplexer passthrough wrappers. Other terminal sequences keep their existing
semantics; this is not a general terminal-output security filter. Clipboard
access permitted by the host terminal itself or explicitly mounted desktop
sockets is separate.

## Troubleshooting and upgrades

- **Configuration is unapproved:** review the file on the host and run
  `config trust --project PATH` again. Even a comment edit changes its digest.
  `run --no-project-config` skips that file when diagnosing a problem.
- **An agent or package manager cannot connect:** check that every required
  origin is allowed and that the program uses the HTTP proxy variables. Direct
  external TCP/UDP connections are unavailable in private mode. See
  [outbound access](#outbound-access).
- **A tool cannot find a host file:** the sandbox home is separate from your
  host home. Use an explicit read-only bind for required files and check any
  paths referenced by agent or Git configuration.
- **An instance is already running:** use another worktree or `--instance` name.
  See [persistent instances](#persistent-instances) before deleting state.

Use `run --dry-run PROGRAM` to inspect the effective configuration, mounts,
environment, and port mappings without starting the sandbox. A dry run still
validates configuration approval and launch prerequisites.

### Older instance layouts

When upgrading from a version that created placeholders, stop its active
sessions so they can clean up. Review any remaining files from interrupted
sessions before removing them manually; the new version does not automatically
delete these files.

For older Podman runtime and FUSE-backed stores, see [persistent
instances](#persistent-instances) and [integration
testing](development.md#integration-testing).
