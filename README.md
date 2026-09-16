# bwrap-agent

Run a coding agent on a project without giving it access to your whole home
directory. `bwrap-agent` uses bubblewrap to run the agent in a sandbox: an
environment with its own home directory and limits on which files and network
services it can reach. The agent can use your installed development tools and
run Podman containers without root privileges.

You can use it to let an agent edit code, run tests, and start development
servers. You can also run a shell or another program; no particular agent is
required.

By default:

- **The agent can edit your project, while your other personal files stay
  hidden.** Apart from the settings and login details described below, files
  in your home directory outside the project are unavailable unless you choose
  to share them. Changes to project files are saved to your computer.
- **Settings and login details carry over.** The agent can read selected agent
  and Git settings from your home directory. For supported agents, login details
  are copied into the sandbox, so you do not need to log in again for every run.
  The agent can read and use those copies.
- **You choose which network services the agent can contact.** It cannot reach
  the internet until you allow the services it needs, such as your AI provider.
- **Podman containers use the sandbox's files and network.** They have their own
  storage, separate from containers you run outside the sandbox.

This project is at an early stage and runs on Linux. Read about its
[security assumptions and limitations](docs/threat-model.md) before using it
with code you do not trust.

## Quick start

### Prerequisites

On Linux, install:

- **bubblewrap 0.12 or newer** (recommended), installed without the setuid bit
  that lets it run with elevated privileges. Version 0.11 also works, but the
  launcher warns that it has weaker security guarantees.
- **pasta**, the network forwarding tool.
- **Go 1.24 or newer** to build from source.
- Your coding agent and the development tools it needs.
- Optionally, **Podman** set up to run containers as your regular user
  ("rootless" mode), for containers and Testcontainers. This requires user and
  group ID ranges assigned in `/etc/subuid` and `/etc/subgid`.

Linux must allow your user to create namespaces without root privileges.
The compiled program runs only on Linux; macOS users can build and test
through the project's
[Linux development container](docs/development.md).

### Build and try a shell

From this checkout:

```sh
make build
./bin/bwrap-agent run --podman off --project . bash
```

With default settings, this opens a shell in the project with a separate home
directory and no internet access. `--podman off` lets you try the sandbox
without setting up containers. Changes to project files are saved to your
computer. Type `exit` to leave the sandbox.

`--project` defaults to the current directory. To work on another project,
pass its directory path. These examples use the program built in this
repository. If you run it from another directory, use its full path.

### Run your agent

Install and configure your agent on your computer first. For OpenCode and Pi,
`bwrap-agent` makes your settings available without letting the agent change
them, and copies saved login details into the project's sandbox on first use.
For other programs, you may need to choose which configuration files and
environment variables to make available. See
[agent and Git configuration](docs/usage.md#agent-and-git-configuration).

Use `--network-allow` to let the agent connect to your AI provider. **Replace
`https://api.example.com` below** with the service address your agent uses.
Include `https://` and the hostname, but no URL path; add a port if needed:

```sh
./bin/bwrap-agent run --network-allow https://api.example.com opencode
```

Repeat the option for other services, such as login servers or package
registries. Network access works through an HTTP proxy, so the program must
support the standard `HTTP_PROXY` and `HTTPS_PROXY` environment variables.
Allowing a package registry does not also allow access to your AI provider.

API keys and other secrets in your computer's environment variables are not
copied automatically. Use `--env NAME` to pass a variable the agent needs
without putting its value in the command line.

Put `bwrap-agent` options before the program name. Everything after the program
name is passed to that program. There is no default agent. Use
`./bin/bwrap-agent run --help` for all options.

## Save your settings

Create a configuration file with explanations and example settings in your
project directory:

```sh
./bin/bwrap-agent config create project
```

Edit `.bwrap-agent.toml` and remove the leading `#` from settings you want to
use. For example, allow your provider's service address:

```toml
network_allow = ["https://api.example.com"]
```

Review the file outside the sandbox, approve it, then launch:

```sh
./bin/bwrap-agent config trust
./bin/bwrap-agent run opencode
```

This file can give the agent access to more files and services on your
computer. Review it before approving it. Every edit, including comments,
requires approval again. `config create` never overwrites an existing file or
approves one for you.

For settings shared across projects, use `config create user`. It creates
`~/.config/bwrap-agent/config.toml` (or the corresponding `XDG_CONFIG_HOME` path).
Every setting in a newly created file starts with `#`, so it has no effect
until you enable it.

Project settings override your user settings, and command-line options
override both. Lists of allowed network services, ports, and additional files
are combined instead of replaced. See the
[configuration guide](docs/usage.md#configuration) for details, including how
to skip a configuration file or remove its approval.

## Common tasks

### Try changes without keeping them

```sh
./bin/bwrap-agent run --workspace-mode copy-on-write bash
```

Changes to the project are thrown away when the sandbox exits. Files in the
sandbox's home directory and caches are still saved. Additional directories
you make writable with `--rw-bind` are still changed directly. Use
`--workspace-mode read-only` to prevent project changes instead. See
[workspace modes](docs/usage.md#workspace-modes-and-protection).

### Connect to a development server

To open a web server running inside the sandbox on port 3000 in your browser:

```sh
./bin/bwrap-agent run --publish 13000:3000 bash
```

Start your server inside that shell, then visit `http://127.0.0.1:13000` in
your browser outside the sandbox. Use `--publish 0:3000` to choose an available
port automatically; the launcher prints the port it chose.

To let the sandbox connect to a service already running on your computer,
such as a database on port 5432:

```sh
./bin/bwrap-agent run --host-port 15432:5432 bash
```

Inside that shell, connect to `127.0.0.1:15432` to reach the database at
`127.0.0.1:5432` outside the sandbox. Both port options work with the default
`--network private` setting. Services running outside the sandbox still have
their usual access to your files and network; `--network-allow` does not limit
what those services can do on the agent's behalf. See
[networking and ports](docs/usage.md#networking-and-ports), including the
[Chrome DevTools example](docs/usage.md#accessing-host-services).

### Reuse or remove an instance

An *instance* is the saved data for a sandbox: its home directory, login
details, caches, and container data. Running the agent again in the same
project reuses that data. Different projects keep their data separate.

Only one run can use an instance at a time. To run more than one session, use
another project directory (such as a Git worktree) or choose another instance
with `--instance NAME`.

```sh
./bin/bwrap-agent instance list
./bin/bwrap-agent instance delete NAME
```

Replace `NAME` with a name from the listing. You must stop an instance before
deleting it, and the command asks for confirmation. Deleting an instance
removes its saved data and leaves your project files in place. See
[persistent instances](docs/usage.md#persistent-instances).

## Further reading

- [Usage guide](docs/usage.md): configuration, networking, Podman, terminals,
  clipboard support, and troubleshooting.
- [Architecture](docs/architecture.md) and [threat model](docs/threat-model.md):
  how isolation works and where its limits are.
- [Development guide](docs/development.md): `make check`, integration tests,
  Testcontainers, and macOS development.
- [Roadmap](docs/roadmap.md): planned work.
