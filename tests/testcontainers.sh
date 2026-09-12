#!/bin/sh
# shellcheck disable=SC2016 # Fixture commands expand inside the sandbox.
set -eu

binary=${1:-./bin/bwrap-agent}
case "$binary" in
    /*) ;;
    *) binary="$PWD/${binary#./}" ;;
esac
: "${BWRAP_AGENT_TEST_IMAGE:=docker.io/library/alpine:3.22}"
export BWRAP_AGENT_TEST_IMAGE
script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
fixture_root="$script_dir/fixtures/testcontainers"
selected_languages=${BWRAP_AGENT_TESTCONTAINERS:-all}

is_selected() {
    case ",$selected_languages," in
        *,all,*|*,"$1",*) return 0 ;;
        *) return 1 ;;
    esac
}

# Validate every selection and toolchain before doing expensive work.
case "$selected_languages" in
    ,*|*,|*,,*) echo 'empty Testcontainers language selection' >&2; exit 2 ;;
    *[!a-z,]*) echo "invalid Testcontainers language selection: $selected_languages" >&2; exit 2 ;;
esac
for language in $(printf '%s' "$selected_languages" | tr ',' ' '); do
    case "$language" in
        all|go|node|python|java) ;;
        *) echo "unknown BWRAP_AGENT_TESTCONTAINERS language: $language" >&2; exit 2 ;;
    esac
done
languages=
for language in go node python java; do
    is_selected "$language" || continue
    case "$language" in
        go) programs=go ;;
        node) programs='node npm' ;;
        python) programs=python3 ;;
        java) programs='java javac mvn' ;;
    esac
    missing=
    for program in $programs; do
        if ! command -v "$program" >/dev/null 2>&1; then
            missing=$program
            break
        fi
    done
    if [ -n "$missing" ]; then
        if [ "$selected_languages" = all ]; then
            echo "testcontainers-$language-skipped: host program $missing is unavailable" >&2
            continue
        fi
        echo "testcontainers-$language requires host program $missing" >&2
        exit 1
    fi
    languages="$languages $language"
done
case "${BWRAP_AGENT_TESTCONTAINERS_COLD:-0}" in
    0|1) ;;
    *) echo 'BWRAP_AGENT_TESTCONTAINERS_COLD must be 0 or 1' >&2; exit 2 ;;
esac

started=$(date +%s)
test_root=$(mktemp -d /tmp/bwrap-agent-testcontainers.XXXXXX)
cleanup() {
    status=$?
    trap - EXIT HUP INT TERM
    if command -v podman >/dev/null 2>&1; then
        if ! USER=$(id -un) LOGNAME=$(id -un) PODMAN_NO_PAUSE_PROCESS=1 podman unshare rm -rf -- "$test_root" 2>/dev/null; then
            rm -rf -- "$test_root" 2>/dev/null || echo "warning: could not remove $test_root" >&2
        fi
    else
        rm -rf -- "$test_root" 2>/dev/null || echo "warning: could not remove $test_root" >&2
    fi
    echo "testcontainers-total: $(($(date +%s) - started))s (exit $status)" >&2
    exit "$status"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
export BWRAP_AGENT_STATE_HOME="$test_root/state-home"
export XDG_CONFIG_HOME="$test_root/xdg-config"
mkdir -p "$XDG_CONFIG_HOME/containers"
printf '[network]\npasta_options = ["--ipv4-only"]\n' >"$XDG_CONFIG_HOME/containers/containers.conf"
cache_root=${BWRAP_AGENT_TESTCONTAINERS_CACHE:-$script_dir/../.cache/testcontainers}
if [ "${BWRAP_AGENT_TESTCONTAINERS_COLD:-0}" = 1 ]; then
    cache_root="$test_root/cache"
fi
mkdir -p "$cache_root"
cache_root=$(CDPATH='' cd -- "$cache_root" && pwd -P)

run_fixture() {
    language=$1
    prepare=$2
    execute=$3
    runtime_root=${4:-}
    runtime_path=${5:-}
    project="$test_root/$language"
    cache="$cache_root/$language"
    mkdir "$project"
    mkdir -p "$cache"
    cp -R "$fixture_root/$language/." "$project"
    set -- "$binary" run --project "$project" \
        --instance "integration-testcontainers-$language" --network host --tty never \
        --rw-bind "$cache" --env "FIXTURE_CACHE=$cache" \
        --env "FIXTURE_LANGUAGE=$language" \
        --env "BWRAP_AGENT_TEST_IMAGE=$BWRAP_AGENT_TEST_IMAGE" \
        --env TESTCONTAINERS_RYUK_DISABLED=true
    if [ -n "$runtime_root" ] && [ "$runtime_root" != /usr ]; then
        set -- "$@" --ro-bind "$runtime_root" \
            --env "PATH=$runtime_path:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
    fi
    "$@" /bin/sh -ec '
        phase() {
            name=$1
            shift
            start=$(date +%s)
            echo "testcontainers-$FIXTURE_LANGUAGE-$name: starting" >&2
            status=0
            "$@" || status=$?
            echo "testcontainers-$FIXTURE_LANGUAGE-$name: $(($(date +%s) - start))s (exit $status)" >&2
            return "$status"
        }
        export GOMODCACHE="$FIXTURE_CACHE/mod" GOCACHE="$FIXTURE_CACHE/build"
        export npm_config_cache="$FIXTURE_CACHE/npm" PIP_CACHE_DIR="$FIXTURE_CACHE/pip"
        phase image /bin/sh -ec '\''podman pull "$BWRAP_AGENT_TEST_IMAGE"; podman run --rm --network bridge --pull=never "$BWRAP_AGENT_TEST_IMAGE" /bin/true'\''
        phase prepare /bin/sh -ec "$1"
        phase client /bin/sh -ec "$2"
    ' fixture "$prepare" "$execute"
}

for language in $languages; do
    case "$language" in
        go)
            go_binary=$(readlink -f "$(command -v go)")
            go_bin=$(dirname "$go_binary")
            run_fixture go 'go build -mod=mod -trimpath -o fixture .' './fixture' "$(dirname "$go_bin")" "$go_bin"
            ;;
        node)
            node_path=$(readlink -f "$(command -v node)")
            node_bin=$(dirname "$node_path")
            npm_real=$(readlink -f "$(command -v npm)")
            runtime_root=$node_bin
            case "$npm_real" in
                "$node_bin"/*) ;;
                *) runtime_root=$(dirname "$node_bin") ;;
            esac
            run_fixture node 'npm install --prefer-offline --no-audit --no-fund --ignore-scripts' 'node index.mjs' "$runtime_root" "$node_bin"
            ;;
        python)
            run_fixture python 'python3 -m venv .venv; .venv/bin/python -m pip install --disable-pip-version-check -r requirements.txt' '.venv/bin/python main.py'
            ;;
        java)
            run_fixture java 'mvn -B -q -Dmaven.repo.local="$FIXTURE_CACHE/maven" compile dependency:build-classpath -Dmdep.outputFile=classpath' 'java -cp "target/classes:$(cat classpath)" integration.Main'
            ;;
    esac
done
