#!/bin/sh
# shellcheck disable=SC2016 # Commands expand inside the sandbox.
set -eu

binary=${1:-./bin/bwrap-agent}
case "$binary" in
    /*) ;;
    *) binary="$PWD/${binary#./}" ;;
esac
for program in mvn python3 timeout; do
    command -v "$program" >/dev/null 2>&1 || {
        echo "Maven integration requires $program" >&2
        exit 1
    }
done
script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
test_root=$(mktemp -d /tmp/bwrap-agent-maven.XXXXXX)
trap 'rm -rf -- "$test_root"' EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
# Exercise Java's passwd-home alias as well as HOME.
export BWRAP_AGENT_STATE_HOME="$test_root/state:home"
export XDG_CONFIG_HOME="$test_root/config"
fixture_host_home="$test_root/host-home"
mkdir "$test_root/project" "$XDG_CONFIG_HOME" "$fixture_host_home"
cp "$script_dir/fixtures/maven/"* "$test_root/project/"
# Exercise Maven-compatible input encoding, including multibyte UTF-8 output
# before the proxies element. The original must remain byte-for-byte intact.
python3 - "$test_root/project/settings.xml" <<'PY'
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
content = '<?xml version="1.0" encoding="ISO-8859-1"?>\n<!-- Café -->\n' + path.read_text(encoding="utf-8")
path.write_bytes(content.encode("iso-8859-1"))
PY

run_maven() {
    env HOME="$fixture_host_home" "$binary" run --project "$test_root/project" --instance maven \
        --podman off --tty never "$@"
}

# A first launch without host settings must not prevent later inheritance.
run_maven --network private /bin/sh -ec 'test -r "$HOME/.m2/settings.xml"'
# Keep independent instance settings to verify opt-out and host removal later.
run_maven --network host --no-maven-config /bin/sh -ec 'mkdir -p "$HOME/.m2"; cp settings.xml "$HOME/.m2/settings.xml"'

mkdir "$fixture_host_home/.m2"
cp "$test_root/project/settings.xml" "$fixture_host_home/.m2/settings.xml"

for transport in default wagon; do
    run_maven --network private --network-allow https://repo.maven.apache.org \
        --env FIXTURE_REPOSITORY=https://repo.maven.apache.org/maven2 \
        --env "FIXTURE_TRANSPORT=$transport" /bin/sh -ec '
        test -z "${MAVEN_OPTS:-}${JAVA_TOOL_OPTIONS:-}${JDK_JAVA_OPTIONS:-}"
        cache="$HOME/.m2/allowed-$FIXTURE_TRANSPORT"
        test ! -e "$cache"
        timeout 180 mvn -B -q -Dmaven.repo.local="$cache" \
            -Dmaven.resolver.transport="$FIXTURE_TRANSPORT" \
            org.apache.maven.plugins:maven-dependency-plugin:3.7.0:resolve
        test -s "$cache/junit/junit/4.13.2/junit-4.13.2.jar"
        if (printf overwrite >"$HOME/.m2/settings.xml") 2>/dev/null; then
            echo "generated settings were writable" >&2
            exit 1
        fi
    '
    printf 'maven-%s-allowed-ok\n' "$transport"

    run_maven --network private \
        --env FIXTURE_REPOSITORY=https://repo.maven.apache.org/maven2 \
        --env "FIXTURE_TRANSPORT=$transport" /bin/sh -ec '
        cache="$HOME/.m2/denied-$FIXTURE_TRANSPORT"
        test ! -e "$cache"
        status=0
        timeout 30 mvn -B -q -Dmaven.repo.local="$cache" \
            -Dmaven.resolver.transport="$FIXTURE_TRANSPORT" \
            org.apache.maven.plugins:maven-dependency-plugin:3.7.0:resolve >denied.log 2>&1 || status=$?
        test "$status" -ne 0
        test "$status" -ne 124
        grep -q 403 denied.log
        test ! -e "$cache/junit/junit/4.13.2/junit-4.13.2.jar"
    '
    printf 'maven-%s-denied-ok\n' "$transport"
done

# Add encrypted credentials between launches. Maven must inherit both the
# updated settings and its new security companion, without copying either back.
master=$(mvn -B -q -Dstyle.color=never --encrypt-master-password fixture-master)
printf '<settingsSecurity><master>%s</master></settingsSecurity>\n' "$master" >"$fixture_host_home/.m2/settings-security.xml"
password=$(mvn -B -q -Dstyle.color=never -Duser.home="$fixture_host_home" --encrypt-password fixture-password)
python3 - "$fixture_host_home/.m2/settings.xml" "$password" <<'PYAUTH'
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
content = path.read_text(encoding="iso-8859-1")
servers = "<servers><server><id>fixture</id><username>fixture</username><password>" + sys.argv[2] + "</password></server></servers>"
path.write_bytes(content.replace("</settings>", servers + "</settings>").encode("iso-8859-1"))
PYAUTH
cp "$fixture_host_home/.m2/settings.xml" "$test_root/project/inherited-settings.xml"
cp "$fixture_host_home/.m2/settings-security.xml" "$test_root/project/inherited-security.xml"

# Serve the downloaded artifacts inside the namespace with no external access.
# A proxied localhost request would be rejected by the enforcing proxy.
run_maven --network private --env FIXTURE_REPOSITORY=http://localhost:18080 /bin/sh -ec '
    python3 auth_server.py "$HOME/.m2/allowed-default" >local-server.log 2>&1 &
    server=$!
    trap '\''kill "$server"; wait "$server" 2>/dev/null || :'\'' EXIT
    python3 -c '\''import socket, time
for attempt in range(100):
    try:
        socket.create_connection(("127.0.0.1", 18080), timeout=1).close()
        break
    except OSError:
        time.sleep(0.05)
else:
    raise SystemExit("local repository did not start")'\''
    timeout 120 mvn -B -q -Dmaven.repo.local="$HOME/.m2/local" \
        org.apache.maven.plugins:maven-dependency-plugin:3.7.0:resolve
    test -s "$HOME/.m2/local/junit/junit/4.13.2/junit-4.13.2.jar"
'
printf 'maven-local-bypass-and-encrypted-auth-ok\n'

# The default Podman-enabled launch uses a different user-namespace stack.
# Reuse the verified cache offline to check settings and cache readability.
if command -v podman >/dev/null 2>&1; then
    env HOME="$fixture_host_home" "$binary" run --project "$test_root/project" --instance maven \
        --podman on --network private --tty never \
        --env FIXTURE_REPOSITORY=https://repo.maven.apache.org/maven2 /bin/sh -ec '
        timeout 30 mvn -B -q -o -Dmaven.repo.local="$HOME/.m2/allowed-default" \
            org.apache.maven.plugins:maven-dependency-plugin:3.7.0:resolve
    '
    printf 'maven-podman-settings-ok\n'
else
    printf 'maven-podman-settings-skipped: Podman is unavailable\n'
fi

for mode in host none; do
    run_maven --network "$mode" /bin/sh -ec 'cmp inherited-settings.xml "$HOME/.m2/settings.xml"; cmp inherited-security.xml "$HOME/.m2/settings-security.xml"'
done
printf 'maven-original-settings-ok\n'
cmp "$fixture_host_home/.m2/settings.xml" "$test_root/project/inherited-settings.xml"
cmp "$fixture_host_home/.m2/settings-security.xml" "$test_root/project/inherited-security.xml"
for mode in private host none; do
    run_maven --network "$mode" --no-maven-config /bin/sh -ec '
        if [ -e "$XDG_CONFIG_HOME/maven/settings.xml" ]; then
            if grep -q "<servers>" "$XDG_CONFIG_HOME/maven/settings.xml"; then
                echo "disabled Maven inheritance exposed host servers" >&2
                exit 1
            fi
        fi
        test "$(cat "$HOME/.m2/settings-security.xml")" = "<settingsSecurity/>"
    '
done
run_maven --network none --no-maven-config /bin/sh -ec 'cmp settings.xml "$HOME/.m2/settings.xml"'
rm "$fixture_host_home/.m2/settings.xml" "$fixture_host_home/.m2/settings-security.xml"
run_maven --network none /bin/sh -ec 'cmp settings.xml "$HOME/.m2/settings.xml"'
printf 'maven-host-inheritance-lifecycle-ok\n'
