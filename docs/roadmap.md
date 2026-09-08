# Roadmap

## Near term

1. Add an environment/feature probe covering user namespaces, subordinate IDs, overlayfs or fuse-overlayfs, pasta, cgroup v2, SELinux, and nested Podman.
2. Run each instance in a transient user systemd scope with CPU, memory, process, and wall-time limits plus reliable `KillMode=mixed` cleanup.
3. Replace the broad `/etc` mount with a generated allowlist and make optional tool roots policy-driven.
4. Add a centrally enforced policy layer, distinct from the convenience TOML config, whose project-local requests can only narrow administrator-defined limits.
5. Add seccomp profiles for kernel attack-surface syscalls not needed by each workload class.

## UX and fleet features

- Extend instance management beyond `instance list` and `instance delete` with `inspect`, component-specific reset, age-based `prune`, rename/rebind, and an optional TUI built on the JSON interface.
- Add broader lifecycle commands such as `doctor`, `stop`, and `shell`.
- A supervisor that reserves ports atomically and reports stable URLs.
- Read-only credential injection through file descriptors or short-lived broker tokens instead of environment variables.
- Shared read-only image and package caches with per-instance writable overlays.
- Extend the OpenCode and Pi adapter registry with config discovery, first-run login, and version checks for more agents while retaining the generic command interface.
- Structured audit events for mounts, published ports, lifecycle, and policy decisions.
- Optional transparent TUN interception, upstream-proxy chaining, and internal CA support for the HTTP(S) allowlist.
