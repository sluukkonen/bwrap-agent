# Repository Guidelines

## Project Structure & Module Organization

`bwrap-agent` is a Linux CLI that runs coding agents inside a bubblewrap sandbox. `cmd/bwrap-agent/main.go` is the entry point; `internal/app/` contains configuration, mount planning, networking, instance management, and security enforcement. Unit tests sit beside implementation files as `*_test.go`. `tests/` contains shell integration suites and Go runner tests; `tests/fixtures/` holds helper programs and multi-language Testcontainers examples. Architecture, threat-model, and roadmap documentation live in `docs/`. Generated binaries, distributions, and development caches belong in `bin/`, `dist/`, and `.cache/`.

## Build, Test, and Development Commands

Use Go 1.24 or newer. The application runs only on Linux. macOS development and testing support is provided on a best-effort basis through Docker Desktop or Colima. Keep macOS-specific workarounds in development and test tooling; the main application code must not contain macOS-specific hacks.

- `make build`: build `bin/bwrap-agent`.
- `make check`: run Go tests and `go vet`.
- `make test-race`: run Go tests with the race detector.
- `make integration`: build and run sandbox integration tests.
- `make integration-testcontainers`: exercise Testcontainers clients against sandboxed Podman.
- `make dev-shell`: open the Linux development container shell.
- `make dist`: produce Linux amd64/arm64 binaries and checksums.

On Linux, try `./bin/bwrap-agent run --project . bash`. Integration requires bubblewrap, pasta, and suitable rootless Podman setup; macOS integration targets use privileged containers.

## Coding Style & Naming Conventions

Format Go with `gofmt`, using tabs and conventional Go identifiers. Keep filenames descriptive and snake_case, such as `git_config.go`; preserve architecture suffixes such as `_amd64.go`. Follow existing shell scripts: four-space indentation, quoted expansions, and `set -eu`. Use `go vet` through `make check`; no separate formatter or linter configuration is checked in.

## Testing & Review Guidelines

Use Go's `testing` package, `TestXxx` functions, table-driven subtests, and temporary directories. Add regression coverage for changed behavior, especially rejected configuration, path aliases, and isolation boundaries. No numeric coverage threshold is configured. Run relevant integration suites for sandbox changes. Before completion, inspect the full diff for bugs and missing error paths, check analogous code when finding a bug, fix material issues, run relevant checks, and review again.

## Commit & Pull Request Guidelines

History uses imperative, sentence-case subjects, such as “Inherit user Git configuration inside the sandbox.” Keep commits focused. In PRs, explain the behavior change, rationale, checks performed, and relevant issues. For security-sensitive changes, describe effects on host exposure and update `docs/threat-model.md` or `docs/architecture.md` as needed.
