# Roadmap

## Near term

1. Add an environment/feature probe covering user namespaces, subordinate IDs, overlayfs or fuse-overlayfs, pasta, cgroup v2, SELinux, and nested Podman.
2. Run each instance in a transient user systemd scope with CPU, memory, process, and wall-time limits plus reliable `KillMode=mixed` cleanup.
3. Replace the broad `/etc` mount with a generated allowlist and make optional tool roots policy-driven.
4. Add a centrally enforced policy layer, distinct from the convenience TOML config, whose project-local requests can only narrow administrator-defined limits.
5. Add seccomp and Landlock defense-in-depth profiles, including explicit denial of mount-related and kernel attack-surface syscalls not needed by nested Podman.
6. Add integration tests for Podman CLI, Compose, Docker-compatible API, Java/Go/Node/Python Testcontainers, volumes, builds, and concurrent identical ports.

## UX and fleet features

- Extend instance management beyond `instance list` and `instance delete` with `inspect`, component-specific reset, age-based `prune`, rename/rebind, and an optional TUI built on the JSON interface.
- Add broader lifecycle commands such as `doctor`, `stop`, and `shell`.
- A supervisor that reserves ports atomically and reports stable URLs.
- Read-only credential injection through file descriptors or short-lived broker tokens instead of environment variables.
- Shared read-only image and package caches with per-instance writable overlays.
- Agent adapters for config discovery, first-run login, and version checks while retaining the generic command interface.
- Structured audit events for mounts, published ports, lifecycle, and policy decisions.
- An HTTP(S)-only proxy mode with destination allowlists and internal CA support.
