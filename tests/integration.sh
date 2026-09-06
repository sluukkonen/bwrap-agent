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
    --state-dir "$test_root/host" \
    --instance integration-host \
    --podman off \
    --network host \
    --tty never \
    /bin/sh -ec 'test -w "$PWD"; test ! -e "$HOME/.ssh"; printf "host-ok\n"'

"$binary" \
    run \
    --state-dir "$test_root/private" \
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
    --state-dir "$test_root/config-state" \
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

mkdir "$test_root/path-without-podman"
ln -s "$(command -v bwrap)" "$test_root/path-without-podman/bwrap"
PATH="$test_root/path-without-podman" "$binary" \
    run \
    --state-dir "$test_root/auto-without-podman" \
    --instance integration-auto-without-podman \
    --network host \
    --tty never \
    /bin/sh -ec 'test "${BWRAP_AGENT_PODMAN:-}" = 0; test -z "${DOCKER_HOST:-}"; printf "auto-without-podman-ok\n"'

readonly_repository="$test_root/read-only-repository"
readonly_project="$test_root/read-only-worktree"
readonly_state="$readonly_project/.sandbox-state"
git init -q "$readonly_repository"
git -C "$readonly_repository" config user.name "Integration Test"
git -C "$readonly_repository" config user.email "integration@example.invalid"
printf '.sandbox-state/\n' >"$readonly_repository/.gitignore"
printf 'unchanged\n' >"$readonly_repository/tracked"
git -C "$readonly_repository" add .gitignore tracked
git -C "$readonly_repository" commit -qm initial
git -C "$readonly_repository" worktree add -q --detach "$readonly_project" HEAD

"$binary" \
    run \
    --project "$readonly_project" \
    --state-dir "$readonly_state" \
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
test -e "$readonly_state/home/state-probe"

"$binary" \
    run \
    --project "$readonly_project" \
    --state-dir "$readonly_state" \
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
    --state-dir "$test_root/podman" \
    --instance integration-podman \
    --network host \
    --tty never \
    /bin/sh -ec 'test -n "$DOCKER_HOST"; test -z "${CONTAINER_HOST:-}"; test -S "$XDG_RUNTIME_DIR/podman/podman.sock"; ! pgrep -x podman >/dev/null; podman info >/dev/null; ! pgrep -x podman >/dev/null; printf "podman-ok\n"'

"$binary" \
    run \
    --state-dir "$test_root/socket" \
    --instance integration-socket \
    --network host \
    --tty never \
    /bin/sh -ec 'socket=${DOCKER_HOST#unix://}; test -S "$socket"; ! pgrep -x podman >/dev/null; result=$(curl --silent --show-error --unix-socket "$socket" http://d/_ping); test "$result" = OK; pgrep -x podman >/dev/null; printf "socket-ok\n"'

if [ -n "${BWRAP_AGENT_TEST_IMAGE:-}" ]; then
    "$binary" \
        run \
        --state-dir "$test_root/containers" \
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
        --state-dir "$test_root/containers" \
        --instance integration-read-only-container \
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
