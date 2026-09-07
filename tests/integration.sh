#!/bin/sh
# shellcheck disable=SC2016 # Variables in sandbox commands expand inside the sandbox.
set -eu

binary=${1:-./bin/bwrap-agent}
case "$binary" in
    /*) ;;
    *) binary="$PWD/${binary#./}" ;;
esac
test_root=$(mktemp -d /tmp/bwrap-agent-integration.XXXXXX)
trap 'rm -rf -- "$test_root"' EXIT HUP INT TERM
BWRAP_AGENT_STATE_HOME="$test_root/state-home"
export BWRAP_AGENT_STATE_HOME

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
BWRAP_AGENT_STATE_HOME="$managed_home" "$binary" instance list --json \
    | grep -q '"name": "managed-project"'
BWRAP_AGENT_STATE_HOME="$managed_home" "$binary" instance list \
    | grep -q 'managed-project.*stopped'
BWRAP_AGENT_STATE_HOME="$managed_home" "$binary" instance delete managed-project --yes
test "$(BWRAP_AGENT_STATE_HOME="$managed_home" "$binary" instance list --json)" = '[]'

"$binary" \
    run \
    --instance integration-host \
    --podman off \
    --network host \
    --tty never \
    /bin/sh -ec 'test -w "$PWD"; test ! -e "$HOME/.ssh"; printf "host-ok\n"'

"$binary" \
    run \
    --instance integration-private \
    --podman off \
    --network private \
    --tty never \
    /bin/sh -ec 'test -r /etc/resolv.conf; printf "private-ok\n"'

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

readonly_repository="$test_root/read-only-repository"
readonly_project="$test_root/read-only-worktree"
git init -q "$readonly_repository"
git -C "$readonly_repository" config user.name "Integration Test"
git -C "$readonly_repository" config user.email "integration@example.invalid"
printf 'unchanged\n' >"$readonly_repository/tracked"
git -C "$readonly_repository" add tracked
git -C "$readonly_repository" commit -qm initial
git -C "$readonly_repository" worktree add -q --detach "$readonly_project" HEAD

"$binary" \
    run \
    --project "$readonly_project" \
    --instance integration-state-only \
    --write-policy state-only \
    --podman off \
    --network host \
    --tty never \
    /bin/sh -ec '
        test "$GIT_OPTIONAL_LOCKS" = 0
        git status --porcelain >/dev/null
        if (printf changed >"$PWD/write-probe") 2>/dev/null; then exit 1; fi
        test "$(cat tracked)" = unchanged
        printf state >"$HOME/state-probe"
        printf temporary >/tmp/temporary-probe
        test "$(cat /tmp/temporary-probe)" = temporary
        printf "state-only-ok\n"
    '
test ! -e "$readonly_project/write-probe"
test -e "$BWRAP_AGENT_STATE_HOME/instances/integration-state-only/state/home/state-probe"

"$binary" \
    run \
    --project "$readonly_project" \
    --instance integration-state-only-podman \
    --write-policy state-only \
    --network host \
    --tty never \
    /bin/sh -ec '
        podman info >/dev/null
        socket=${DOCKER_HOST#unix://}
        test "$(curl --silent --show-error --unix-socket "$socket" http://d/_ping)" = OK
        printf "state-only-podman-ok\n"
    '

"$binary" \
    run \
    --instance integration-podman \
    --network host \
    --tty never \
    /bin/sh -ec 'test -n "$DOCKER_HOST"; test -z "${CONTAINER_HOST:-}"; test -S "$XDG_RUNTIME_DIR/podman/podman.sock"; ! pgrep -x podman >/dev/null; podman info >/dev/null; ! pgrep -x podman >/dev/null; printf "podman-ok\n"'

"$binary" \
    run \
    --instance integration-socket \
    --network host \
    --tty never \
    /bin/sh -ec 'socket=${DOCKER_HOST#unix://}; test -S "$socket"; ! pgrep -x podman >/dev/null; result=$(curl --silent --show-error --unix-socket "$socket" http://d/_ping); test "$result" = OK; pgrep -x podman >/dev/null; printf "socket-ok\n"'

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
        --write-policy state-only \
        --env "BWRAP_AGENT_TEST_IMAGE=$BWRAP_AGENT_TEST_IMAGE" \
        --network host \
        --tty never \
        /bin/sh -ec '
            if podman run --rm --user 0:0 -v "$PWD:/workspace:rw" "$BWRAP_AGENT_TEST_IMAGE" /bin/sh -ec '\''touch /workspace/container-write-probe'\''; then
                exit 1
            fi
            test ! -e "$PWD/container-write-probe"
            printf "container-read-only-ok\n"
        '
fi
