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

# Deletion must recover from permission failures without recognizing a layout.
# Create subordinate-owned private files, then move them into the old store path.
for pending in no yes; do
    instance="integration-old-storage-$pending"
    TMPDIR="$bootstrap_parent" "$binary" run --instance "$instance" \
        --network host --podman on --tty never /bin/sh -ec '
            mkdir -p "$HOME/legacy-storage/storage/locked"
            touch "$HOME/legacy-storage/storage/locked/data"
            chmod 700 "$HOME/legacy-storage/storage/locked"
            podman unshare chown -R 1:1 "$HOME/legacy-storage/storage/locked"
        '
    instance_root="$BWRAP_AGENT_STATE_HOME/instances/$instance"
    mv "$instance_root/state/home/legacy-storage" "$instance_root/state/podman"
    # Remove the empty new storage, so only the permission fallback can help.
    rm -rf "$instance_root/state/home/.local/share/containers/storage"
    if [ "$pending" = yes ]; then
        failing_bin="$test_root/deletion-failing-bin"
        mkdir -p "$failing_bin"
        printf '#!/bin/sh\nexit 1\n' >"$failing_bin/podman"
        chmod 755 "$failing_bin/podman"
        if PATH="$failing_bin:$PATH" "$binary" instance delete "$instance" --yes >"$test_root/deletion-failure.log" 2>&1; then
            echo 'deletion unexpectedly succeeded without rootless cleanup' >&2
            exit 1
        fi
        grep -q 'Podman namespace removal failed' "$test_root/deletion-failure.log"
        pending_root=$(find "$BWRAP_AGENT_STATE_HOME/instances" -maxdepth 1 -name ".deleting-$instance-*" -print)
        test -d "$pending_root"
        test ! -e "$pending_root/metadata.json"
    else
        pending_root="$instance_root"
    fi
    "$binary" instance delete "$instance" --yes
    test ! -e "$pending_root"
    assert_bootstrap_removed
done
printf 'podman-old-storage-delete-ok\n'

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
