#!/bin/sh
# Run explicitly selected Testcontainers client integrations inside bwrap-agent.
set -eu

binary=${1:-./bin/bwrap-agent}
case "$binary" in
    /*) ;;
    *) binary="$PWD/${binary#./}" ;;
esac

# This explicit target may pull an image; regular integration remains image-free
# unless its caller deliberately sets BWRAP_AGENT_TEST_IMAGE.
: "${BWRAP_AGENT_TEST_IMAGE:=docker.io/library/alpine:3.22}"
export BWRAP_AGENT_TEST_IMAGE

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
fixture_root="$script_dir/fixtures/testcontainers"
selected_languages=${BWRAP_AGENT_TESTCONTAINERS:-all}
test_root=$(mktemp -d /tmp/bwrap-agent-testcontainers.XXXXXX)
cleanup() {
    if command -v podman >/dev/null 2>&1; then
        if ! podman unshare rm -rf -- "$test_root" 2>/dev/null; then
            rm -rf -- "$test_root" 2>/dev/null || echo "warning: could not remove $test_root" >&2
        fi
    else
        rm -rf -- "$test_root" 2>/dev/null || echo "warning: could not remove $test_root" >&2
    fi
}
trap cleanup EXIT HUP INT TERM
export BWRAP_AGENT_STATE_HOME="$test_root/state-home"
export XDG_CONFIG_HOME="$test_root/xdg-config"
mkdir "$XDG_CONFIG_HOME"

is_selected() {
    case ",$selected_languages," in
        *,all,*|*,"$1",*) return 0 ;;
        *) return 1 ;;
    esac
}

run_fixture() {
    language=$1
    command=$2
    runtime_root=${3:-}
    runtime_path=${4:-}
    project="$test_root/$language"
    mkdir "$project"
    cp -R "$fixture_root/$language/." "$project"
    if [ -n "$runtime_root" ]; then
        "$binary" run \
            --project "$project" \
            --instance "integration-testcontainers-$language" \
            --network host \
            --tty never \
            --ro-bind "$runtime_root" \
            --env "PATH=$runtime_path:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" \
            --env "BWRAP_AGENT_TEST_IMAGE=$BWRAP_AGENT_TEST_IMAGE" \
            --env TESTCONTAINERS_RYUK_DISABLED=true \
            /bin/sh -ec "$command"
    else
        "$binary" run \
            --project "$project" \
            --instance "integration-testcontainers-$language" \
            --network host \
            --tty never \
            --env "BWRAP_AGENT_TEST_IMAGE=$BWRAP_AGENT_TEST_IMAGE" \
            --env TESTCONTAINERS_RYUK_DISABLED=true \
            /bin/sh -ec "$command"
    fi
}

skip_or_fail_missing() {
    language=$1
    program=$2
    if [ "$selected_languages" = all ]; then
        echo "testcontainers-$language-skipped: host program $program is unavailable" >&2
        return 0
    fi
    echo "testcontainers-$language requires host program $program" >&2
    return 1
}

for language in $(printf '%s' "$selected_languages" | tr ',' ' '); do
    case "$language" in
        all|go|node|python|java) ;;
        *)
            echo "unknown BWRAP_AGENT_TESTCONTAINERS language: $language" >&2
            exit 2
            ;;
    esac
done

if is_selected go; then
    # The fixture is copied to disposable state, so let Go record its resolved
    # checksums there instead of requiring a repository-maintained go.sum.
    run_fixture go 'command -v go >/dev/null; go run -mod=mod .'
fi
if is_selected node; then
    if npm_path=$(command -v npm 2>/dev/null) && node_path=$(command -v node 2>/dev/null); then
		node_path=$(readlink -f "$node_path")
		node_bin=$(dirname "$node_path")
        npm_real=$(readlink -f "$npm_path")
        runtime_root=$node_bin
        case "$npm_real" in
            "$node_bin"/*) ;;
            *) runtime_root=$(dirname "$node_bin") ;;
        esac
		if [ "$runtime_root" = /usr ]; then
			run_fixture node 'npm install --no-audit --no-fund --ignore-scripts; node index.mjs'
		else
			run_fixture node 'npm install --no-audit --no-fund --ignore-scripts; node index.mjs' "$runtime_root" "$node_bin"
		fi
    else
        skip_or_fail_missing node npm
    fi
fi
if is_selected python; then
    if command -v python3 >/dev/null 2>&1; then
        run_fixture python 'python3 -m venv .venv; .venv/bin/python -m pip install --disable-pip-version-check -r requirements.txt; .venv/bin/python main.py'
    else
        skip_or_fail_missing python python3
    fi
fi
if is_selected java; then
    if command -v mvn >/dev/null 2>&1; then
        run_fixture java 'mvn -q compile exec:java'
    else
        skip_or_fail_missing java mvn
    fi
fi
