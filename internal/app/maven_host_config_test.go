package app

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestMavenConfigOptions(t *testing.T) {
	project, config := t.TempDir(), t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", config)
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	for _, scenario := range []struct {
		name, user, project string
		args                []string
		disabled            bool
	}{
		{name: "default"},
		{name: "cli-disable", args: []string{"--no-maven-config"}, disabled: true},
		{name: "independent", args: []string{"--no-agent-config", "--no-git-config"}},
		{name: "user-disable", user: "maven_config = false", disabled: true},
		{name: "project-enable", user: "maven_config = false", project: "maven_config = true"},
		{name: "project-disable", user: "maven_config = true", project: "maven_config = false", disabled: true},
		{name: "cli-enable", project: "maven_config = false", args: []string{"--maven-config"}},
		{name: "cli-wins", project: "maven_config = true", args: []string{"--no-maven-config"}, disabled: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			writeAgentTestFile(t, filepath.Join(config, "bwrap-agent", "config.toml"), scenario.user)
			writeAgentTestFile(t, filepath.Join(project, projectConfigName), scenario.project)
			trustTestConfig(t, project)
			args := append([]string{"run", "--project", project}, scenario.args...)
			args = append(args, "/bin/true")
			opts, code, err := parseOptions(args, &bytes.Buffer{}, &bytes.Buffer{})
			if err != nil || code != 0 || opts.NoMavenConfig != scenario.disabled {
				t.Fatalf("options=%#v code=%d err=%v", opts, code, err)
			}
		})
	}
}

func TestMavenHostSettingsLifecycle(t *testing.T) {
	for _, mode := range []string{"private", "host", "none"} {
		t.Run(mode, func(t *testing.T) {
			_, context := testAgentContext(t)
			home := t.TempDir()
			t.Setenv("HOME", home)
			// Exercise both paths used by Java and ordinary HOME clients.
			context.state = filepath.Join(context.state, "state:alias")
			if err := os.Mkdir(context.state, 0o700); err != nil {
				t.Fatal(err)
			}
			host := filepath.Join(home, ".m2/settings.xml")
			security := filepath.Join(home, ".m2/settings-security.xml")
			original := filepath.Join(context.state, "home/.m2/settings.xml")
			generated := filepath.Join(context.state, "config/maven/settings.xml")
			local := "<settings><offline>true</offline></settings>"
			writeAgentTestFile(t, original, local)
			for _, phase := range []string{"missing", "added", "edited", "disabled", "reenabled", "security-removed", "removed"} {
				switch phase {
				case "added", "edited":
					writeAgentTestFile(t, host, "<settings><servers><server><password>host-"+phase+"</password></server></servers></settings>")
					writeAgentTestFile(t, security, "<settingsSecurity><master>master-"+phase+"</master></settingsSecurity>")
				case "security-removed":
					if err := os.Remove(security); err != nil {
						t.Fatal(err)
					}
				case "removed":
					if err := os.Remove(host); err != nil {
						t.Fatal(err)
					}
				}
				// Seed a previous host-derived copy, including when entering
				// host/none mode. It must never survive the next preparation.
				writeAgentTestFile(t, generated, "stale-host-secret")
				files, err := prepareMavenConfig(context, t.TempDir(), mode, phase != "disabled")
				if err != nil {
					t.Fatalf("%s: %v", phase, err)
				}
				inherited := phase != "missing" && phase != "removed" && phase != "disabled"
				wantCount := 0
				if inherited || mode == "private" {
					wantCount = 2
				}
				if inherited && phase != "security-removed" {
					wantCount += 2
				}
				if len(files) != wantCount {
					t.Fatalf("%s: mounts = %#v", phase, files)
				}
				for _, file := range files {
					info, err := os.Stat(file.Source)
					if err != nil || info.Mode().Perm() != 0o600 {
						t.Fatalf("unsafe permissions: %#v", file)
					}
					content, err := os.ReadFile(file.Source)
					if err != nil {
						t.Fatal(err)
					}
					if filepath.Base(file.Destination) == "settings-security.xml" {
						if !strings.Contains(string(content), "master-") {
							t.Fatal("missing security file")
						}
						continue
					}
					if strings.Contains(string(content), "host-") != inherited {
						t.Fatalf("%s: incorrect source %s", phase, content)
					}
					if strings.Contains(string(content), "127.0.0.1") != (mode == "private") {
						t.Fatalf("%s: incorrect proxy %s", phase, content)
					}
					if mode != "private" {
						expected, err := os.ReadFile(host)
						if err != nil || !bytes.Equal(content, expected) {
							t.Fatal("host settings changed")
						}
					}
				}
				content, err := os.ReadFile(original)
				if err != nil || string(content) != local {
					t.Fatal("instance settings overwritten")
				}
				content, err = os.ReadFile(generated)
				if mode != "private" && !os.IsNotExist(err) {
					t.Fatal("stale generated file still exists")
				}
				if bytes.Contains(content, []byte("stale-host-secret")) || !inherited && bytes.Contains(content, []byte("host-")) {
					t.Fatal("stale host credentials exposed")
				}
			}
		})
	}
}

