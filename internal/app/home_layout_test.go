package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func homeLayoutFixture(t *testing.T) (string, Options) {
	t.Helper()
	home := filepath.Join(t.TempDir(), "host-home")
	for _, relative := range []string{"project", "tools", "writable"} {
		if err := os.MkdirAll(filepath.Join(home, relative), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	return home, Options{
		Project: filepath.Join(home, "project"), Instance: "home-layout",
		Network: "host", Podman: "off", TTY: "never", Command: []string{"/bin/true"},
		ROBind: []string{filepath.Join(home, "tools")}, RWBind: []string{filepath.Join(home, "writable")},
	}
}

func TestBuildPlanUsesHostHomeWithPrivateBacking(t *testing.T) {
	for _, mode := range []string{"write-through", "copy-on-write", "read-only"} {
		for _, nestedState := range []bool{false, true} {
			name := mode + "/external-state"
			if nestedState {
				name = mode + "/home-state"
			}
			t.Run(name, func(t *testing.T) {
				home, opts := homeLayoutFixture(t)
				opts.WorkspaceMode = mode
				if nestedState {
					t.Setenv("BWRAP_AGENT_STATE_HOME", filepath.Join(home, ".local", "state", "bwrap-agent"))
				}
				writeTestFile(t, filepath.Join(home, ".gitconfig"), "[user]\nname = Home Test\n")
				plan, err := BuildPlan(opts)
				if err != nil {
					t.Fatal(err)
				}
				expected := map[string]string{
					"HOME": home, "TMPDIR": "/tmp", "XDG_CONFIG_HOME": filepath.Join(home, ".config"),
					"XDG_CACHE_HOME": filepath.Join(home, ".cache"),
					"XDG_DATA_HOME":  filepath.Join(home, ".local", "share"),
					"XDG_STATE_HOME": filepath.Join(home, ".local", "state"),
				}
				for key, want := range expected {
					if got := plan.LaunchEnv[key]; got != want {
						t.Errorf("%s = %q, want %q", key, got, want)
					}
				}
				joined := strings.Join(plan.Bwrap, "\x00")
				homeMount := "--bind\x00" + filepath.Join(plan.State, "home") + "\x00" + home
				homeIndex := strings.Index(joined, homeMount)
				if homeIndex < 0 {
					t.Fatalf("private home mount missing: %#v", plan.Bwrap)
				}
				for _, mount := range []string{
					"--ro-bind\x00" + opts.ROBind[0] + "\x00" + opts.ROBind[0],
					"--bind\x00" + opts.RWBind[0] + "\x00" + opts.RWBind[0],
					"--ro-bind\x00" + filepath.Join(home, ".gitconfig") + "\x00" + filepath.Join(home, ".gitconfig"),
				} {
					if index := strings.Index(joined, mount); index <= homeIndex {
						t.Errorf("mount %q is missing or precedes private home", mount)
					}
				}
				projectIndex := strings.Index(joined, "--bind\x00"+opts.Project+"\x00"+opts.Project)
				if mode == "read-only" {
					projectIndex = strings.Index(joined, "--ro-bind\x00"+opts.Project+"\x00"+opts.Project)
				}
				if mode == "copy-on-write" {
					projectIndex = strings.Index(joined, "--overlay-src\x00"+opts.Project)
				}
				if projectIndex <= homeIndex {
					t.Fatal("project mount is missing or hidden by private home")
				}
				passwd, err := os.ReadFile(filepath.Join(filepath.Dir(plan.State), "generated", "etc", "passwd"))
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(passwd), ":"+home+":/bin/sh") {
					t.Fatalf("passwd home mismatch: %s", passwd)
				}
				for _, relative := range []string{"home/saved", "home/.config/saved", "home/.local/share/saved"} {
					writeTestFile(t, filepath.Join(plan.State, relative), "keep me")
				}
				if _, err := BuildPlan(opts); err != nil {
					t.Fatal(err)
				}
				for _, relative := range []string{"home/saved", "home/.config/saved", "home/.local/share/saved"} {
					content, err := os.ReadFile(filepath.Join(plan.State, relative))
					if err != nil || string(content) != "keep me" {
						t.Fatalf("instance data changed: %s: %q, %v", relative, content, err)
					}
				}
				if _, err := os.Stat(filepath.Join(home, ".config")); !os.IsNotExist(err) {
					t.Fatalf("created host configuration directory: %v", err)
				}
			})
		}
	}
}

func TestBuildPlanSupportsSymlinkedHostHome(t *testing.T) {
	home, opts := homeLayoutFixture(t)
	alias := filepath.Join(t.TempDir(), "home-alias")
	if err := os.Symlink(home, alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", alias)
	opts.ROBind = []string{"~/tools"}
	plan, err := BuildPlan(opts)
	if err != nil {
		t.Fatal(err)
	}
	if plan.LaunchEnv["HOME"] != alias {
		t.Fatalf("HOME = %q", plan.LaunchEnv["HOME"])
	}
	joined := strings.Join(plan.Bwrap, "\x00")
	for _, operation := range []string{
		"--bind\x00" + filepath.Join(plan.State, "home") + "\x00" + home,
		"--symlink\x00" + home + "\x00" + alias,
		"--ro-bind\x00" + filepath.Join(home, "tools") + "\x00" + filepath.Join(home, "tools"),
	} {
		if !strings.Contains(joined, operation) {
			t.Fatalf("missing operation %q", operation)
		}
	}
}

func TestBuildPlanRejectsPrivateHomeDestinationSymlinks(t *testing.T) {
	for _, relative := range []string{"home/.config", "home/.local", "home/tools", "home/project", "home/.gitconfig"} {
		t.Run(relative, func(t *testing.T) {
			home, opts := homeLayoutFixture(t)
			writeTestFile(t, filepath.Join(home, ".gitconfig"), "[user]\nname = Test\n")
			plan, err := BuildPlan(opts)
			if err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(plan.State, relative)
			if err := os.RemoveAll(destination); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(t.TempDir(), destination); err != nil {
				t.Fatal(err)
			}
			if _, err := BuildPlan(opts); err == nil {
				t.Fatal("accepted a symlink in the private mount destination")
			}
		})
	}
}

func TestHomeLayoutRejectsMountsCoveringHome(t *testing.T) {
	for _, kind := range []string{"project", "ro-bind", "rw-bind"} {
		t.Run(kind, func(t *testing.T) {
			home, _ := homeLayoutFixture(t)
			identity := instanceIdentity{Project: filepath.Join(home, "project"), State: t.TempDir()}
			switch kind {
			case "project":
				identity.Project = home
			case "ro-bind":
				identity.ROBind = []string{filepath.Dir(home)}
			case "rw-bind":
				identity.RWBind = []string{home}
			}
			if _, err := resolveHomeLayout(identity); err == nil || !strings.Contains(err.Error(), "replace private sandbox home") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestHomeLayoutOmitsStateAliases(t *testing.T) {
	for _, podman := range []string{"off", "on"} {
		t.Run(podman, func(t *testing.T) {
			_, opts := homeLayoutFixture(t)
			opts.Podman = podman
			plan, err := BuildPlan(opts)
			if err != nil {
				t.Fatal(err)
			}
			for _, relative := range []string{"config", "data", "tmp", "podman"} {
				if _, err := os.Lstat(filepath.Join(plan.State, relative)); !os.IsNotExist(err) {
					t.Fatalf("created obsolete directory %s: %v", relative, err)
				}
				writeAgentTestFile(t, filepath.Join(plan.State, relative, "legacy"), "untouched")
			}
			plan, err = BuildPlan(opts)
			if err != nil {
				t.Fatal(err)
			}
			homeMounts := 0
			for index, arg := range plan.Bwrap {
				switch arg {
				case "--bind", "--ro-bind":
					source, destination := plan.Bwrap[index+1], plan.Bwrap[index+2]
					if pathWithin(plan.State, destination) {
						t.Errorf("backing destination exposed: %s", destination)
					}
					if source == filepath.Join(plan.State, "home") {
						homeMounts++
					}
				case "--setenv":
					if strings.Contains(plan.Bwrap[index+2], plan.State) {
						t.Errorf("backing path in sandbox environment: %s", plan.Bwrap[index+1])
					}
				}
			}
			if homeMounts != 1 {
				t.Fatalf("private home mounted %d times", homeMounts)
			}
			for _, relative := range []string{"config", "data", "tmp", "podman"} {
				content, err := os.ReadFile(filepath.Join(plan.State, relative, "legacy"))
				if err != nil || string(content) != "untouched" {
					t.Fatalf("legacy data modified: %q, %v", content, err)
				}
			}
			for _, relative := range []string{"home/.config/legacy", "home/.local/share/legacy"} {
				if _, err := os.Lstat(filepath.Join(plan.State, relative)); !os.IsNotExist(err) {
					t.Fatalf("legacy data migrated to %s: %v", relative, err)
				}
			}
		})
	}
}

func TestHomeLayoutPreservesTemporaryDirectoryOverride(t *testing.T) {
	_, opts := homeLayoutFixture(t)
	opts.Env = []string{"TMPDIR=/var/tmp"}
	plan, err := BuildPlan(opts)
	if err != nil {
		t.Fatal(err)
	}
	if plan.LaunchEnv["TMPDIR"] != "/var/tmp" {
		t.Fatalf("TMPDIR = %q", plan.LaunchEnv["TMPDIR"])
	}
}
