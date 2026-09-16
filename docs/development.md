# Development

See the [README](../README.md) for runtime prerequisites.

## Build and test

On Linux, the standard targets run directly on the host. On macOS, they build
and run the development image automatically, mounting the checkout and
persistent Go caches into the container:

```console
$ make build
$ make test
$ make test-race
$ make vet
$ make check
$ make integration
$ make integration-testcontainers
$ make dist
$ make dev-shell
```

`make check` runs the unit tests and vet. `make dev-shell` opens an
interactive, non-root Linux shell with Go and the sandbox runtime dependencies
installed. The development image is available for both amd64 and arm64, so
Docker selects the native architecture on Intel and Apple Silicon Macs. Direct
`go build`, `go test`, and host-side `gopls` package checks remain unsupported
on macOS because the runtime intentionally uses Linux-only APIs.

The first containerized command downloads the development image and Go
modules; later commands reuse `.cache/` in the checkout. If Docker is
unavailable, start Docker Desktop or run `colima start`. `make install`
remains Linux-only because macOS builds produce Linux executables.

## Integration testing

On macOS, the two integration targets use a privileged container with Docker's
seccomp profile disabled and host cgroup access. Some Colima kernels
additionally set `kernel.apparmor_restrict_unprivileged_userns=1`, which
prevents Bubblewrap and pasta from constructing their nested namespaces. The
runner detects that setting and prints the temporary `colima ssh` command
needed to disable it in a dedicated development VM; restart the VM or restore
the value to `1` afterward. Native non-root Linux integration remains the
authoritative security-boundary test. Use the privileged targets only on a
trusted checkout. The regular `build`, `test`, `test-race`, `vet`, `check`,
and `dev-shell` targets stay non-root and do not request those permissions.

Privileged test runs mount a fresh anonymous Docker volume at `/tmp`. This
keeps disposable Podman state on the VM's backing filesystem instead of
Docker's writable overlay layer, allowing native OverlayFS without exposing
`/dev/fuse` inside the sandbox. Docker removes the volume with the container
through `--rm`; dependency caches remain in `.cache/`. On native Linux, place
instance state on a filesystem that supports native OverlayFS. Existing
FUSE-backed instance stores are not automatically migrated; use a fresh state
location rather than deleting or rewriting an existing store.

Set `BWRAP_AGENT_TEST_IMAGE` to an Alpine-compatible image available from the
test environment to include Podman root/non-root execution, volumes, builds,
and Compose in `make integration`. The regular suite also verifies
Docker-compatible API socket activation and concurrent private sandboxes
binding the same guest port.

## Testcontainers clients and caches

`make integration-testcontainers` runs the available Go, Node, Python, and
Java Testcontainers clients against the sandbox-local Podman socket. It
defaults to `docker.io/library/alpine:3.22`; override `BWRAP_AGENT_TEST_IMAGE`
for an internal registry or another Alpine-compatible image. The macOS
development image includes all four toolchains. On native Linux, the default
`all` selection reports and skips client languages whose host toolchain is
absent. An explicit selection such as `BWRAP_AGENT_TESTCONTAINERS=go,python`
fails if either selected toolchain is unavailable. Testcontainers' Ryuk
sidecar is disabled because this launcher already stops all instance
containers at exit.

Each client first pulls and runs the test image, so Podman failures appear
before dependency installation. The runner reports image preparation,
dependency/build preparation, client execution, and total elapsed seconds,
including failed phases. Dependencies and Go build artifacts persist in
`.cache/testcontainers/`; projects, installed environments, and all Podman
state are recreated on every run. Dependency caches reduce downloads and
compilation; package managers may still contact their registries.
Image-registry access is required on every run.

Set `BWRAP_AGENT_TESTCONTAINERS_CACHE` to use another cache directory, or
`BWRAP_AGENT_TESTCONTAINERS_COLD=1` to use a temporary cache that is removed
at exit without changing the persistent cache. On macOS, an overridden path
must be visible and writable inside the development container (for example
`/workspace/.cache/another-cache`). Remove `.cache/testcontainers/` when no
test run is active to reset the default caches. Caches contain executable
build inputs and should only be shared with trusted checkouts.

```console
$ BWRAP_AGENT_TESTCONTAINERS=go,python make integration-testcontainers
$ BWRAP_AGENT_TESTCONTAINERS_COLD=1 make integration-testcontainers
```

The launcher is a CGO-free Go executable. Standard Go modules are used through
`go.mod` and `go.sum`; dependencies are not vendored.