func TestMavenHostSourceSafety(t *testing.T) {
	for _, name := range []string{"settings.xml", "settings-security.xml"} {
		for _, scenario := range []string{"regular", "symlink", "dangling", "directory", "fifo", "project", "state", "rw-bind", "unreadable"} {
			t.Run(name+"/"+scenario, func(t *testing.T) {
				_, context := testAgentContext(t)
				home := t.TempDir()
				t.Setenv("HOME", home)
				settings := filepath.Join(home, ".m2/settings.xml")
				writeAgentTestFile(t, settings, "<settings/>")
				source := filepath.Join(home, ".m2", name)
				if err := os.Remove(source); err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
				switch scenario {
				case "directory":
					if err := os.Mkdir(source, 0o700); err != nil {
						t.Fatal(err)
					}
				case "fifo":
					if err := unix.Mkfifo(source, 0o600); err != nil {
						t.Fatal(err)
					}
				case "regular", "unreadable":
					writeAgentTestFile(t, source, "<settings/>")
					if scenario == "unreadable" {
						if os.Geteuid() == 0 {
							t.Skip("requires non-root user")
						}
						if err := os.Chmod(source, 0); err != nil {
							t.Fatal(err)
						}
					}
				default:
					target := filepath.Join(t.TempDir(), "source")
					switch scenario {
					case "project":
						target = filepath.Join(context.project, "source")
					case "state":
						target = filepath.Join(context.state, "source")
					case "rw-bind":
						context.rwBind = []string{t.TempDir()}
						target = filepath.Join(context.rwBind[0], "source")
					}
					if scenario != "dangling" {
						writeAgentTestFile(t, target, "<settings/>")
					}
					if err := os.Symlink(target, source); err != nil {
						t.Fatal(err)
					}
				}
				_, err := prepareMavenConfig(context, t.TempDir(), "private", true)
				if (err != nil) != (scenario != "regular" && scenario != "symlink") {
					t.Fatalf("source %s: %v", source, err)
				}
			})
		}
	}
}

func TestMavenHostSelectionBeforeParsing(t *testing.T) {
	_, context := testAgentContext(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	host := filepath.Join(home, ".m2/settings.xml")
	original := filepath.Join(context.state, "home/.m2/settings.xml")
	writeAgentTestFile(t, host, "<settings/>")
	writeAgentTestFile(t, original, "invalid instance XML")
	if _, err := prepareMavenConfig(context, t.TempDir(), "private", true); err != nil {
		t.Fatal(err)
	}
	writeAgentTestFile(t, original, "<settings/>")
	writeAgentTestFile(t, host, "invalid host XML")
	for _, mode := range []string{"private", "host", "none"} {
		if _, err := prepareMavenConfig(context, t.TempDir(), mode, true); err == nil {
			t.Fatalf("%s silently ignored invalid host XML", mode)
		}
		if _, err := prepareMavenConfig(context, t.TempDir(), mode, false); err != nil {
			t.Fatal(err)
		}
	}
}
