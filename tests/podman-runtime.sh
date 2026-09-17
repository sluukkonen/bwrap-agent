#!/bin/sh
# shellcheck disable=SC2016 # Variables in sandbox commands expand inside the sandbox.
# Invoked with the integration suite's disposable instance store.
set -eu
binary=$1
test_root=$2
bootstrap_parent="$test_root/bootstrap-runtime"
mkdir -p "$bootstrap_parent"
active_pids=
cleanup_processes() {
    for pid in $active_pids; do kill "$pid" 2>/dev/null || :; done
    for pid in $active_pids; do wait "$pid" 2>/dev/null || :; done
}
trap cleanup_processes EXIT

assert_bootstrap_removed() {
    test -z "$(find "$bootstrap_parent" -mindepth 1 -print -quit)"
}

# A dry run must describe the bootstrap without allocating it, and disabled
# Podman must not need a host runtime directory at all.
TMPDIR="$bootstrap_parent" "$binary" run --instance integration-bootstrap-dry \
    --network host --podman on --tty never --dry-run /bin/true >"$test_root/bootstrap-plan.json"
grep -q podman-bootstrap "$test_root/bootstrap-plan.json"
assert_bootstrap_removed
test ! -e "$BWRAP_AGENT_STATE_HOME/instances/integration-bootstrap-dry/state/run"
TMPDIR="$bootstrap_parent" XDG_RUNTIME_DIR=/unavailable "$binary" run \
    --instance integration-bootstrap-off --network host --podman off --tty never /bin/true
assert_bootstrap_removed

# Databases created with the old HOME/XDG paths must survive the new layout.
legacy_state="$BWRAP_AGENT_STATE_HOME/instances/integration-legacy-home/state"
"$binary" run --instance integration-legacy-home --network host --podman on --tty never \
    --env "HOME=$legacy_state/home" --env "XDG_CONFIG_HOME=$legacy_state/config" \
    --env "XDG_DATA_HOME=$legacy_state/data" --env "XDG_CACHE_HOME=$legacy_state/home/.cache" \
    --env "XDG_STATE_HOME=$legacy_state/home/.local/state" /bin/sh -ec '
        podman volume create legacy-home >/dev/null
        volume=$(podman volume inspect --format "{{.Mountpoint}}" legacy-home)
        printf retained >"$volume/marker"
    '
"$binary" run --instance integration-legacy-home --network host --podman on --tty never /bin/sh -ec '
    volume=$(podman volume inspect --format "{{.Mountpoint}}" legacy-home)
    test "$(cat "$volume/marker")" = retained
'
"$binary" instance delete integration-legacy-home --yes
printf 'podman-legacy-home-ok\n'

