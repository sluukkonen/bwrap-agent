#!/bin/sh
set -eu

usage() {
    echo "usage: $0 run|privileged|shell [COMMAND ...]" >&2
    exit 2
}

mode=${1:-}
case "$mode" in
    run|privileged|shell) shift ;;
    *) usage ;;
esac

if ! command -v docker >/dev/null 2>&1; then
    echo "bwrap-agent: Docker is required for the macOS development workflow." >&2
    echo "Start Docker Desktop or Colima, then retry this command." >&2
    exit 1
fi
if ! docker info >/dev/null 2>&1; then
    echo "bwrap-agent: the Docker daemon is unavailable." >&2
    echo "Start Docker Desktop or run 'colima start', then retry this command." >&2
    exit 1
fi

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
project_root=$(CDPATH= cd -- "$script_dir/.." && pwd)
developer_uid=$(id -u)
developer_gid=$(id -g)
if [ "$developer_uid" -eq 0 ]; then
    echo "bwrap-agent: the development container must be started by a non-root user." >&2
    exit 1
fi

image=bwrap-agent-dev:local
docker build \
    --build-arg "DEV_UID=$developer_uid" \
    --build-arg "DEV_GID=$developer_gid" \
    --file "$script_dir/Dockerfile.dev" \
    --tag "$image" \
    "$script_dir"

mkdir -p "$project_root/.cache/go-build" "$project_root/.cache/go-mod"

case "$mode" in
    run)
        exec docker run --rm \
            --volume "$project_root:/workspace" \
            --volume "$project_root/.cache/go-build:/home/developer/.cache/go-build" \
            --volume "$project_root/.cache/go-mod:/home/developer/.cache/go-mod" \
            --env BWRAP_AGENT_TEST_IMAGE \
            --env BWRAP_AGENT_TESTCONTAINERS \
            "$image" "$@"
        ;;
    privileged)
        # Integration exercises nested user, mount, PID, network, and cgroup
        # namespaces. This is intentionally broader than the default checks.
        apparmor_restriction=$(docker run --rm "$image" sh -c \
            'cat /proc/sys/kernel/apparmor_restrict_unprivileged_userns 2>/dev/null || true')
        if [ "$apparmor_restriction" = 1 ]; then
            echo "bwrap-agent: this Docker VM restricts unprivileged user namespaces." >&2
            echo "Bubblewrap and pasta integration cannot run under that policy." >&2
            echo "For a dedicated Colima development VM, disable the restriction with:" >&2
            echo "  colima ssh -- sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0" >&2
            echo "Restore it with the same command ending in '=1', or restart the VM." >&2
            exit 1
        fi
        exec docker run --rm \
            --privileged \
            --security-opt seccomp=unconfined \
            --cgroupns host \
            --volume "$project_root:/workspace" \
            --volume "$project_root/.cache/go-build:/home/developer/.cache/go-build" \
            --volume "$project_root/.cache/go-mod:/home/developer/.cache/go-mod" \
            --env BWRAP_AGENT_TEST_IMAGE \
            --env BWRAP_AGENT_TESTCONTAINERS \
            "$image" "$@"
        ;;
    shell)
        exec docker run --rm -it \
            --volume "$project_root:/workspace" \
            --volume "$project_root/.cache/go-build:/home/developer/.cache/go-build" \
            --volume "$project_root/.cache/go-mod:/home/developer/.cache/go-mod" \
            --env BWRAP_AGENT_TEST_IMAGE \
            --env BWRAP_AGENT_TESTCONTAINERS \
            "$image" bash
        ;;
esac
