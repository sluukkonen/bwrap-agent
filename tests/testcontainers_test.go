package tests

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise runner control flow without package registries, Podman, or toolchains.
func TestTestcontainersRunner(t *testing.T) {
	for _, tc := range []struct {
		name, selection, fail, cold                       string
		wantError, wantLaunch, wantPrivate, noGo, symlink bool
	}{
		{name: "warm", selection: "go", wantLaunch: true, wantPrivate: true},
		{name: "symlink cache", selection: "go", wantLaunch: true, wantPrivate: true, symlink: true},
		{name: "cold", selection: "go", cold: "1", wantLaunch: true, wantPrivate: true},
		{name: "missing Go", selection: "go", noGo: true, wantError: true},
		{name: "cold failure", selection: "go", cold: "1", fail: "pull", wantError: true, wantLaunch: true},
		{name: "prepare failure", selection: "go", fail: "prepare", wantError: true, wantLaunch: true},
		{name: "preflight failure", selection: "go", fail: "pull", wantError: true, wantLaunch: true},
		{name: "client failure", selection: "go", fail: "client", wantError: true, wantLaunch: true},
		{name: "private network failure", selection: "go", fail: "network", wantError: true, wantLaunch: true, wantPrivate: true},
		{name: "unknown", selection: "go,ruby", wantError: true},
		{name: "whitespace", selection: "go node", wantError: true},
		{name: "invalid cold", selection: "go", cold: "yes", wantError: true},
		{name: "empty element", selection: "go,,node", wantError: true},
		{name: "missing explicit tool", selection: "go,java", wantError: true},
		{name: "missing default tools", selection: "all", wantLaunch: true, wantPrivate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			bin := filepath.Join(root, "bin")
			if err := os.Mkdir(bin, 0755); err != nil {
				t.Fatal(err)
			}
			write := func(name, content string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nset -eu\n"+content), 0755); err != nil {
					t.Fatal(err)
				}
			}
			// A controlled PATH makes the missing-tool cases host independent.
			for _, name := range []string{"dirname", "tr", "date", "mktemp", "mkdir", "cp", "rm", "id", "readlink"} {
				path, err := exec.LookPath(name)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path, filepath.Join(bin, name)); err != nil {
					t.Fatal(err)
				}
			}
			write("podman", `
if [ "$1" = unshare ]; then shift; exec "$@"; fi
echo "$1" >> "$PROBE/log"
[ "$1" != "$FAIL_PHASE" ]
`)
			if !tc.noGo {
				write("go", `
[ "$FAIL_PHASE" != prepare ]
echo prepare >> "$PROBE/log"
printf '#!/bin/sh\necho client >> "$PROBE/log"\n[ "$FAIL_PHASE" != client ]\n' > fixture
chmod +x fixture
`)
			}
			// chmod is only needed by the fake compiler.
			chmod, err := exec.LookPath("chmod")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(chmod, filepath.Join(bin, "chmod")); err != nil {
				t.Fatal(err)
			}
			write("launcher", `
[ "$1" = run ]
shift
network=
while [ "$#" -gt 0 ]; do
    case "$1" in
        --no-config) shift ;;
        --project) cd "$2"; shift 2 ;;
        --network) network=$2; shift 2 ;;
        --env) case "$2" in PATH=*) ;; *) export "$2" ;; esac; shift 2 ;;
        --instance|--tty|--podman|--rw-bind|--ro-bind) shift 2 ;;
        --*) echo "unexpected launcher option: $1" >&2; exit 2 ;;
        *) break ;;
    esac
done
echo "launch $network" >> "$PROBE/log"
if [ "${FIXTURE_CACHE+x}" = x ]; then
    printf '%s' "$FIXTURE_CACHE" > "$PROBE/cache-path"
fi
exec "$@"
`)
			cache := filepath.Join(root, "cache with spaces")
			if tc.symlink {
				alias := filepath.Join(root, "alias")
				if err := os.Symlink(root, alias); err != nil {
					t.Fatal(err)
				}
				cache = filepath.Join(alias, "cache with spaces")
			}
			command := exec.Command("/bin/sh", "./testcontainers.sh", filepath.Join(bin, "launcher"))
			command.Env = []string{
				"PATH=" + bin, "PROBE=" + root, "FAIL_PHASE=" + tc.fail,
				"BWRAP_AGENT_TESTCONTAINERS=" + tc.selection,
				"BWRAP_AGENT_TESTCONTAINERS_CACHE=" + cache,
				"BWRAP_AGENT_TESTCONTAINERS_COLD=" + tc.cold,
			}
			output, err := command.CombinedOutput()
			if (err != nil) != tc.wantError {
				t.Fatalf("err=%v, output=%s", err, output)
			}
			log, _ := os.ReadFile(filepath.Join(root, "log"))
			if strings.Contains(string(log), "launch") != tc.wantLaunch {
				t.Fatalf("unexpected log: %s; output: %s", log, output)
			}
			if strings.Contains(string(log), "launch private") != tc.wantPrivate {
				t.Fatalf("unexpected private launch: %s; output: %s", log, output)
			}
			if tc.wantPrivate && !tc.wantError && !strings.Contains(string(output), "testcontainers-go-private-ok") {
				t.Fatalf("private regression did not complete: %s", output)
			}
			if tc.fail == "prepare" && strings.Contains(string(log), "client") {
				t.Fatalf("client ran after failed prepare: %s", log)
			}
			if tc.fail == "pull" && strings.Contains(string(log), "prepare") {
				t.Fatalf("installed after failed preflight: %s", log)
			}
			if tc.wantLaunch {
				path, err := os.ReadFile(filepath.Join(root, "cache-path"))
				if err != nil {
					t.Fatal(err)
				}
				_, statErr := os.Stat(string(path))
				if tc.cold == "1" {
					if !os.IsNotExist(statErr) {
						t.Fatalf("cold cache survived: %s (%v)", path, statErr)
					}
				} else {
					want, err := filepath.EvalSymlinks(filepath.Join(cache, "go"))
					if err != nil {
						t.Fatal(err)
					}
					if statErr != nil || string(path) != want {
						t.Fatalf("persistent cache: %s (%v), want %s", path, statErr, want)
					}
				}
				if !strings.Contains(string(output), "testcontainers-total:") {
					t.Fatalf("missing total: %s", output)
				}
			}
		})
	}
}
