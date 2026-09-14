package app

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrivateFilesLifecycle(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	opts := Options{Project: t.TempDir(), Instance: "private-files", Network: "none", Podman: "off", TTY: "never", Command: []string{"/bin/true"}}
	plan, err := BuildPlan(opts)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(filepath.Dir(plan.State), "generated")
	resolver := filepath.Join(root, "etc/resolv.conf")
	for relative, mode := range map[string]os.FileMode{".": 0700, "etc": 0700, "etc/resolv.conf": 0444, "executables": 0700, "executables/init": 0555} {
		info, err := os.Stat(filepath.Join(root, relative))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != mode {
			t.Fatalf("%s permissions = %o", relative, info.Mode().Perm())
		}
	}
	joined := strings.Join(plan.Bwrap, "\x00")
	if strings.Contains(joined, "--ro-bind-data") || strings.Contains(joined, "/proc/self/fd") || !strings.Contains(joined, "--ro-bind\x00"+resolver+"\x00/etc/resolv.conf") {
		t.Fatalf("unexpected mounts: %v", plan.Bwrap)
	}
	before, err := os.Stat(resolver)
	if err != nil {
		t.Fatal(err)
	}
	identity, lock, err := resolveAndLockInstance(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := BuildPlan(opts); err == nil {
		t.Fatal("refreshed a locked instance")
	}
	after, err := os.Stat(resolver)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("locked backing changed: %v", err)
	}
	lock.Close()
	if _, err := writeStateFile(root, "maven/settings-security.xml", []byte("stale secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := writeStateFile(identity.State, "config/etc/resolv.conf", []byte("legacy"), 0600); err != nil {
		t.Fatal(err)
	}
	userSettings, err := writeStateFile(identity.State, "home/.m2/settings.xml", []byte("<settings/>"), 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := BuildPlan(opts); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(root, "maven/settings-security.xml"), filepath.Join(identity.State, "config/etc/resolv.conf")} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stale file remains: %s: %v", path, err)
		}
	}
	if content, err := os.ReadFile(userSettings); err != nil || string(content) != "<settings/>" {
		t.Fatalf("user settings changed: %q, %v", content, err)
	}
	opts.Command = []string{"/nonexistent/private-files-test"}
	if _, err := BuildPlan(opts); err == nil {
		t.Fatal("invalid command succeeded")
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed preparation retained files: %v, %v", entries, err)
	}
	if err := deleteInstance(identity.Instance, true, false, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted instance retained private directory: %v", err)
	}
}

func TestPrivateFilesRejectRootAliasAndUnlinkChildAliases(t *testing.T) {
	root := t.TempDir()
	identity := instanceIdentity{Root: root, State: filepath.Join(root, "state")}
	if err := os.Mkdir(identity.State, 0700); err != nil {
		t.Fatal(err)
	}
	victim := t.TempDir()
	sentinel := writeEtcFixture(t, victim, "sentinel")
	generated := filepath.Join(root, "generated")
	if err := os.Symlink(victim, generated); err != nil {
		t.Fatal(err)
	}
	if _, err := preparePrivateFiles(identity); err == nil {
		t.Fatal("accepted private directory symlink")
	}
	if err := os.Remove(generated); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(generated, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(generated, "stale")); err != nil {
		t.Fatal(err)
	}
	if _, err := preparePrivateFiles(identity); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(sentinel); err != nil || string(content) != "sentinel\n" {
		t.Fatalf("cleanup followed alias: %q, %v", content, err)
	}
}

func TestPrivateExecutableIsIndependentSnapshot(t *testing.T) {
	source := writeEtcFixture(t, t.TempDir(), "command")
	root := t.TempDir()
	snapshot, err := copyPrivateExecutable(root, "command", source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("modified"), 0600); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(snapshot); err != nil || string(content) != "command\n" {
		t.Fatalf("snapshot changed with source: %q, %v", content, err)
	}
	if _, err := copyPrivateExecutable(root, "command", source); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(snapshot); err != nil || string(content) != "modified" {
		t.Fatalf("snapshot did not refresh: %q, %v", content, err)
	}
}

type failingPrivateReader struct{}

func (failingPrivateReader) Read(p []byte) (int, error) {
	return copy(p, "partial"), errors.New("read failed")
}

func TestPrivateFileCopyFailureIsAtomic(t *testing.T) {
	root := t.TempDir()
	path, err := writeStateFile(root, "file", []byte("original"), 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writeStateFileFrom(root, "file", failingPrivateReader{}, 0555); err == nil {
		t.Fatal("read failure ignored")
	}
	if content, err := os.ReadFile(path); err != nil || string(content) != "original" {
		t.Fatalf("failed copy replaced file: %q, %v", content, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 {
		t.Fatalf("temporary copy remains: %v, %v", entries, err)
	}
}

func TestLaunchLeavesMavenSettingsUnmanaged(t *testing.T) {
	for _, mode := range []string{"private", "host", "none"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
			home := t.TempDir()
			t.Setenv("HOME", home)
			// Host settings must not be parsed, copied, or mounted.
			writeAgentTestFile(t, filepath.Join(home, ".m2/settings.xml"), "invalid XML with host credentials")
			writeAgentTestFile(t, filepath.Join(home, ".m2/settings-security.xml"), "host master secret")
			opts := Options{Project: t.TempDir(), Instance: "unmanaged-maven", Network: mode, Podman: "off", TTY: "never", Command: []string{"/bin/true"}}
			plan, err := BuildPlan(opts)
			if err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(filepath.Dir(plan.State), "generated")
			stale := []string{filepath.Join(root, "maven/settings.xml"), filepath.Join(root, "maven/settings-security.xml"), filepath.Join(plan.State, "config/maven/settings.xml")}
			for _, path := range stale {
				writeAgentTestFile(t, path, "stale credentials")
			}
			originals := map[string]string{
				filepath.Join(plan.State, "home/.m2/settings.xml"):          "<settings><proxies/></settings>",
				filepath.Join(plan.State, "home/.m2/settings-security.xml"): "instance master secret",
			}
			for path, content := range originals {
				writeAgentTestFile(t, path, content)
			}
			plan, err = BuildPlan(opts)
			if err != nil {
				t.Fatal(err)
			}
			for _, arg := range plan.Bwrap {
				if strings.Contains(arg, "/.m2") || strings.Contains(arg, root+"/maven") {
					t.Fatalf("unexpected Maven mount: %s", arg)
				}
			}
			for _, path := range stale {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("stale Maven file remains: %s: %v", path, err)
				}
			}
			for path, want := range originals {
				if content, err := os.ReadFile(path); err != nil || string(content) != want {
					t.Fatalf("instance settings changed: %s: %q, %v", path, content, err)
				}
			}
		})
	}
}
