package app

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestProjectConfigTemplateIsNeutralAndComplete(t *testing.T) {
	directory := t.TempDir()
	if err := createProjectConfig(directory); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, projectConfigName)
	assertConfigTemplate(t, path, true)
}

func assertConfigTemplate(t *testing.T, path string, project bool) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(content, []byte("\n")) {
		t.Fatal("generated configuration does not end with a newline")
	}
	configType := reflect.TypeOf(fileConfig{})
	for index := 0; index < configType.NumField(); index++ {
		key := configType.Field(index).Tag.Get("toml")
		if key == "instance" && !project {
			if bytes.Contains(content, []byte("# instance =")) {
				t.Error("user template includes project-only instance setting")
			}
			continue
		}
		setting := "# " + key + " ="
		if key == "env" {
			setting = "# [env]"
		}
		if !bytes.Contains(content, []byte(setting)) {
			t.Errorf("generated configuration does not document %q", setting)
		}
	}
	for lineNumber, line := range strings.Split(strings.TrimSuffix(string(content), "\n"), "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			t.Errorf("line %d is active TOML: %q", lineNumber+1, line)
		}
	}
	layer, found, err := loadConfigFile(path)
	if err != nil || !found {
		t.Fatalf("generated configuration did not load: found=%v err=%v", found, err)
	}
	if layer.instance != nil || layer.agentConfig != nil || layer.gitConfig != nil || layer.network != nil || len(layer.networkAllow) != 0 || len(layer.publish) != 0 ||
		layer.podman != nil || layer.workspaceMode != nil || layer.landlock != nil || layer.seccomp != nil || len(layer.roBind) != 0 ||
		len(layer.rwBind) != 0 || len(layer.environment) != 0 || layer.tty != nil || layer.clipboard != nil {
		t.Fatalf("generated configuration is not neutral: %#v", layer)
	}
}

func TestCreateProjectConfigAndReportCreatesFile(t *testing.T) {
	directory := t.TempDir()
	t.Chdir(directory)
	var stdout bytes.Buffer
	if err := createProjectConfigAndReport(".", &stdout); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "Created "+filepath.Join(directory, projectConfigName)+"\nReview the file, then run bwrap-agent config trust.\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
	info, err := os.Stat(filepath.Join(directory, projectConfigName))
	if err != nil {
		t.Fatal(err)
	}
	if permissions := info.Mode().Perm(); permissions != 0o644 {
		t.Fatalf("permissions = %04o, want 0644", permissions)
	}
}

