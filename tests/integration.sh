#!/bin/sh
# shellcheck disable=SC2016 # Variables in sandbox commands expand inside the sandbox.
set -eu

binary=${1:-./bin/bwrap-agent}
case "$binary" in
    /*) ;;
    *) binary="$PWD/${binary#./}" ;;
esac
port_server=${2:-./bin/bwrap-agent-integration-server}
case "$port_server" in
    /*) ;;
    *) port_server="$PWD/${port_server#./}" ;;
esac
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
fixture_root="$script_dir/fixtures"
test_root=$(mktemp -d /tmp/bwrap-agent-integration.XXXXXX)
background_pids=
home_state_root=
cleanup() {
    for pid in $background_pids; do
        kill "$pid" 2>/dev/null || :
    done
    for pid in $background_pids; do
        wait "$pid" 2>/dev/null || :
    done
    if command -v podman >/dev/null 2>&1; then
        if ! podman unshare rm -rf -- "$test_root" 2>/dev/null; then
            rm -rf -- "$test_root" 2>/dev/null || echo "warning: could not remove $test_root" >&2
        fi
    else
        rm -rf -- "$test_root" 2>/dev/null || echo "warning: could not remove $test_root" >&2
    fi
    if [ -n "$home_state_root" ]; then
        rm -rf -- "$home_state_root" 2>/dev/null || echo "warning: could not remove $home_state_root" >&2
    fi
}
trap cleanup EXIT HUP INT TERM
BWRAP_AGENT_STATE_HOME="$test_root/state-home"
export BWRAP_AGENT_STATE_HOME
XDG_CONFIG_HOME="$test_root/xdg-config"
export XDG_CONFIG_HOME
mkdir "$XDG_CONFIG_HOME"

config_create_project="$test_root/config-create"
mkdir "$config_create_project"
(
    cd "$config_create_project"
    "$binary" config create
    test -f .bwrap-agent.toml
    if "$binary" config create >/dev/null 2>&1; then
        exit 1
    fi
)

default_project="$test_root/default-project"
mkdir "$default_project"
cd "$default_project"

seccomp_plan=$("$binary" run --instance integration-seccomp-plan --podman off --seccomp required --network host --tty never --dry-run /bin/true)
printf '%s\n' "$seccomp_plan" | grep -q '"effective": "enabled"'
printf '%s\n' "$seccomp_plan" | grep -q '"profile": "development"'
printf '%s\n' "$seccomp_plan" | grep -q -- '--seccomp'
seccomp_off_plan=$("$binary" run --instance integration-seccomp-off --podman off --seccomp off --network host --tty never --dry-run /bin/true)
printf '%s\n' "$seccomp_off_plan" | grep -q '"effective": "off"'
if printf '%s\n' "$seccomp_off_plan" | grep -q -- '--seccomp'; then
    exit 1
fi
"$binary" run --instance integration-seccomp-off-run --podman off --seccomp off --network host --tty never /bin/true

managed_home="$test_root/managed-home"
managed_project="$test_root/managed-project"
mkdir "$managed_project"
BWRAP_AGENT_STATE_HOME="$managed_home" "$binary" \
    run \
    --project "$managed_project" \
    --podman off \
    --network host \
    --tty never \
    /bin/true
mkdir "$managed_home/instances/managed-project/state/inaccessible"
chmod 000 "$managed_home/instances/managed-project/state/inaccessible"
BWRAP_AGENT_STATE_HOME="$managed_home" "$binary" instance list --json \
    | grep -q '"name": "managed-project"'
BWRAP_AGENT_STATE_HOME="$managed_home" "$binary" instance list \
    | grep -q 'managed-project.*stopped'
chmod 700 "$managed_home/instances/managed-project/state/inaccessible"
BWRAP_AGENT_STATE_HOME="$managed_home" "$binary" instance delete managed-project --yes
test "$(BWRAP_AGENT_STATE_HOME="$managed_home" "$binary" instance list --json)" = '[]'

"$binary" \
    run \
    --instance integration-host \
    --podman off \
    --network host \
    --tty never \
    /bin/sh -ec '
        grep -Eq "^Seccomp:[[:space:]]+2$" /proc/self/status
        test -w "$PWD"
        test ! -e "$HOME/.ssh"
        getent passwd "$(id -u)" >/dev/null
        getent group "$(id -g)" >/dev/null
        getent hosts localhost >/dev/null
        getent hosts "$(hostname)" >/dev/null
        test -r /etc/os-release
        test ! -e /etc/shadow
        test ! -e /etc/machine-id
        test ! -e /opt
        test ! -e /nix/store
        printf "host-ok\n"
    '

BWRAP_AGENT_STATE_HOME="$test_root/state:home" "$binary" \
    run \
    --project "$default_project" \
    --instance integration-passwd-home \
    --podman off \
    --network host \
    --tty never \
    /bin/sh -ec '
        passwd_home=$(getent passwd "$(id -u)" | cut -d: -f6)
        test "$passwd_home" = /run/bwrap-agent/passwd-home
        test -d "$passwd_home"
        printf reachable >"$passwd_home/reachability-probe"
        test "$(cat "$HOME/reachability-probe")" = reachable
        printf "passwd-home-alias-ok\n"
    '

mkdir -p "$HOME/.local/state"
home_state_root=$(mktemp -d "$HOME/.local/state/bwrap-agent-integration.XXXXXX")
BWRAP_AGENT_STATE_HOME="$home_state_root" "$binary" \
    run \
    --project "$default_project" \
    --instance integration-private-home-state \
    --podman off \
    --network private \
    --network-allow https://example.com \
    --tty never \
    /bin/true
printf 'private-home-state-ok\n'

equals_binary_dir="$test_root/build=debug"
mkdir "$equals_binary_dir"
cp "$binary" "$equals_binary_dir/bwrap-agent"
"$equals_binary_dir/bwrap-agent" \
    run \
    --project "$default_project" \
    --instance integration-equals-launcher \
    --podman off \
    --network private \
    --network-allow https://example.com \
    --tty never \
    /bin/true
printf 'equals-launcher-ok\n'

"$binary" \
    run \
    --instance integration-private \
    --podman off \
    --network private \
    --tty never \
    /bin/sh -ec '
        test -r /etc/resolv.conf
        grep -q "nameserver 169.254.1.1" /etc/resolv.conf
        getent hosts localhost >/dev/null
        getent hosts "$(hostname)" >/dev/null
        test ! -e /etc/shadow
        test ! -e /etc/machine-id
        printf "private-ok\n"
    '

private_port_one="$test_root/private-port-one"
private_port_two="$test_root/private-port-two"
mkdir "$private_port_one" "$private_port_two"
private_output_one="$test_root/private-port-one.log"
private_output_two="$test_root/private-port-two.log"
"$binary" \
    run --project "$private_port_one" --instance integration-private-port-one --podman off \
    --network private --tty never \
    "$port_server" \
    >"$private_output_one" 2>&1 &
private_pid_one=$!
background_pids="$background_pids $private_pid_one"
"$binary" \
    run --project "$private_port_two" --instance integration-private-port-two --podman off \
    --network private --tty never \
    "$port_server" \
    >"$private_output_two" 2>&1 &
private_pid_two=$!
background_pids="$background_pids $private_pid_two"
attempt=0
while :; do
    if grep -q '^ready$' "$private_output_one" && grep -q '^ready$' "$private_output_two"; then
        break
    fi
    attempt=$((attempt + 1))
    if [ "$attempt" -ge 100 ]; then
        cat "$private_output_one" "$private_output_two" >&2
        exit 1
    fi
    kill -0 "$private_pid_one"
    kill -0 "$private_pid_two"
    sleep 0.05
done
kill "$private_pid_one" "$private_pid_two"
wait "$private_pid_one" 2>/dev/null || :
wait "$private_pid_two" 2>/dev/null || :
background_pids=
printf 'concurrent-private-ports-ok\n'

config_home="$test_root/config-home"
config_project="$test_root/config-project"
mkdir -p "$config_home/bwrap-agent" "$config_project" "$config_home/bwrap-agent/readable"
printf 'from-config-bind\n' >"$config_home/bwrap-agent/readable/value"
printf '%s\n' \
    'network = "host"' \
    'podman = "off"' \
    'tty = "never"' \
    'ro_bind = ["readable"]' \
    '' \
    '[env]' \
    'CONFIG_USER = "user"' \
    'CONFIG_HOST = { inherit = true }' \
    >"$config_home/bwrap-agent/config.toml"
printf '%s\n' \
    '[env]' \
    'CONFIG_USER = "project"' \
    'CONFIG_EMPTY = ""' \
    >"$config_project/.bwrap-agent.toml"

CONFIG_HOST=from-host XDG_CONFIG_HOME="$config_home" "$binary" \
    run \
    --project "$config_project" \
    --instance integration-config \
    --env CONFIG_CLI=cli \
    /bin/sh -ec '
        test "$CONFIG_USER" = project
        test "$CONFIG_HOST" = from-host
        test "$CONFIG_EMPTY" = ""
        test "$CONFIG_CLI" = cli
        test "$(cat "$1/value")" = from-config-bind
        printf "config-ok\n"
    ' sh "$config_home/bwrap-agent/readable"

agent_home="$test_root/agent-home"
agent_config="$test_root/agent-config"
agent_data="$test_root/agent-data"
agent_bins="$test_root/agent-bins"
mkdir -p "$agent_home" "$agent_config/opencode" "$agent_data/opencode" "$agent_bins"
printf 'first\n' >"$agent_config/opencode/live-value"
printf 'host-auth\n' >"$agent_data/opencode/auth.json"
printf '%s\n' \
    '#!/bin/sh' \
    'set -eu' \
    'test "$(cat "$XDG_CONFIG_HOME/opencode/live-value")" = "$1"' \
    'if (printf blocked >"$XDG_CONFIG_HOME/opencode/write-probe") 2>/dev/null; then exit 1; fi' \
    'test -w "$XDG_DATA_HOME/opencode"' \
    'test "$(cat "$XDG_DATA_HOME/opencode/auth.json")" = "$2"' \
    'printf "%s\n" "$3" >"$XDG_DATA_HOME/opencode/auth.json"' \
    >"$agent_bins/opencode"
chmod +x "$agent_bins/opencode"
HOME="$agent_home" XDG_CONFIG_HOME="$agent_config" XDG_DATA_HOME="$agent_data" "$binary" \
    run --project "$config_project" --instance integration-opencode --podman off --network host --tty never \
    "$agent_bins/opencode" first host-auth sandbox-auth
printf 'second\n' >"$agent_config/opencode/live-value"
printf 'changed-host-auth\n' >"$agent_data/opencode/auth.json"
HOME="$agent_home" XDG_CONFIG_HOME="$agent_config" XDG_DATA_HOME="$agent_data" "$binary" \
    run --project "$config_project" --instance integration-opencode --podman off --network host --tty never \
    "$agent_bins/opencode" second sandbox-auth sandbox-auth
test ! -e "$agent_config/opencode/write-probe"
printf 'opencode-agent-ok\n'

protected_project="$test_root/protected-project"
mkdir -p "$protected_project/.opencode/plugins" "$protected_project/.pi/prompts"
printf '{}\n' >"$protected_project/opencode.json"
printf '{}\n' >"$protected_project/.opencode/opencode.json"
printf '{}\n' >"$protected_project/.pi/settings.json"
printf '# trusted host configuration\n' >"$protected_project/.bwrap-agent.toml"
git init -q "$protected_project"
"$binary" \
    run --project "$protected_project" --instance integration-protected --podman off --network host --tty never \
    /bin/sh -ec '
        printf source >ordinary-source
        for directory in .opencode .pi .git
        do
            if mv "$directory" "$directory-moved" 2>/dev/null; then
                echo "unexpectedly renamed protected control directory: $directory" >&2
                exit 1
            fi
        done
        for path in \
            .bwrap-agent.toml opencode.json .opencode/opencode.json \
            .opencode/plugins/new-plugin .opencode/tools/new-tool \
            .pi/settings.json .pi/extensions/new-extension .pi/npm/new-package .pi/git/new-package \
            .git/config .git/hooks/new-hook .git/config.worktree
        do
            if (printf blocked >"$path") 2>/dev/null; then
                echo "unexpectedly wrote protected path: $path" >&2
                exit 1
            fi
        done
        printf editable >.pi/prompts/prompt.md
        printf editable >.mcp.json
        printf "control-paths-ok\n"
    '
test "$(cat "$protected_project/ordinary-source")" = source
test "$(cat "$protected_project/.pi/prompts/prompt.md")" = editable
test "$(cat "$protected_project/.mcp.json")" = editable

concurrent_project="$test_root/concurrent-control-project"
mkdir "$concurrent_project"
"$binary" \
    run --project "$concurrent_project" --instance integration-concurrent-one --podman off --network host --tty never \
    /bin/sh -ec 'printf started >started; while ! test -e release; do sleep 0.02; done' &
first_launcher=$!
while ! test -e "$concurrent_project/started"; do sleep 0.02; done
"$binary" \
    run --project "$concurrent_project" --instance integration-concurrent-two --podman off --network host --tty never \
    /bin/true
test -e "$concurrent_project/.bwrap-agent.toml"
test -e "$concurrent_project/opencode.json"
touch "$concurrent_project/release"
wait "$first_launcher"
test ! -e "$concurrent_project/.bwrap-agent.toml"
test ! -e "$concurrent_project/opencode.json"
test ! -e "$concurrent_project/opencode.jsonc"
test ! -e "$concurrent_project/.opencode"
test ! -e "$concurrent_project/.pi"
printf 'concurrent-control-paths-ok\n'

signal_project="$test_root/signal-control-project"
mkdir "$signal_project"
"$binary" \
    run --project "$signal_project" --instance integration-signal-cleanup --podman off --network host --tty never \
    /bin/sh -ec 'while :; do sleep 1; done' &
signal_launcher=$!
while ! test -e "$signal_project/.bwrap-agent.toml"; do
    kill -0 "$signal_launcher"
    sleep 0.01
done
kill -TERM "$signal_launcher"
if wait "$signal_launcher"; then
    echo 'signal cleanup launcher unexpectedly succeeded' >&2
    exit 1
fi
test ! -e "$signal_project/.bwrap-agent.toml"
test ! -e "$signal_project/opencode.json"
test ! -e "$signal_project/opencode.jsonc"
test ! -e "$signal_project/.opencode"
test ! -e "$signal_project/.pi"
printf 'signal-control-paths-ok\n'

pi_home="$test_root/pi-home"
pi_agent="$pi_home/.pi/agent"
mkdir -p "$pi_agent/extensions" "$agent_bins"
printf 'host-settings\n' >"$pi_agent/settings.json"
printf 'host-pi-auth\n' >"$pi_agent/auth.json"
printf '%s\n' \
    '#!/bin/sh' \
    'set -eu' \
    'test "$(cat "$PI_CODING_AGENT_DIR/settings.json")" = host-settings' \
    'if (printf blocked >"$PI_CODING_AGENT_DIR/settings.json") 2>/dev/null; then exit 1; fi' \
    'test "$(cat "$PI_CODING_AGENT_DIR/auth.json")" = host-pi-auth' \
    'printf sandbox-pi-auth >"$PI_CODING_AGENT_DIR/auth.json"' \
    'printf session >"$PI_CODING_AGENT_SESSION_DIR/session.jsonl"' \
    >"$agent_bins/pi"
chmod +x "$agent_bins/pi"
HOME="$pi_home" "$binary" \
    run --project "$config_project" --instance integration-pi --podman off --network host --tty never \
    "$agent_bins/pi"
test "$(cat "$BWRAP_AGENT_STATE_HOME/instances/integration-pi/state/home/.pi/agent/auth.json")" = sandbox-pi-auth
test -e "$BWRAP_AGENT_STATE_HOME/instances/integration-pi/state/home/.pi/agent/sessions/session.jsonl"
printf 'pi-agent-ok\n'

mkdir "$test_root/path-without-podman"
ln -s "$(command -v bwrap)" "$test_root/path-without-podman/bwrap"
PATH="$test_root/path-without-podman" "$binary" \
    run \
    --instance integration-auto-without-podman \
    --network host \
    --tty never \
    /bin/sh -ec 'test "${BWRAP_AGENT_PODMAN:-}" = 0; test -z "${DOCKER_HOST:-}"; printf "auto-without-podman-ok\n"'

"$binary" \
    run \
    --instance integration-landlock-off-environment \
    --podman off \
    --landlock off \
    --env BWRAP_AGENT_INTERNAL_LANDLOCK_WRITES=invalid \
    --network host \
    --tty never \
    /bin/sh -ec 'test -z "${BWRAP_AGENT_INTERNAL_LANDLOCK_WRITES:-}"; printf "landlock-off-environment-ok\n"'

readonly_repository="$test_root/read-only-repository"
readonly_project="$test_root/read-only-worktree"
git init -q "$readonly_repository"
git -C "$readonly_repository" config user.name "Integration Test"
git -C "$readonly_repository" config user.email "integration@example.invalid"
printf 'unchanged\n' >"$readonly_repository/tracked"
git -C "$readonly_repository" add tracked
git -c commit.gpgSign=false -C "$readonly_repository" commit -qm initial
git -C "$readonly_repository" worktree add -q --detach "$readonly_project" HEAD

"$binary" \
    run \
    --project "$readonly_project" \
    --instance integration-read-only \
    --workspace-mode read-only \
    --podman off \
    --network host \
    --tty never \
    /bin/sh -ec '
        test "$GIT_OPTIONAL_LOCKS" = 0
        test -z "${BWRAP_AGENT_INTERNAL_LANDLOCK_WRITES:-}"
        if unshare --user --map-root-user /bin/true 2>/dev/null; then exit 1; fi
        git status --porcelain >/dev/null
        if (printf changed >"$PWD/write-probe") 2>/dev/null; then exit 1; fi
        test "$(cat tracked)" = unchanged
        printf state >"$HOME/state-probe"
        printf temporary >/tmp/temporary-probe
        test "$(cat /tmp/temporary-probe)" = temporary
        printf "read-only-ok\n"
    '
test ! -e "$readonly_project/write-probe"
test -e "$BWRAP_AGENT_STATE_HOME/instances/integration-read-only/state/home/state-probe"

"$binary" \
    run \
    --project "$readonly_project" \
    --instance integration-read-only-podman \
    --workspace-mode read-only \
    --network host \
    --tty never \
    /bin/sh -ec '
        test "$BWRAP_AGENT_PODMAN" = 0
        test -z "${DOCKER_HOST:-}"
        if podman info >/dev/null 2>&1; then exit 1; fi
        printf "read-only-podman-disabled-ok\n"
    '

"$binary" \
    run \
    --instance integration-podman \
	--unsetenv BWRAP_AGENT_PODMAN \
	--env BWRAP_AGENT_INTERNAL_LANDLOCK_WRITES=invalid \
    --network host \
    --tty never \
	/bin/sh -ec 'grep -Eq "^Seccomp:[[:space:]]+2$" /proc/self/status; test "$BWRAP_AGENT_PODMAN" = 1; test -z "${BWRAP_AGENT_INTERNAL_LANDLOCK_WRITES:-}"; test -n "$DOCKER_HOST"; test -z "${CONTAINER_HOST:-}"; test -S "$XDG_RUNTIME_DIR/podman/podman.sock"; test "$(getent passwd 0 | cut -d: -f6)" = "$HOME"; test ! -e /etc/subuid; test ! -e /etc/subgid; test ! -e /etc/containers/containers.conf; ! pgrep -x podman >/dev/null; podman info >/dev/null; ! pgrep -x podman >/dev/null; printf "podman-ok\n"'
test ! -e "$BWRAP_AGENT_STATE_HOME/instances/integration-podman/state/run/libpod/tmp/pause.pid"

"$binary" \
    run \
    --instance integration-socket \
    --network host \
    --tty never \
    /bin/sh -ec 'socket=${DOCKER_HOST#unix://}; test -S "$socket"; ! pgrep -x podman >/dev/null; result=$(curl --silent --show-error --unix-socket "$socket" http://d/_ping); test "$result" = OK; version=$(curl --silent --show-error --unix-socket "$socket" http://d/version); printf %s "$version" | grep -q '"ApiVersion"'; pgrep -x podman >/dev/null; printf "docker-api-ok\n"'

if [ -n "${BWRAP_AGENT_TEST_IMAGE:-}" ]; then
    "$binary" \
        run \
        --project "$readonly_project" \
        --instance integration-containers \
        --env "BWRAP_AGENT_TEST_IMAGE=$BWRAP_AGENT_TEST_IMAGE" \
        --network host \
        --tty never \
        /bin/sh -ec '
            podman run --rm --user 0:0 "$BWRAP_AGENT_TEST_IMAGE" /bin/sh -ec '\''test "$(id -u)" = 0'\''
            podman run --rm --user 1000:1000 "$BWRAP_AGENT_TEST_IMAGE" /bin/sh -ec '\''test "$(id -u)" = 1000; test "$(id -g)" = 1000'\''
            printf "containers-ok\n"
        '

    "$binary" \
        run \
        --project "$readonly_project" \
        --instance integration-containers \
        --workspace-mode copy-on-write \
        --env "BWRAP_AGENT_TEST_IMAGE=$BWRAP_AGENT_TEST_IMAGE" \
        --network host \
        --tty never \
        /bin/sh -ec '
            podman run --rm --user 0:0 -v "$PWD:/workspace:rw" "$BWRAP_AGENT_TEST_IMAGE" /bin/sh -ec '\''touch /workspace/container-write-probe'\''
            test -e "$PWD/container-write-probe"
            printf "container-copy-on-write-ok\n"
        '
    test ! -e "$readonly_project/container-write-probe"

    protected_config_before=$(cat "$protected_project/.git/config")
    "$binary" \
        run \
        --project "$protected_project" \
        --instance integration-protected-podman \
		--env BWRAP_AGENT_PODMAN=0 \
        --env "BWRAP_AGENT_TEST_IMAGE=$BWRAP_AGENT_TEST_IMAGE" \
        --network host \
        --tty never \
        /bin/sh -ec '
			test "$BWRAP_AGENT_PODMAN" = 1
			if chmod u+w /run/bwrap-agent/init 2>/tmp/init-chmod-error; then exit 1; fi
			if printf replaced >/run/bwrap-agent/init 2>/tmp/init-write-error; then exit 1; fi
			if mv /run/bwrap-agent/init /run/bwrap-agent/init.replaced 2>/tmp/init-rename-error; then exit 1; fi
            printf agent-write-through >ordinary-agent-output
            if (printf direct-change >.git/config) 2>/tmp/direct-write-error; then exit 1; fi
            if umount .git/config 2>/tmp/direct-unmount-error; then exit 1; fi
            mkdir /tmp/ancestor-bind
            if mount --bind "$PWD" /tmp/ancestor-bind 2>/tmp/ancestor-bind-error; then exit 1; fi
            podman run --rm --privileged -v "$PWD:/workspace:rw" "$BWRAP_AGENT_TEST_IMAGE" /bin/sh -ec '\''
                printf container-write-through >/workspace/ordinary-container-output
                if (printf container-change >/workspace/.git/config) 2>/tmp/write-error; then exit 8; fi
                if umount /workspace/.git/config 2>/tmp/unmount-error; then exit 9; fi
            '\''
            test "$(cat .git/config)" != container-change
        '
    test "$(cat "$protected_project/.git/config")" = "$protected_config_before"
    test "$(cat "$protected_project/ordinary-agent-output")" = agent-write-through
    test "$(cat "$protected_project/ordinary-container-output")" = container-write-through

    readonly_exposure="$test_root/readonly-exposure"
    printf 'host-read-only\n' >"$readonly_exposure"
    "$binary" \
        run \
        --project "$readonly_project" \
        --instance integration-readonly-bind-podman \
        --ro-bind "$readonly_exposure" \
        --env "BWRAP_AGENT_TEST_IMAGE=$BWRAP_AGENT_TEST_IMAGE" \
        --network host \
        --tty never \
        /bin/sh -ec '
            if podman run --rm -v "$1:/target:rw" "$BWRAP_AGENT_TEST_IMAGE" /bin/sh -ec '\''printf container-change >/target'\''; then exit 1; fi
            test "$(cat "$1")" = host-read-only
        ' sh "$readonly_exposure"
    test "$(cat "$readonly_exposure")" = host-read-only

    printf '%s\n' \
        '#!/bin/sh' \
        'set -eu' \
		'! podman run --rm -v "$XDG_CONFIG_HOME/opencode:/config:rw" "$BWRAP_AGENT_TEST_IMAGE" /bin/sh -ec "printf container-change >/config/live-value"' \
        'test "$(cat "$XDG_CONFIG_HOME/opencode/live-value")" = host-agent-config' \
        >"$agent_bins/opencode"
    chmod +x "$agent_bins/opencode"
    printf 'host-agent-config\n' >"$agent_config/opencode/live-value"
    HOME="$agent_home" XDG_CONFIG_HOME="$agent_config" XDG_DATA_HOME="$agent_data" "$binary" \
        run --project "$config_project" --instance integration-opencode-podman \
        --env "BWRAP_AGENT_TEST_IMAGE=$BWRAP_AGENT_TEST_IMAGE" --network host --tty never \
        "$agent_bins/opencode"
    test "$(cat "$agent_config/opencode/live-value")" = host-agent-config
    printf 'podman-read-only-exposures-ok\n'

    "$binary" \
        run \
        --project "$readonly_project" \
        --instance integration-containers \
        --env "BWRAP_AGENT_TEST_IMAGE=$BWRAP_AGENT_TEST_IMAGE" \
        --network host \
        --tty never \
        /bin/sh -ec '
            volume=integration-volume
            podman volume create "$volume" >/dev/null
            podman run --rm -v "$volume:/data" "$BWRAP_AGENT_TEST_IMAGE" /bin/sh -ec '\''printf volume-ok >/data/result'\''
            test "$(podman run --rm -v "$volume:/data:ro" "$BWRAP_AGENT_TEST_IMAGE" cat /data/result)" = volume-ok
            podman volume rm "$volume" >/dev/null

            mkdir build-context
            printf "FROM %s\\nRUN printf build-ok >/build-result\\nCMD [\\\"/bin/cat\\\", \\\"/build-result\\\"]\\n" "$BWRAP_AGENT_TEST_IMAGE" >build-context/Containerfile
            podman build --tag bwrap-agent-integration-build build-context >/dev/null
            test "$(podman run --rm bwrap-agent-integration-build)" = build-ok
            podman image rm bwrap-agent-integration-build >/dev/null
            printf "podman-volumes-builds-ok\\n"
        '

    compose_project="$test_root/compose-project"
    mkdir "$compose_project"
    cp "$fixture_root/compose-root-and-user.yml" "$compose_project/compose.yml"
    printf 'compose fixture\n' >"$compose_project/README.md"
    # The non-root container must be able to traverse this disposable bind source.
    chmod 711 "$test_root"
    chmod 755 "$compose_project"
    compose_output=$("$binary" \
        run \
        --project "$compose_project" \
        --instance integration-compose \
        --env "BWRAP_AGENT_TEST_IMAGE=$BWRAP_AGENT_TEST_IMAGE" \
        --network host \
        --tty never \
        podman-compose -f compose.yml up --abort-on-container-exit --exit-code-from root 2>&1)
    printf '%s\n' "$compose_output" | grep -q compose-root-ok
    printf '%s\n' "$compose_output" | grep -q compose-nonroot-ok
    printf 'compose-ok\n'

    unusable_runtime="$test_root/unusable-runtime"
    printf 'not a directory\n' >"$unusable_runtime"
    unusable_xdg="$test_root/unusable-xdg"
    mkdir -p "$unusable_xdg/containers"
    printf 'invalid storage configuration\n' >"$unusable_xdg/containers/storage.conf"
    printf 'invalid containers configuration\n' >"$unusable_xdg/containers/containers.conf"
    container_instance="$BWRAP_AGENT_STATE_HOME/instances/integration-containers"
    interrupted_tombstone="$BWRAP_AGENT_STATE_HOME/instances/.deleting-integration-containers-0123456789abcdef"
    mv "$container_instance" "$interrupted_tombstone"
    mkdir "$interrupted_tombstone/.podman-cleanup-interrupted"
    printf 'stale cleanup state\n' >"$interrupted_tombstone/.podman-cleanup-interrupted/probe"
    delete_output=$(XDG_RUNTIME_DIR="$unusable_runtime" XDG_CONFIG_HOME="$unusable_xdg" \
        CONTAINERS_STORAGE_CONF="$unusable_runtime" CONTAINERS_CONF="$unusable_runtime" \
        STORAGE_DRIVER=invalid STORAGE_OPTS=invalid PODMAN_NO_PAUSE_PROCESS=0 \
        "$binary" instance delete integration-containers --yes)
    printf '%s\n' "$delete_output" | grep -q 'Deleted pending instance data'
    test "$(find "$BWRAP_AGENT_STATE_HOME/instances" -maxdepth 1 -type d -name '.deleting-integration-containers-*' -print -quit)" = ""
    printf 'podman-instance-delete-ok\n'
fi
