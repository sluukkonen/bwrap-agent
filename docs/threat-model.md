# Threat model

## Protected assets

The boundary is intended to protect hidden host files, other agent processes, host process and IPC namespaces, and host network port ownership. The `state-only` policy additionally protects the selected project and its Git metadata from writes. Container bind mounts remain constrained by the same filesystem view.

## Assumptions

- The kernel, bubblewrap, Podman, OCI runtime, and pasta are trusted and patched.
- Unprivileged user namespaces are enabled and the user has non-overlapping `/etc/subuid` and `/etc/subgid` ranges.
- The agent runs as the invoking user, not through setuid or rootful Podman.
- UID 0 displayed inside the launcher or a container is namespaced and maps back to the invoking unprivileged host user.
- Under the default `workspace` write policy, the project itself is untrusted and disposable. A malicious agent can delete or rewrite it.
- The per-instance state is also writable and untrusted. Do not store unrelated secrets there.
- Agent credentials are copied into writable per-instance state. A compromised agent can read or corrupt every credential seeded into its own instance; project ownership prevents that state from being reused by another canonical project.
- Network peers are potentially hostile unless the environment is genuinely air-gapped.

## Deliberate exposures

- Host operating-system files under `/usr`, `/etc`, `/sys`, and optional tool stores are readable. `/etc` can contain operational metadata; deployments needing confidentiality should replace it with an allowlist.
- The default `workspace` policy grants write access to validated external Git metadata, including refs and metadata shared by linked worktrees.
- In `workspace`, conventional Git and supported-agent control surfaces are overlaid read-only, including paths that do not yet exist. This reduces persistence through hooks, configuration, plugins, and tools while leaving ordinary project content writable. `run --allow-control-file-writes` deliberately disables this protection for the run.
- Explicit `run --rw-bind` paths are fully writable and are therefore rejected by `state-only`.
- Project-local `.bwrap-agent.toml` is fully trusted and evaluated by the host launcher before sandbox creation. It can request arbitrary read-only or read-write binds, select host networking, copy explicitly named host environment variables into the sandbox, and select an instance name owned by that canonical project. Cross-project instance reuse is rejected. Use `run --no-project-config` for untrusted checkouts.
- Recognized host agent configuration and executable resources are exposed read-only. Path overrides and automatically discovered sources are rejected if a sandbox-writable path could retarget them, but the configuration itself is trusted and may execute plugins or refer to deliberately exposed resources.
- External Git metadata is exposed only after its worktree backlink or `core.worktree` association has been validated. Malformed and unrelated `.git` pointers abort launch.
- Network access permits exfiltration of project contents. Air-gapping or a controlled proxy is required when confidentiality matters.
- Interactive commands control a launcher-owned proxy PTY, not the host terminal device. Their escape-sequence output is necessarily forwarded to the user's terminal; the launcher restores terminal modes on exit as defense in depth.
- Kernel and namespace escapes remain possible. This is defense in depth, not a VM-strength tenant boundary.

## Why the host Podman socket is excluded

The Podman API grants the socket holder full Podman functionality. An agent could create a container that bind-mounts any path the host Podman service can see, bypassing bubblewrap. Correctly filtering all Podman and Docker-compatible API operations, build contexts, symlinks, image volume behavior, and future endpoints would itself be a security-critical container broker. This prototype avoids that fragile boundary.

## Known gaps in the prototype

- No seccomp policy, Landlock layer, resource quotas, audit log, or verified host-policy file yet.
- `/etc` and `/sys` are broad read-only mounts.
- Automatic host port allocation has a race.
- Concurrent use of one instance is rejected by an exclusive host-side lock; separate agents need separate worktrees or instance names.
- Managed instances can be listed and deleted explicitly, but automatic age-based pruning and component-specific reset are not implemented.
- Control-path protection is path-based. It does not interpret custom Git `core.hooksPath` values, arbitrary external plugin references, hard-link aliases created before launch, or every future agent convention. A repository initialized during a run receives Git-specific protection on its next launch.
- Only current Fedora-like and conventional merged-`/usr` layouts have been exercised; a compatibility test matrix is needed.
- Detached processes are stopped best-effort. A systemd scope with cgroup kill semantics should enforce cleanup.
- Missing control paths require temporary host-side mountpoint inodes while a sandbox is running. Normal exit and handled signals remove the exact inodes. An uncatchable launcher termination can leave neutral, restrictive placeholders temporarily; the project registry lets the next protected launch verify and reuse them, and its last active launcher removes them.