for mode in host private none; do
    instance="integration-ephemeral-$mode"
    if [ -n "${BWRAP_AGENT_TEST_IMAGE:-}" ]; then
        TMPDIR="$bootstrap_parent" "$binary" run --instance "$instance" --network host --podman on --tty never \
            --env "BWRAP_AGENT_TEST_IMAGE=$BWRAP_AGENT_TEST_IMAGE" \
            /bin/sh -ec 'podman pull "$BWRAP_AGENT_TEST_IMAGE"'
        assert_bootstrap_removed
    fi
    TMPDIR="$bootstrap_parent" XDG_RUNTIME_DIR=/unavailable "$binary" run \
        --instance "$instance" --network "$mode" --podman on --tty never \
        --env "BWRAP_AGENT_TEST_IMAGE=${BWRAP_AGENT_TEST_IMAGE:-}" \
        --env CONTAINERS_GRAPHROOT=/unavailable --env CONTAINERS_RUNROOT=/unavailable \
        /bin/sh -ec '
            runtime=/run/bwrap-agent/runtime
            test "$(stat -f -c %T "$runtime")" = tmpfs
            test "$(stat -c %a "$runtime")" = 700
            test "${CONTAINERS_GRAPHROOT+x}" != x
            test "${CONTAINERS_RUNROOT+x}" != x
            awk "\$1 == 1 && \$2 == 1 && \$3 > 1 { found = 1 } END { exit !found }" /proc/self/uid_map
            test "$(podman info --format "{{.Store.RunRoot}}")" = "$runtime/containers"
            podman volume create retained >/dev/null
            volume=$(podman volume inspect --format "{{.Mountpoint}}" retained)
            printf retained >"$volume/marker"
            if [ -n "$BWRAP_AGENT_TEST_IMAGE" ]; then
                podman create --pull=never --network host --name retained \
                    -v retained:/data "$BWRAP_AGENT_TEST_IMAGE" cat /data/marker >/dev/null
                test "$(podman start --attach retained)" = retained
            fi
            touch "$runtime/marker"
        '
    assert_bootstrap_removed
    test ! -e "$BWRAP_AGENT_STATE_HOME/instances/$instance/state/run"
    TMPDIR="$bootstrap_parent" "$binary" run \
        --instance "$instance" --network "$mode" --podman on --tty never \
        --env "BWRAP_AGENT_TEST_IMAGE=${BWRAP_AGENT_TEST_IMAGE:-}" --unsetenv XDG_RUNTIME_DIR \
        /bin/sh -ec '
            runtime=/run/bwrap-agent/runtime
            test ! -e "$runtime/marker"
            test "${XDG_RUNTIME_DIR+x}" != x
            test "$(curl --fail --silent --unix-socket "$runtime/podman/podman.sock" http://d/_ping)" = OK
            volume=$(podman volume inspect --format "{{.Mountpoint}}" retained)
            test "$(cat "$volume/marker")" = retained
            if [ -n "$BWRAP_AGENT_TEST_IMAGE" ]; then
                podman image exists "$BWRAP_AGENT_TEST_IMAGE"
                test "$(podman start --attach retained)" = retained
            fi
        '
    assert_bootstrap_removed
    "$binary" instance delete "$instance" --yes
done

# Failure after the bootstrap starts must retain the command status and remove
# all bootstrap scratch files, just like a successful run.
status=0
TMPDIR="$bootstrap_parent" "$binary" run --instance integration-bootstrap-failure \
    --network host --podman on --tty never /bin/sh -c 'exit 17' || status=$?
test "$status" = 17
assert_bootstrap_removed

# An executable with an unavailable interpreter fails at command.Start, after
# bootstrap preparation but before any Podman process can initialize its store.
fake_bin="$test_root/bootstrap-fake-bin"
mkdir "$fake_bin"
printf '#!/nonexistent-bootstrap-interpreter\n' >"$fake_bin/podman"
chmod 755 "$fake_bin/podman"
status=0
TMPDIR="$bootstrap_parent" PATH="$fake_bin:$PATH" "$binary" run \
    --instance integration-bootstrap-start-failure --network host --podman on --tty never /bin/true || status=$?
test "$status" = 127
assert_bootstrap_removed

# Separate active instances need separate bootstrap stores. Interrupting them
# must release their stores without waiting for a later invocation.
for name in one two; do
    project="$test_root/bootstrap-concurrent-$name"
    mkdir "$project"
    TMPDIR="$bootstrap_parent" "$binary" run --project "$project" \
        --instance "integration-bootstrap-$name" --network host --podman on --tty never \
        /bin/sh -ec 'podman info >/dev/null; touch started; exec sleep 60' &
    active_pids="$active_pids $!"
done
attempt=0
while ! test -e "$test_root/bootstrap-concurrent-one/started" || ! test -e "$test_root/bootstrap-concurrent-two/started"; do
    for pid in $active_pids; do kill -0 "$pid"; done
    attempt=$((attempt + 1))
    test "$attempt" -lt 200
    sleep 0.05
done
test "$(find "$bootstrap_parent" -mindepth 1 -maxdepth 1 -type d | wc -l)" -eq 2
for pid in $active_pids; do kill -TERM "$pid"; done
for pid in $active_pids; do
    if wait "$pid"; then
        echo 'interrupted bootstrap launcher unexpectedly succeeded' >&2
        exit 1
    fi
done
active_pids=
assert_bootstrap_removed
printf 'podman-ephemeral-runtime-ok\n'
