# Architecture

## Namespace and process order

```text
host user
  `-- podman unshare                 rootless user/mount namespace + subuid map
       `-- pasta --netns-only        one connected network namespace per agent
            `-- bwrap               synthetic filesystem + PID/IPC/UTS/cgroup namespaces
                 `-- Go sandbox init process
                      |-- podman service  lazily activated sandbox-local API
                      `-- agent           OpenCode, Aider, Codex, shell, ...
                           `-- containers nested inside every boundary above
```

The ordering is essential. `bwrap --unshare-all` first creates a one-ID user namespace. Rootless Podman then cannot use `newuidmap` to install the user's subordinate ranges. Entering `podman unshare` first installs the complete mapping; bubblewrap deliberately does **not** create another user namespace. It still owns the mount, PID, IPC, UTS, and cgroup namespace boundaries.

For interactive sessions, the launcher first allocates a dedicated PTY and makes the namespace stack its foreground process group. Bubblewrap then omits `--new-session`, because the proxy PTY—not the user's host TTY—is the controlling terminal. The parent relays input, output, resize events, and signals, restores termios, and disables mouse/focus/paste/alternate-screen modes at exit. Non-interactive commands retain `--new-session` and direct stdout/stderr for clean pipelines.

With `run --podman off`, pasta creates the user and private network namespaces together. In host mode, bubblewrap creates a user namespace automatically when required by an unprivileged caller. `run --network none` asks bubblewrap for a fresh disconnected network namespace. Host mode inherits the host network namespace.

## Configuration resolution

The CLI is parsed into a presence-aware representation first, so an omitted flag is distinguishable from an explicit override. The canonical project is then resolved and TOML layers are loaded from the user config directory and the exact project root. Built-in defaults, user config, project config, and CLI values are merged in that order before instance resolution and launch-plan construction. A fixed `instance` name is accepted only in project configuration because a user-wide literal would collide across canonical projects; `--instance` remains the highest-precedence override. Help and CLI syntax handling happen before config loading, providing a recovery path for malformed files.

Config scalars replace lower values, while mount and publish lists append. Environment values are merged as literal, host-inherit, or unset directives and resolved only after all layers have been applied. Only variables explicitly selected for host inheritance are copied from the original launcher environment. Loaded config paths are retained as launch-plan metadata for dry-run auditing.

## Filesystem view

The root begins as an empty tmpfs. The launcher adds:

| Path | Access | Purpose |
|---|---|---|
| `/usr`, legacy lib/bin paths | read-only | host compilers, runtimes, Podman, shell tools |
| `/etc` | read-only | NSS, TLS, registries, package/tool configuration |
| `/sys`, `/opt`, `/nix/store` when present | read-only | runtime/tool compatibility |
| project at its original absolute path | policy-controlled | source and build outputs |
| managed instance `state/` at its original path | read-write | isolated home, caches, Podman storage |
| external Git common directory | policy-controlled | make linked worktrees functional |
| `/proc`, `/dev` | new virtual filesystems | process and minimal device access; only host `/dev/net/tun` is added for nested pasta |
| `/tmp`, `/var/tmp`, `/run` | private | scratch data and API sockets |

Keeping the project's original absolute path avoids breaking absolute symlinks and build metadata. A linked worktree's `.git` file points outside the worktree, so its common Git directory is mounted too. Before granting that extra exposure, the launcher validates the worktree's `commondir` and backlink. External submodule and separate Git directories require `core.worktree` to resolve back to the selected project. Unrelated or malformed pointers fail closed. The default `workspace` write policy makes validated project and Git paths writable. `state-only` mounts them read-only, rejects explicit read-write binds, and sets `GIT_OPTIONAL_LOCKS=0` for inspection-oriented Git commands.

Instance state is the deliberate writable exception under `state-only`. All state belongs to the managed store; per-run custom state paths are not supported. The store is rejected if either its lexical or canonical path overlaps the project, validated external Git metadata, or a read-write bind. This keeps its host-only metadata and lock outside every agent-writable tree and prevents the writable state mount from covering protected workspace mounts. A future ephemeral mode can supply writable state that is discarded at shutdown.

Agent support is command-oriented rather than hard-coded: `run` requires a program and passes the remainder through unchanged. A registry keyed by executable basename can prepare agent-specific state. The OpenCode adapter copies `XDG_CONFIG_HOME/opencode` and its data-directory `auth.json` into the instance on first use. This gives plugins and OAuth refreshes a writable isolated copy instead of granting write access to the real host configuration. A user-installed executable itself is mounted at a neutral internal path. Agents that need adjacent resource files can expose their installation directory with `run --ro-bind` until a dedicated adapter is added.

## Podman state and socket

Each named instance gets its own storage configuration, graph root, run root, home, and runtime directory. This prevents container/image/name conflicts. Podman integration defaults to `auto`: it is enabled when the executable is available, can be required with `run --podman on`, and can be disabled with `run --podman off`.

An enabled instance creates its API listener before launching the target. The listener uses the short sandbox-private `/run/bwrap-agent/runtime` path, avoiding Unix socket pathname limits regardless of the managed-store or instance-name length; the outer `podman unshare` process separately receives the host-visible instance runtime path. The service itself starts lazily on the first connection by inheriting that listener through the systemd socket-activation descriptor contract. Docker-compatible clients therefore see a stable endpoint without adding a readiness delay to launches that do not use it. `DOCKER_HOST` and the Testcontainers override point to the socket; `CONTAINER_HOST` remains unset so ordinary Podman commands continue to run locally.

Managed instances are stored as `instances/NAME/metadata.json` plus `instances/NAME/state/`. Only `state/` enters the sandbox. Strict, versioned metadata records the canonical project association and creation/last-use timestamps; the containing directory serves as the host-only advisory lock. A short registry lock serializes name allocation and deletion.

The default instance name is a sanitized project-directory basename. Project configuration may replace it, and `--instance` may replace the configured value. If an automatically generated name is already associated with another canonical project, the launcher appends a short hash of the canonical path. A configured or command-line name is never silently rebound to another project. This ownership rule prevents one project from selecting persistent agent or Podman state poisoned by, or containing secrets from, another project. Name resolution, creation, metadata validation, and non-blocking instance-lock acquisition occur while the registry lock is held, so deletion cannot interleave between them. The instance lock descriptor is held by the launcher supervisor for the complete namespace lifetime and is not passed into the sandbox. Separate worktrees naturally receive separate instances unless their shared project configuration fixes the same name; concurrent worktrees can omit the setting or override it on the command line.

`instance list` derives running state by probing the same lock and computes approximate allocated disk usage without following symlinks or double-counting hard links. Files removed concurrently from active caches are ignored. `instance delete` refuses active instances, revalidates metadata under both locks, then atomically renames the directory to a reserved private tombstone before recursively removing it.

At shutdown, the sandbox init mode in the `bwrap-agent` executable stops every container in that instance. This is safe because the store is instance-exclusive. Images and writable storage remain cached for the next run with the same name.

## Port semantics

Private namespaces allow identical guest ports in parallel. Pasta's automatic inbound forwarding is disabled because it would race for the same host ports. `run --publish HOST:GUEST` installs a deliberate mapping; `HOST=0` chooses a currently unused host port. Allocation has a small bind-release-start race that should be removed by a future long-running supervisor.
