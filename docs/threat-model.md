# Threat model

## Protected assets

The boundary is intended to protect hidden host files, other agent processes, host process and IPC namespaces, and host network port ownership. Workspace modes decide whether project and Git changes write through, go to a disposable overlay, or are rejected. With Podman enabled, protected nested mounts are inherited into a less-privileged child user namespace, causing the kernel to lock them together with their ancestors against remounting or detachment by the agent and its containers.

## Assumptions

- The kernel, bubblewrap, Podman, OCI runtime, and pasta are trusted and patched.
- Unprivileged user namespaces are enabled and the user has non-overlapping `/etc/subuid` and `/etc/subgid` ranges.
- The agent runs as the invoking user, not through setuid or rootful Podman.
- UID 0 displayed inside the launcher or a container is namespaced and maps back to the invoking unprivileged host user.
- Under the default `write-through` workspace mode, the project itself is untrusted and disposable. A malicious agent can delete or rewrite it.
- The per-instance state is also writable and untrusted. Do not store unrelated secrets there.
- Agent credentials are copied into writable per-instance state. A compromised agent can read or corrupt every credential seeded into its own instance; project ownership prevents that state from being reused by another canonical project.
- Network peers are potentially hostile unless the environment is genuinely air-gapped.

## Deliberate exposures

- Host operating-system files under `/usr` and `/sys` are readable. The generated `/etc` view deliberately exposes loader configuration, selected Java and Maven runtime configuration, public CA and crypto policy, timezone and operating-system release metadata, plus registry/signature and SELinux/FUSE policy when Podman is enabled. Distribution CA roots linked from the allowlisted paths, including `/var/lib/ca-certificates`, are also exposed read-only. Host account inventories and unrelated system/tool configuration remain hidden. Podman-enabled runs bind the outer rootless namespace's sysfs view writable for privileged-container compatibility, but the agent's child user namespace has no capabilities in the namespace that owns that sysfs mount. Explicit read-only binds may expose additional tool roots or system configuration.
- The default `write-through` mode grants write access to validated external Git metadata, including refs and metadata shared by linked worktrees.
- In `write-through`, conventional Git and supported-agent control surfaces are exposed read-only, including paths that do not yet exist. OpenCode's bootstrap-only `.opencode/.gitignore` writes are redirected to a launcher-owned backing file and cannot change the host project. With Podman enabled, a less-privileged child user/mount namespace kernel-locks the protected mounts to their ancestors, preventing direct and privileged-container mount operations from revealing the underlying controls. `run --allow-control-file-writes` deliberately disables this protection for the run.
- Explicit `run --rw-bind` paths are fully writable exceptions in every workspace mode.
- Project-local `.bwrap-agent.toml` is fully trusted and evaluated by the host launcher before sandbox creation. It can request arbitrary read-only or read-write binds, select host networking, copy explicitly named host environment variables into the sandbox, and select an instance name owned by that canonical project. Cross-project instance reuse is rejected. Use `run --no-project-config` for untrusted checkouts.
- Recognized host agent configuration and executable resources are exposed read-only. Path overrides and automatically discovered sources are rejected if a sandbox-writable path could retarget them, but the configuration itself is trusted and may execute plugins or refer to deliberately exposed resources.
- External Git metadata is exposed only after its worktree backlink or `core.worktree` association has been validated. Malformed and unrelated `.git` pointers abort launch.
- Private networking denies outbound HTTP/HTTPS access by default. Its DNS service resolves A/AAAA queries for any valid hostname and filters protected local answers. Query names can carry data to external DNS servers even with an empty origin allowlist; use `--network none` to disable external communication. Each allowed origin can receive project contents and must be trusted accordingly. Host networking remains unrestricted and permits arbitrary exfiltration.
- Interactive commands control a launcher-owned proxy PTY, not the host terminal device. Their escape-sequence output is necessarily forwarded to the user's terminal; the launcher restores terminal modes on exit as defense in depth.
- The opt-in `--clipboard wayland` bridge grants sandbox processes permission to replace the host's regular clipboard via recognized OSC 52 writes. User selection cannot be authenticated. A host `wl-copy` helper receives only bounded UTF-8 text on stdin, with fixed arguments and a minimal host environment; the desktop socket remains outside the sandbox. The bridge implements no reads and consumes recognized clipboard queries. It is not a comprehensive terminal-output filter: other encodings and passthrough wrappers retain the terminal's existing behavior. Successful background clipboard owners may outlive the sandbox until their selection is replaced. Clipboard managers may also retain history.
- Kernel and namespace escapes remain possible. This is defense in depth, not a VM-strength tenant boundary.

## Why the host Podman socket is excluded

The Podman API grants the socket holder full Podman functionality. An agent could create a container that bind-mounts any path the host Podman service can see, bypassing bubblewrap. Correctly filtering all Podman and Docker-compatible API operations, build contexts, symlinks, image volume behavior, and future endpoints would itself be a security-critical container broker. This prototype avoids that fragile boundary.

## Known gaps in the prototype

- Default seccomp profiles conservatively reduce exposed kernel attack surface, but they are denylist defense in depth rather than a complete syscall allowlist.
- Resource quotas, an audit log, and a verified host-policy file are not implemented yet.
- Bubblewrap 0.11 is a warned compatibility tier affected by an upstream setup-time path-resolution vulnerability. The launcher orders destination creation before untrusted mounts and rejects setuid builds, but Bubblewrap 0.12 or newer is required for the supported security tier.
- Podman-enabled launches require creation of a second user/mount namespace with an identity mapping of the outer rootless namespace. Failure is fatal rather than silently weakening mount protection.
- `copy-on-write` with Podman disables nested per-container SELinux labeling because Bubblewrap's private temporary overlay cannot be relabeled. The outer bwrap-agent process retains its host SELinux confinement.
- `/sys` remains a broad host view. It is read-only without Podman; with Podman it is writable in the outer rootless namespace so privileged containers can initialize, while the sandbox and containers run in a child namespace that lacks capabilities over that mount.
- Automatic host port allocation has a race.
- Concurrent use of one instance is rejected by an exclusive host-side lock; separate agents need separate worktrees or instance names.
- Managed instances can be listed and deleted explicitly, but automatic age-based pruning and component-specific reset are not implemented.
- Control-path protection is path-based. It does not interpret custom Git `core.hooksPath` values, arbitrary external plugin references, hard-link aliases created before launch, or every future agent convention. A repository initialized during a run receives Git-specific protection on its next launch.
- Only current Fedora-like and conventional merged-`/usr` layouts have been exercised; a compatibility test matrix is needed.
- Private-mode clients must honor HTTP proxy environment variables. Transparent interception, upstream-proxy chaining, non-HTTP protocols, and TLS Encrypted ClientHello are not supported.
- HTTPS enforcement verifies the CONNECT authority and visible TLS SNI without decrypting traffic. It cannot inspect the encrypted HTTP `Host`/`:authority`; an explicitly allowed endpoint that supports domain fronting or acts as a relay can therefore carry traffic for another logical origin.
- Detached processes are stopped best-effort. A systemd scope with cgroup kill semantics should enforce cleanup.
- Missing control paths require temporary host-side mountpoint inodes while a sandbox is running. Normal exit and handled signals remove the exact inodes. An uncatchable launcher termination can leave neutral, restrictive placeholders temporarily; the project registry lets the next protected launch verify and reuse them, and its last active launcher removes them.