func TestConfigCreateCLIIsStandalone(t *testing.T) {
	var stdout, stderr bytes.Buffer
	parsed, code, err := parseCLI([]string{"config", "create", "project"}, &stdout, &stderr)
	if err != nil || code != 0 || parsed.Command != commandConfigCreateProject {
		t.Fatalf("parseCLI failed: parsed=%#v code=%d err=%v stderr=%q", parsed, code, err, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	parsed, code, err = parseCLI([]string{"config", "create", "user"}, &stdout, &stderr)
	if err != nil || code != 0 || parsed.Command != commandConfigCreateUser {
		t.Fatalf("user parse failed: parsed=%#v code=%d err=%v", parsed, code, err)
	}

	for _, args := range [][]string{
		{"config", "create"},
		{"config", "create", "global"},
		{"config", "create", "user", "opencode"},
		{"config", "create", "user", "--network", "none"},
		{"config", "create", "project", "opencode"},
		{"config", "create", "project", "--network", "none"},
		{"--network", "none", "config", "create", "project"},
	} {
		stdout.Reset()
		stderr.Reset()
		_, code, err := parseCLI(args, &stdout, &stderr)
		if len(args) == 2 && (!strings.Contains(stderr.String(), "project") || !strings.Contains(stderr.String(), "user")) {
			t.Errorf("missing scope guidance: %q", stderr.String())
		}
		if err == nil || code != 2 {
			t.Fatalf("parseCLI(%q) = code=%d err=%v stderr=%q", args, code, err, stderr.String())
		}
	}
}

func TestSubcommandNamesAndFlagsAfterProgramArePassedThrough(t *testing.T) {
	var stdout, stderr bytes.Buffer
	parsed, code, err := parseCLI([]string{"run", "config", "create", "project", "--network", "none"}, &stdout, &stderr)
	if err != nil || code != 0 {
		t.Fatalf("parseCLI failed: code=%d err=%v stderr=%q", code, err, stderr.String())
	}
	want := []string{"config", "create", "project", "--network", "none"}
	if parsed.Command != commandRun || strings.Join(parsed.Run.Command, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("target arguments were interpreted by the launcher: %#v", parsed)
	}
}

func TestConfigCreateParsingDoesNotLoadBrokenUserConfiguration(t *testing.T) {
	configDirectory := filepath.Join(t.TempDir(), "bwrap-agent")
	if err := os.MkdirAll(configDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDirectory, "config.toml"), []byte("this is not toml"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Dir(configDirectory))
	for scope, command := range map[string]commandKind{"project": commandConfigCreateProject, "user": commandConfigCreateUser} {
		var stdout, stderr bytes.Buffer
		parsed, code, err := parseCLI([]string{"config", "create", scope}, &stdout, &stderr)
		if err != nil || code != 0 || parsed.Command != command {
			t.Fatalf("config creation was blocked by user config: code=%d err=%v stderr=%q", code, err, stderr.String())
		}
	}
}

func TestCreateUserConfigPathsAndTemplate(t *testing.T) {
	for _, useXDG := range []bool{true, false} {
		name := "home"
		if useXDG {
			name = "xdg"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("HOME", root)
			t.Setenv("XDG_CONFIG_HOME", "")
			base := filepath.Join(root, ".config")
			if useXDG {
				base = filepath.Join(root, "custom", "config")
				t.Setenv("XDG_CONFIG_HOME", base)
			}
			t.Chdir(root)
			var stdout bytes.Buffer
			if err := createUserConfigAndReport(&stdout); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(base, "bwrap-agent", "config.toml")
			if stdout.String() != "Created "+path+"\n" {
				t.Fatalf("stdout = %q", stdout.String())
			}
			for target, mode := range map[string]os.FileMode{path: 0o600, filepath.Dir(path): 0o700, base: 0o700} {
				info, err := os.Stat(target)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != mode {
					t.Errorf("%s permissions = %o, want %o", target, info.Mode().Perm(), mode)
				}
			}
			if _, err := os.Lstat(filepath.Join(root, projectConfigName)); !os.IsNotExist(err) {
				t.Fatalf("user command created a project file: %v", err)
			}
			assertConfigTemplate(t, path, false)
		})
	}
}

func TestConfigCreationRefusesExistingEntries(t *testing.T) {
	for _, scope := range []string{"project", "user"} {
		for _, kind := range []string{"file", "directory", "symlink", "dangling-symlink"} {
			t.Run(scope+"/"+kind, func(t *testing.T) {
				root := t.TempDir()
				t.Setenv("XDG_CONFIG_HOME", root)
				path := filepath.Join(root, projectConfigName)
				if scope == "user" {
					path = filepath.Join(root, "bwrap-agent", "config.toml")
				}
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(root, "target")
				original := []byte("original content\n")
				switch kind {
				case "file":
					if err := os.WriteFile(path, original, 0o600); err != nil {
						t.Fatal(err)
					}
				case "directory":
					if err := os.Mkdir(path, 0o755); err != nil {
						t.Fatal(err)
					}
				case "symlink", "dangling-symlink":
					if kind == "symlink" {
						if err := os.WriteFile(target, original, 0o600); err != nil {
							t.Fatal(err)
						}
					}
					if err := os.Symlink(target, path); err != nil {
						t.Fatal(err)
					}
				}
				before, err := os.Lstat(path)
				if err != nil {
					t.Fatal(err)
				}
				var stdout bytes.Buffer
				if scope == "user" {
					err = createUserConfigAndReport(&stdout)
				} else {
					err = createProjectConfigAndReport(root, &stdout)
				}
				if err == nil || !strings.Contains(err.Error(), path) {
					t.Fatalf("creation error = %v", err)
				}
				if stdout.Len() != 0 {
					t.Fatalf("reported success: %q", stdout.String())
				}
				after, err := os.Lstat(path)
				if err != nil {
					t.Fatal(err)
				}
				if !os.SameFile(before, after) || before.Mode() != after.Mode() {
					t.Fatal("existing entry changed")
				}
				if kind == "file" || kind == "symlink" {
					content, err := os.ReadFile(path)
					if err != nil || !bytes.Equal(content, original) {
						t.Fatalf("existing content changed: %q, %v", content, err)
					}
				}
				if kind == "dangling-symlink" {
					if _, err := os.Lstat(target); !os.IsNotExist(err) {
						t.Fatalf("dangling target created: %v", err)
					}
				}
			})
		}
	}
}

func TestConfigCreationFilesystemErrors(t *testing.T) {
	root := t.TempDir()
	blocker := filepath.Join(root, "file")
	if err := os.WriteFile(blocker, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	if err := createProjectConfigAndReport(blocker, &stdout); err == nil || !strings.Contains(err.Error(), blocker) {
		t.Fatalf("project error = %v", err)
	}
	t.Setenv("XDG_CONFIG_HOME", blocker)
	if err := createUserConfigAndReport(&stdout); err == nil || !strings.Contains(err.Error(), blocker) {
		t.Fatalf("user error = %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("reported success: %q", stdout.String())
	}
	content, err := os.ReadFile(blocker)
	if err != nil || string(content) != "unchanged" {
		t.Fatalf("blocker changed: %q, %v", content, err)
	}
	t.Setenv("XDG_CONFIG_HOME", "relative")
	if err := createUserConfigAndReport(&stdout); err == nil {
		t.Fatal("accepted relative XDG_CONFIG_HOME")
	}
}
