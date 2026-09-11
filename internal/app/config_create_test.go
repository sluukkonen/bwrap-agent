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
	if layer.instance != nil || layer.agentConfig != nil || layer.network != nil || len(layer.networkAllow) != 0 || len(layer.publish) != 0 ||
		layer.podman != nil || layer.workspaceMode != nil || layer.landlock != nil || layer.seccomp != nil || len(layer.roBind) != 0 ||
		len(layer.rwBind) != 0 || len(layer.environment) != 0 || layer.tty != nil || layer.clipboard != nil {
		t.Fatalf("generated configuration is not neutral: %#v", layer)
	}
}

func TestCreateProjectConfigAndReportCreatesFile(t *testing.T) {
	directory := t.TempDir()
	var stdout bytes.Buffer
	if err := createProjectConfigAndReport(directory, &stdout); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "Created .bwrap-agent.toml\n" {
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

func TestCreateProjectConfigRefusesExistingFile(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, projectConfigName)
	original := []byte("existing content\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := createProjectConfig(directory); err == nil {
		t.Fatal("createProjectConfig replaced an existing file")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(content, original) {
		t.Fatalf("existing content changed to %q", content)
	}
}

func TestCreateProjectConfigRefusesExistingSymlink(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	path := filepath.Join(directory, projectConfigName)
	original := []byte("target content\n")
	if err := os.WriteFile(target, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := createProjectConfig(directory); err == nil {
		t.Fatal("createProjectConfig followed an existing symlink")
	}
	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(content, original) {
		t.Fatalf("symlink target changed to %q", content)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("configuration symlink was replaced: mode=%v", info.Mode())
	}
}

func TestConfigCreateCLIIsStandalone(t *testing.T) {
	var stdout, stderr bytes.Buffer
	parsed, code, err := parseCLI([]string{"config", "create"}, &stdout, &stderr)
	if err != nil || code != 0 || parsed.Command != commandConfigCreate {
		t.Fatalf("parseCLI failed: parsed=%#v code=%d err=%v stderr=%q", parsed, code, err, stderr.String())
	}

	for _, args := range [][]string{
		{"config", "create", "opencode"},
		{"config", "create", "--network", "none"},
		{"--network", "none", "config", "create"},
	} {
		stdout.Reset()
		stderr.Reset()
		_, code, err := parseCLI(args, &stdout, &stderr)
		if err == nil || code != 2 {
			t.Fatalf("parseCLI(%q) = code=%d err=%v stderr=%q", args, code, err, stderr.String())
		}
	}
}

func TestSubcommandNamesAndFlagsAfterProgramArePassedThrough(t *testing.T) {
	var stdout, stderr bytes.Buffer
	parsed, code, err := parseCLI([]string{"run", "config", "create", "--network", "none"}, &stdout, &stderr)
	if err != nil || code != 0 {
		t.Fatalf("parseCLI failed: code=%d err=%v stderr=%q", code, err, stderr.String())
	}
	want := []string{"config", "create", "--network", "none"}
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
	var stdout, stderr bytes.Buffer
	parsed, code, err := parseCLI([]string{"config", "create"}, &stdout, &stderr)
	if err != nil || code != 0 || parsed.Command != commandConfigCreate {
		t.Fatalf("config creation was blocked by user config: code=%d err=%v stderr=%q", code, err, stderr.String())
	}
}
