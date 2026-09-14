package app

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestGitConfigSources(t *testing.T) {
	for _, scenario := range []string{"both", "home-only", "xdg-only", "missing", "fallback-unset", "fallback-empty", "symlink", "dangling", "directory", "fifo", "unreadable", "project", "state", "git-common", "rw-bind", "symlink-parent", "destination-symlink", "destination-parent-symlink", "destination-directory", "existing", "disabled"} {
		t.Run(scenario, func(t *testing.T) {
			_, context := testAgentContext(t)
			home, config := t.TempDir(), t.TempDir()
			t.Setenv("HOME", home)
			context.hostEnv = []string{"XDG_CONFIG_HOME=" + config}
			if scenario == "fallback-unset" || scenario == "fallback-empty" {
				config = filepath.Join(home, ".config")
				context.hostEnv = nil
				if scenario == "fallback-empty" {
					context.hostEnv = []string{"XDG_CONFIG_HOME="}
				}
			}
			source := filepath.Join(home, ".gitconfig")
			xdgSource := filepath.Join(config, "git", "config")
			destination := filepath.Join(context.state, "home", ".gitconfig")
			if scenario != "missing" && scenario != "xdg-only" {
				writeAgentTestFile(t, source, "[user]\nname = Home\n")
			}
			if scenario == "both" || scenario == "xdg-only" || strings.HasPrefix(scenario, "fallback-") {
				writeAgentTestFile(t, xdgSource, "[user]\nname = XDG\n")
			}
			wantError := false
			switch scenario {
			case "symlink", "dangling", "project", "state", "git-common", "rw-bind", "symlink-parent":
				target := filepath.Join(t.TempDir(), "config")
				switch scenario {
				case "project":
					target = filepath.Join(context.project, "config")
				case "state":
					target = filepath.Join(context.state, "config-file")
				case "git-common":
					context.gitCommon = t.TempDir()
					target = filepath.Join(context.gitCommon, "config")
				case "rw-bind":
					context.rwBind = []string{t.TempDir()}
					target = filepath.Join(context.rwBind[0], "config")
				}
				if scenario != "dangling" {
					writeAgentTestFile(t, target, "[user]\nname = Linked\n")
				}
				if scenario == "symlink-parent" {
					alias := filepath.Join(context.project, "alias")
					if err := os.Symlink(filepath.Dir(target), alias); err != nil {
						t.Fatal(err)
					}
					target = filepath.Join(alias, "config")
				}
				if err := os.Remove(source); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, source); err != nil {
					t.Fatal(err)
				}
				wantError = scenario != "symlink"
			case "directory", "fifo", "disabled":
				if err := os.Remove(source); err != nil {
					t.Fatal(err)
				}
				if scenario == "fifo" {
					if err := unix.Mkfifo(source, 0o600); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Mkdir(source, 0o700); err != nil {
					t.Fatal(err)
				}
				wantError = scenario != "disabled"
			case "unreadable":
				if os.Geteuid() == 0 {
					t.Skip("requires an unprivileged user")
				}
				if err := os.Chmod(source, 0); err != nil {
					t.Fatal(err)
				}
				wantError = true
			case "destination-symlink", "destination-directory", "existing":
				writeAgentTestFile(t, destination, "instance-local\n")
				if scenario != "existing" {
					if err := os.Remove(destination); err != nil {
						t.Fatal(err)
					}
					if scenario == "destination-symlink" {
						if err := os.Symlink(source, destination); err != nil {
							t.Fatal(err)
						}
					} else if err := os.Mkdir(destination, 0o700); err != nil {
						t.Fatal(err)
					}
					wantError = true
				}
			case "destination-parent-symlink":
				if err := os.Symlink(t.TempDir(), filepath.Dir(destination)); err != nil {
					t.Fatal(err)
				}
				wantError = true
			}
			mounts, err := prepareGitConfigMounts(context, scenario != "disabled")
			if wantError {
				if err == nil || !strings.Contains(err.Error(), "Git user configuration") {
					t.Fatalf("expected contextual error, got mounts=%#v err=%v", mounts, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			wantCount := 1
			if scenario == "both" || strings.HasPrefix(scenario, "fallback-") {
				wantCount = 2
			}
			if scenario == "missing" || scenario == "disabled" {
				wantCount = 0
			}
			if len(mounts) != wantCount {
				t.Fatalf("mounts = %#v, want %d", mounts, wantCount)
			}
			for _, mount := range mounts {
				expected := source
				if mount.Destination == filepath.Join(context.state, "config", "git", "config") {
					expected = xdgSource
				} else if mount.Destination != destination {
					t.Fatalf("unexpected destination: %#v", mount)
				}
				resolved, err := filepath.EvalSymlinks(expected)
				if err != nil || mount.Source != resolved {
					t.Fatalf("unexpected source: %#v err=%v", mount, err)
				}
			}
			if scenario == "existing" {
				got, err := os.ReadFile(destination)
				if err != nil || string(got) != "instance-local\n" {
					t.Fatalf("instance config changed: %q %v", got, err)
				}
			}
		})
	}
}

func TestGitConfigOptions(t *testing.T) {
	project, config := t.TempDir(), t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", config)
	for _, scenario := range []struct {
		name, user, project string
		args                []string
		disabled            bool
	}{
		{name: "default"},
		{name: "cli-disable", args: []string{"--no-git-config"}, disabled: true},
		{name: "independent", args: []string{"--no-agent-config"}},
		{name: "user-disable", user: "git_config = false", disabled: true},
		{name: "project-enable", user: "git_config = false", project: "git_config = true"},
		{name: "project-disable", user: "git_config = true", project: "git_config = false", disabled: true},
		{name: "cli-enable", project: "git_config = false", args: []string{"--git-config"}},
		{name: "cli-wins", project: "git_config = true", args: []string{"--no-git-config"}, disabled: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			writeAgentTestFile(t, filepath.Join(config, "bwrap-agent", "config.toml"), scenario.user)
			writeAgentTestFile(t, filepath.Join(project, projectConfigName), scenario.project)
			args := append([]string{"run", "--project", project}, scenario.args...)
			args = append(args, "/bin/true")
			opts, code, err := parseOptions(args, &bytes.Buffer{}, &bytes.Buffer{})
			if err != nil || code != 0 || opts.NoGitConfig != scenario.disabled {
				t.Fatalf("options=%#v code=%d err=%v", opts, code, err)
			}
		})
	}
}

func TestGitConfigPlan(t *testing.T) {
	home, config := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", config)
	source := filepath.Join(home, ".gitconfig")
	writeAgentTestFile(t, source, "[user]\nname = Test\n")
	writeAgentTestFile(t, filepath.Join(config, "git", "config"), "[user]\nemail = test@example.invalid\n")
	for _, mode := range []string{"write-through", "copy-on-write", "read-only"} {
		for _, podman := range []string{"off", "on"} {
			if mode == "read-only" && podman == "on" {
				continue
			}
			t.Run(mode+"-"+podman, func(t *testing.T) {
				t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
				opts := Options{Project: t.TempDir(), Network: "host", Podman: podman, WorkspaceMode: mode, TTY: "never", NoAgentConfig: true, Command: []string{"/bin/true"}}
				plan, err := BuildPlan(opts)
				if err != nil {
					t.Fatal(err)
				}
				joined := strings.Join(plan.Bwrap, "\x00")
				for _, mount := range []agentMount{
					{source, filepath.Join(plan.State, "home", ".gitconfig"), false},
					{filepath.Join(config, "git", "config"), filepath.Join(plan.State, "config", "git", "config"), false},
				} {
					resolved, _ := filepath.EvalSymlinks(mount.Source)
					index := strings.Index(joined, "--ro-bind\x00"+resolved+"\x00"+mount.Destination)
					if index < 0 || index < strings.Index(joined, "--bind\x00"+plan.State) {
						t.Fatalf("missing or hidden Git mount: %#v", plan.Bwrap)
					}
				}
				opts.NoGitConfig = true
				plan, err = BuildPlan(opts)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(strings.Join(plan.Bwrap, "\x00"), source) {
					t.Fatal("disabled Git config is exposed")
				}
			})
		}
	}
}
