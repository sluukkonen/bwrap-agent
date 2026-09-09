# Architecture

## Namespace and process order

```text
host user
  `-- podman unshare                 rootless user/mount namespace + subuid map
       `-- pasta --splice-only       loopback-only proxy and published-port forwarding
            `-- bwrap               synthetic filesystem + PID/IPC/UTS/cgroup namespaces
                 `-- Go namespace trampoline
                      `-- locked child user/mount namespace + full subuid map
                           |-- podman service  lazily activated sandbox-local API
                           `-- agent           OpenCode, Pi, shell, ...
                                `-- containers nested inside every boundary above
```

The ordering is essential. Putting `bwrap --unshare-all` first would create a one-ID user namespace, after which rootless Podman could not use `newuidmap` to install the user's subordinate ranges. Entering `podman unshare` first installs the complete mapping; Bubblewrap constructs its mount, PID, IPC, UTS, and cgroup namespace boundaries there. Its init then identity-maps the complete available UID/GID range into a child user/mount namespace. Because the mount hierarchy crosses into a less-privileged user namespace, Linux locks its inherited mounts together and locks their read-only flags. The agent retains the mappings needed by Podman but cannot separate a protected child mount from its writable ancestor.

For interactive sessions, the launcher first allocates a dedicated PTY and makes the namespace stack its foreground process group. Bubblewrap then omits `--new-session`, because the proxy PTY—not the user's host TTY—is the controlling terminal. The parent relays input, output, resize events, and signals, restores termios, and disables mouse/focus/paste/alternate-screen modes at exit. Non-interactive commands retain `--new-session` and direct stdout/stderr for clean pipelines.

With `run --podman off`, pasta creates the user and private network namespaces together. In host mode, bubblewrap creates a user namespace automatically when required by an unprivileged caller. `run --network none` asks bubblewrap for a fresh disconnected network namespace. Host mode inherits the host network namespace.

## Configuration resolution

The CLI is parsed into a presence-aware representation first, so an omitted flag is distinguishable from an explicit override. The canonical project is then resolved and TOML layers are loaded from the user config directory and the exact project root. Built-in defaults, user config, project config, and CLI values are merged in that order before instance resolution and launch-plan construction. A fixed `instance` name is accepted only in project configuration because a user-wide literal would collide across canonical projects; `--instance` remains the highest-precedence override. Help and CLI syntax handling happen before config loading, providing a recovery path for malformed files.

Config scalars replace lower values, while network-allow, mount, and publish lists append. Network origins are normalized and deduplicated. Environment values are merged as literal, host-inherit, or unset directives and resolved only after all layers have been applied. Only variables explicitly selected for host inheritance are copied from the original launcher environment. Loaded config paths are retained as launch-plan metadata for dry-run auditing.

## Filesystem view

The root begins as an empty tmpfs. The launcher adds:

| Path | Access | Purpose |
|---|---|---|
| `/usr`, legacy lib/bin paths | read-only | host compilers, runtimes, Podman, shell tools |
| `/etc` | read-only | NSS, TLS, registries, package/tool configuration |
| `/sys`, `/opt`, `/nix/store` when present | policy-controlled | runtime/tool compatibility; Podman uses the outer rootless sysfs view |
| project at its original absolute path | workspace-mode controlled | source and build outputs |
| managed instance `state/` at its original path | read-write | isolated home, caches, Podman storage |
| external Git common directory | policy-controlled | make linked worktrees functional |
| `/proc`, `/dev` | new virtual filesystems | process and minimal device access; only host `/dev/net/tun` is added for nested pasta |
| `/tmp`, `/var/tmp`, `/run` | private | scratch data and API sockets |

Keeping the project's original absolute path avoids breaking absolute symlinks and build metadata. A linked worktree's `.git` file points outside the worktree, so its common Git directory is mounted too. Before granting that extra exposure, the launcher validates the worktree's `commondir` and backlink. External submodule and separate Git directories require `core.worktree` to resolve back to the selected project. Unrelated or malformed pointers fail closed. `write-through` binds validated project and Git paths writable, `copy-on-write` uses invisible temporary OverlayFS upper layers, and `read-only` binds them read-only and sets `GIT_OPTIONAL_LOCKS=0`.

Under `write-through`, a final layer overrides the writable project and Git layers for conventional high-impact control paths. It covers bwrap-agent project configuration; Git config, hooks, and per-worktree config; OpenCode project config, plugins, and tools; and Pi settings, extensions, and package locations. When Podman is enabled, the child namespace makes those mounts a kernel-locked unit with their ancestors. Direct agent mount operations, non-recursive ancestor binds, and privileged nested containers cannot detach the masks or change their read-only flags. Missing entries retain the identity-tracked placeholder and project-wide lifetime-lock design. `copy-on-write` already prevents persistence for the complete workspace and therefore does not add redundant nested control mounts; `read-only` makes the complete workspace immutable.

The sandbox init executable is passed to Bubblewrap through an inherited read-only file descriptor, copied onto private executable-safe backing, and bind-mounted read-only at `/run/bwrap-agent/init`. It therefore neither inherits `noexec` from persistent state nor becomes mutable inside the sandbox. Packaged executable resources use the same mechanism.

Protection applies regardless of the target executable. `run --allow-control-file-writes` is a CLI-only, all-or-nothing escape hatch; it also permits symlinked project configuration and emits a warning in `write-through`. `run --dry-run` reports the effective `protected_paths`, workspace mode, Bubblewrap tier, and Landlock status.

Instance state remains writable and persistent independently of workspace mode. All state belongs to the managed store; per-run custom state paths are not supported. The store is rejected if either its lexical or canonical path overlaps the project, validated external Git metadata, or a read-write bind. A future state-lifetime option can make this state disposable.

When Podman is disabled, Landlock ABI 3 or newer can enforce the generated write allowlist after Bubblewrap completes its mount layout. State, private temporary/runtime paths, explicit read-write binds, and writable workspace views are allowed; everything else is denied mutation. Further user namespaces are disabled. Podman is incompatible with required Landlock because container setup needs mount and pivot operations; automatic Landlock reports that it was skipped when Podman is enabled.

Bubblewrap 0.11 is accepted only as a non-setuid compatibility tier and produces a warning. All trusted destination directories are emitted before project or state mounts so setup never deliberately creates parent directories through an already-mounted untrusted tree. Bubblewrap 0.12 or newer remains the supported security tier because it fixes upstream path-resolution behavior that the launcher cannot fully replace.

Agent support is command-oriented rather than hard-coded: `run` requires a program and passes the remainder through unchanged. An internal registry keyed by executable basename lets an adapter contribute read-only mounts, sandbox environment values, seed-once mutable files, and an optional packaged-command mount. Auto-discovered sources are canonicalized and rejected when their lexical or resolved paths overlap the project, instance state, external Git metadata, or explicit writable binds, preventing a prior sandbox run from retargeting a source toward hidden host data.

The OpenCode adapter mounts `XDG_CONFIG_HOME/opencode` and documented path-based config overrides read-only. Its writable XDG data, sessions, logs, and caches remain in the instance; only `auth.json` is seeded from the host once. The Pi adapter constructs an instance-local `PI_CODING_AGENT_DIR`, mounts host-owned top-level configuration and resources into it read-only, seeds authentication, trust, and model-catalog state once, and keeps sessions instance-local. Pi's `~/.agents/skills` discovery is provided as a read-only mount. For npm-installed Pi, the package root and host Node executable are mounted at neutral paths so installations from user-level runtime managers retain their JavaScript entrypoint and runtime. Standalone and ordinary system executables retain the generic command path.

## Podman state and socket

Each named instance gets its own storage configuration, graph root, run root, home, and runtime directory. This prevents container/image/name conflicts. Podman integration defaults to `auto`: it is enabled when the executable is available, can be required with `run --podman on`, and can be disabled with `run --podman off`. Read-only workspaces and required Landlock enforcement remain incompatible with Podman.

An enabled instance creates its API listener before launching the target. The listener uses the short sandbox-private `/run/bwrap-agent/runtime` path, avoiding Unix socket pathname limits regardless of the managed-store or instance-name length; the outer `podman unshare` process separately receives the host-visible instance runtime path. The service itself starts lazily on the first connection by inheriting that listener through the systemd socket-activation descriptor contract. Docker-compatible clients therefore see a stable endpoint without adding a readiness delay to launches that do not use it. `DOCKER_HOST` and the Testcontainers override point to the socket; `CONTAINER_HOST` remains unset so ordinary Podman commands continue to run locally.

Managed instances are stored as `instances/NAME/metadata.json` plus `instances/NAME/state/`. Only `state/` enters the sandbox. Strict, versioned metadata records the canonical project association and creation/last-use timestamps; the containing directory serves as the host-only advisory lock. A short registry lock serializes name allocation and deletion.

The default instance name is a sanitized project-directory basename. Project configuration may replace it, and `--instance` may replace the configured value. If an automatically generated name is already associated with another canonical project, the launcher appends a short hash of the canonical path. A configured or command-line name is never silently rebound to another project. This ownership rule prevents one project from selecting persistent agent or Podman state poisoned by, or containing secrets from, another project. Name resolution, creation, metadata validation, and non-blocking instance-lock acquisition occur while the registry lock is held, so deletion cannot interleave between them. The instance lock descriptor is held by the launcher supervisor for the complete namespace lifetime and is not passed into the sandbox. Separate worktrees naturally receive separate instances unless their shared project configuration fixes the same name; concurrent worktrees can omit the setting or override it on the command line.

`instance list` derives running state by probing the same lock and computes approximate allocated disk usage without following symlinks or double-counting hard links. Files removed concurrently from active caches and permission-inaccessible subtrees such as OverlayFS work directories are omitted rather than breaking the complete listing. `instance delete` refuses active instances, revalidates metadata under both locks, then atomically renames the directory to a reserved private tombstone. Tombstones containing rootless container storage are removed through `podman unshare` with launcher-owned runtime, storage, and containers configuration kept inside the tombstone; ordinary state uses direct removal. The launcher and cleanup namespace disable Podman's persistent pause process because their foreground supervisors already keep the required namespace alive. Deletion also retires a legacy pause process only after a pidfd and its exact pre-rename environment identify it as belonging to that instance. A repeated delete retries an already-confirmed tombstone and reclaims temporary cleanup state left by an interrupted or failed cleanup.

At shutdown, the sandbox init mode in the `bwrap-agent` executable stops every container in that instance. This is safe because the store is instance-exclusive. Images and writable storage remain cached for the next run with the same name.

## Port semantics

Private mode runs pasta in splice-only mode: the namespace has loopback but no routable interface. One namespace-to-host TCP mapping reaches a Go HTTP/CONNECT proxy bound to a random host-loopback port; the runtime port is substituted only after the listener is reserved. The sandbox uses a loopback proxy URL, while ordinary nested Podman networks receive a `host.containers.internal` URL backed by a dedicated nested-pasta host-loopback mapping. A nested container using Podman's `--network host` must instead point its proxy variables to the sandbox's `127.0.0.1:65532`. Direct sockets have no route. The proxy normalizes destinations, enforces the merged origin policy, rejects protected host-local address classes, pins each dial to a checked DNS result, forces HTTP's upstream Host to the allowed authority, and verifies that CONNECT begins with TLS whose visible SNI matches that authority. Unknown, hidden-ECH, and non-HTTP protocols fail closed.

Private namespaces allow identical guest ports in parallel. Pasta's automatic inbound forwarding is disabled because it would race for the same host ports. `run --publish HOST:GUEST` installs a deliberate loopback mapping; `HOST=0` chooses a currently unused host port. Allocation has a small bind-release-start race that should be removed by a future long-running supervisor.
