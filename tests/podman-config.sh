#!/bin/sh
# shellcheck disable=SC2016 # Variables in sandbox commands expand inside the sandbox.
# Invoked by integration.sh with its disposable root and instance cleanup.
set -eu
binary=$1
config_root="$2/podman-user-config"
mkdir -p "$config_root/containers/containers.conf.d"
cat >"$config_root/containers/containers.conf" <<'CONF'
[containers]
env = ["KEEP_HOST=kept", "HTTP_PROXY=http://wrong.invalid:1234"]
[network]
pasta_options = ["--ipv6-only"]
network_config_dir = "/unavailable/host-networks"
[engine]
remote = true
active_service = "unavailable-host-service"
static_dir = "/unavailable/host-db"
volume_path = "/unavailable/host-volumes"
tmp_dir = "/unavailable/host-tmp"
cgroup_manager = "systemd"
events_logger = "journald"
CONF
cat >"$config_root/containers/containers.conf.d/90-ipv4.conf" <<'CONF'
[containers]
env = ["KEEP_DROPIN=kept", {append=true}]
[network]
pasta_options = ["--ipv4-only"]
CONF
# If the user storage.conf is loaded, Podman must fail to parse it.
printf 'invalid host storage configuration\n' >"$config_root/containers/storage.conf"
cat >"$config_root/containers/registries.conf" <<'CONF'
unqualified-search-registries = ["inherited-registry.invalid"]
[aliases]
"config-test-image" = "localhost/config-test-selected"
CONF
before=$(find "$config_root" -type f -exec sha256sum {} \; | sort)
for mode in host private none; do
    if [ -n "${BWRAP_AGENT_TEST_IMAGE:-}" ] && [ "$mode" != none ]; then
        # Populate the same instance using host networking; private-mode checks
        # must work without granting registry access through the allowlist.
        XDG_CONFIG_HOME="$config_root" "$binary" run \
            --instance "integration-podman-config-$mode" --network host --podman on --tty never \
            --env "BWRAP_AGENT_TEST_IMAGE=$BWRAP_AGENT_TEST_IMAGE" \
            /bin/sh -ec 'podman pull "$BWRAP_AGENT_TEST_IMAGE"; podman tag "$BWRAP_AGENT_TEST_IMAGE" localhost/config-test-selected:latest'
    fi
    XDG_CONFIG_HOME="$config_root" "$binary" run \
        --instance "integration-podman-config-$mode" --network "$mode" --podman on --tty never \
        --env "CONFIG_TEST_NETWORK=$mode" --env "BWRAP_AGENT_TEST_IMAGE=${BWRAP_AGENT_TEST_IMAGE:-}" \
        /bin/sh -ec '
            test -z "${CONTAINERS_CONF:-}"
            test -r "$CONTAINERS_CONF_OVERRIDE"
            test -r "$CONTAINERS_STORAGE_CONF"
            if (printf changed >"$XDG_CONFIG_HOME/containers/containers.conf") 2>/dev/null; then exit 1; fi
            podman info --format "{{.Registries}}" | grep -q inherited-registry.invalid
            podman info --format "{{.Store.GraphRoot}}" | grep -F "$BWRAP_AGENT_INSTANCE/state/"
            result=$(curl --fail --silent --show-error --unix-socket "${DOCKER_HOST#unix://}" http://d/v5.0.0/libpod/info)
            printf %s "$result" | grep -q inherited-registry.invalid
            case "$CONFIG_TEST_NETWORK" in
                none) exit 0 ;;
            esac
            if [ -z "$BWRAP_AGENT_TEST_IMAGE" ]; then exit 0; fi
            # Short-name resolution exercises user registries.conf; debug output
            # checks the effective pasta argv, not just the generated TOML.
            podman --log-level=debug run --rm --pull=never config-test-image env > /tmp/config-env 2>/tmp/config-debug || {
                cat /tmp/config-debug >&2; exit 1;
            }
            grep -q -- --ipv4-only /tmp/config-debug
            if grep -q -- --ipv6-only /tmp/config-debug; then cat /tmp/config-debug >&2; exit 1; fi
            grep -qx KEEP_HOST=kept /tmp/config-env
            grep -qx KEEP_DROPIN=kept /tmp/config-env
            if [ "$CONFIG_TEST_NETWORK" = private ]; then
                grep -qx HTTP_PROXY=http://host.containers.internal:65532 /tmp/config-env
                grep -q -- --map-host-loopback /tmp/config-debug
            fi
            if podman run --rm --pull=never --privileged -v "$XDG_CONFIG_HOME/containers:/config:rw" config-test-image \
                /bin/sh -ec "printf changed >/config/containers.conf"; then exit 1; fi
        '
    # Reuse the same instance to cover persistent engine paths and moved config.
    XDG_CONFIG_HOME="$config_root" "$binary" run \
        --instance "integration-podman-config-$mode" --network "$mode" --podman on --tty never \
        podman info >/dev/null
    "$binary" instance delete "integration-podman-config-$mode" --yes
done
after=$(find "$config_root" -type f -exec sha256sum {} \; | sort)
test "$before" = "$after"
printf 'podman-user-config-ok\n'
