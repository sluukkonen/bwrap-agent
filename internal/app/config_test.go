package app

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestConfigurationPrecedenceAndEnvironment(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	root := t.TempDir()
	project := filepath.Join(root, "project")
	userDirectory := filepath.Join(root, "config", "bwrap-agent")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(userDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(userDirectory, "config.toml"), `
network = "host"
podman = "off"
workspace_mode = "copy-on-write"
landlock = "off"
seccomp = "off"
network_allow = ["https://user.example", "https://duplicate.example"]
publish = ["11001:1"]
host_port = ["12001:1"]
ro_bind = ["user-relative"]
unset_env = ["PROJECT_REINTRODUCED"]

[env]
OVERRIDE = "user"
USER_ONLY = "user"
MISSING_FALLBACK = "lower"
`)
	writeTestFile(t, filepath.Join(project, ".bwrap-agent.toml"), `
instance = "project-instance"
agent_config = "off"
network = "private"
tty = "never"
workspace_mode = "read-only"
landlock = "auto"
seccomp = "auto"
network_allow = ["https://project.example", "https://duplicate.example:443"]
publish = ["11002:2"]
host_port = ["12002:2"]
ro_bind = ["project-relative"]
unset_env = ["PROJECT_UNSET"]

[env]
OVERRIDE = "project"
PROJECT_REINTRODUCED = "project"
PROJECT_UNSET = "discarded"
MISSING_FALLBACK = { inherit = true }
PRESENT = { inherit = true }
`)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("PRESENT", "from-host")
	t.Setenv("CLI_INHERIT", "from-cli-host")
	t.Setenv("MISSING_FALLBACK", "temporarily-present")
	os.Unsetenv("MISSING_FALLBACK")

	trustTestConfig(t, project)
	var stdout, stderr bytes.Buffer
	opts, code, err := parseOptions([]string{
		"run",
		"--project", project,
		"--network", "none",
		"--network-allow", "https://cli.example",
		"--instance", "cli-instance",
		"--workspace-mode", "write-through",
		"--landlock", "required",
		"--seccomp", "required",
		"--agent-config", "auto",
		"--publish", "11003:3",
		"--host-port", "12003:3", "--host-port", "12004:4/udp",
		"--ro-bind", "cli-relative",
		"--env", "OVERRIDE=cli",
		"--env", "CLI_INHERIT",
		"--unsetenv", "USER_ONLY",
		"/bin/true",
	}, &stdout, &stderr)
	if err != nil || code != 0 {
		t.Fatalf("parseOptions failed: code=%d err=%v stderr=%q", code, err, stderr.String())
	}
	if opts.Instance != "cli-instance" || opts.Network != "none" || opts.Podman != "off" || opts.WorkspaceMode != "write-through" || opts.Landlock != "required" || opts.Seccomp != "required" || opts.TTY != "never" || opts.AgentConfig != "auto" {
		t.Fatalf("unexpected merged scalars: %#v", opts)
	}
	if want := []string{"11001:1", "11002:2", "11003:3"}; !reflect.DeepEqual(opts.Publish, want) {
		t.Fatalf("publish = %#v, want %#v", opts.Publish, want)
	}
	if want := []string{"12001:1", "12002:2", "12003:3", "12004:4/udp"}; !reflect.DeepEqual(opts.HostPort, want) {
		t.Fatalf("host ports = %#v, want %#v", opts.HostPort, want)
	}
	if want := []string{"https://user.example", "https://duplicate.example", "https://project.example", "https://cli.example"}; !reflect.DeepEqual(opts.NetworkAllow, want) {
		t.Fatalf("network allowlist = %#v, want %#v", opts.NetworkAllow, want)
	}
	wantBinds := []string{
		filepath.Join(userDirectory, "user-relative"),
		filepath.Join(project, "project-relative"),
		filepath.Join(mustWorkingDirectory(t), "cli-relative"),
	}
	if !reflect.DeepEqual(opts.ROBind, wantBinds) {
		t.Fatalf("ro binds = %#v, want %#v", opts.ROBind, wantBinds)
	}
	wantEnv := []string{
		"CLI_INHERIT=from-cli-host",
		"OVERRIDE=cli",
		"PRESENT=from-host",
		"PROJECT_REINTRODUCED=project",
	}
	if !reflect.DeepEqual(opts.Env, wantEnv) {
		t.Fatalf("env = %#v, want %#v", opts.Env, wantEnv)
	}
	wantUnset := []string{"MISSING_FALLBACK", "PROJECT_UNSET", "USER_ONLY"}
	if !reflect.DeepEqual(opts.UnsetEnv, wantUnset) {
		t.Fatalf("unset env = %#v, want %#v", opts.UnsetEnv, wantUnset)
	}
	wantSources := []ConfigSource{
		{Scope: "user", Path: filepath.Join(userDirectory, "config.toml")},
		{Scope: "project", Path: filepath.Join(project, ".bwrap-agent.toml")},
	}
	if !reflect.DeepEqual(opts.ConfigFiles, wantSources) {
		t.Fatalf("config files = %#v, want %#v", opts.ConfigFiles, wantSources)
	}
}

func TestInstanceIsProjectConfigurable(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(project, projectConfigName), `instance = "configured-instance"`)
	trustTestConfig(t, project)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "empty-config"))

	parse := func(extra ...string) Options {
		args := append([]string{"run", "--project", project}, extra...)
		args = append(args, "/bin/true")
		opts, code, err := parseOptions(args, &bytes.Buffer{}, &bytes.Buffer{})
		if err != nil || code != 0 {
			t.Fatalf("parseOptions(%q) failed: code=%d err=%v", args, code, err)
		}
		return opts
	}

	if got := parse().Instance; got != "configured-instance" {
		t.Fatalf("project-configured instance = %q", got)
	}
	if got := parse("--instance", "cli-instance").Instance; got != "cli-instance" {
		t.Fatalf("CLI instance override = %q", got)
	}
	if got := parse("--no-project-config").Instance; got != "" {
		t.Fatalf("instance with project config disabled = %q, want default selection", got)
	}
}

func TestInstanceIsRejectedInUserConfiguration(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	userDirectory := filepath.Join(root, "config", "bwrap-agent")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(userDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(userDirectory, "config.toml"), `instance = "global-instance"`)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))

	var stderr bytes.Buffer
	_, code, err := parseOptions([]string{"run", "--project", project, "/bin/true"}, &bytes.Buffer{}, &stderr)
	if err == nil || code != 2 || !strings.Contains(err.Error(), "only valid in project configuration") {
		t.Fatalf("user instance setting result: code=%d err=%v stderr=%q", code, err, stderr.String())
	}
}

func TestProjectConfiguredInstanceRetainsProjectOwnership(t *testing.T) {
	root := t.TempDir()
	projectA := filepath.Join(root, "project-a")
	projectB := filepath.Join(root, "project-b")
	for _, project := range []string{projectA, projectB} {
		if err := os.MkdirAll(project, 0o700); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, filepath.Join(project, projectConfigName), `instance = "shared-name"`)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "empty-config"))
	t.Setenv("BWRAP_AGENT_STATE_HOME", filepath.Join(root, "state-home"))

	trustTestConfig(t, projectA)
	trustTestConfig(t, projectB)
	parse := func(project string) Options {
		opts, code, err := parseOptions([]string{"run", "--project", project, "/bin/true"}, &bytes.Buffer{}, &bytes.Buffer{})
		if err != nil || code != 0 {
			t.Fatalf("parse project %s: code=%d err=%v", project, code, err)
		}
		return opts
	}
	if _, err := resolveTestInstance(t, parse(projectA)); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveTestInstance(t, parse(projectB)); err == nil || !strings.Contains(err.Error(), "belongs to project") {
		t.Fatalf("second project reused configured instance: %v", err)
	}
}

func mustWorkingDirectory(t *testing.T) string {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return directory
}

func TestConfigurationIsStrict(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{"unknown key", `netwrok = "none"`},
		{"invocation key", `project = "/tmp"`},
		{"wrong scalar type", `network = true`},
		{"invalid choice", `tty = "sometimes"`},
		{"invalid seccomp", `seccomp = "sometimes"`},
		{"invalid network origin", `network_allow = ["ssh://example.com"]`},
		{"invalid instance", `instance = "has spaces"`},
		{"reserved instance", `instance = ".deleting-private"`},
		{"invalid env type", "[env]\nFOO = true"},
		{"false inheritance", "[env]\nFOO = { inherit = false }"},
		{"inheritance option", "[env]\nFOO = { inherit = true, required = true }"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			writeTestFile(t, path, test.content)
			if _, found, err := loadConfigFile(path); err == nil || found {
				t.Fatalf("loadConfigFile unexpectedly succeeded: found=%v err=%v", found, err)
			}
		})
	}
}

func TestConfigurationRecoveryFlags(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	userDirectory := filepath.Join(root, "config", "bwrap-agent")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(userDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	userPath := filepath.Join(userDirectory, "config.toml")
	projectPath := filepath.Join(project, ".bwrap-agent.toml")
	writeTestFile(t, userPath, `network = "host"`)
	writeTestFile(t, projectPath, `this is not toml`)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))

	parse := func(extra ...string) (Options, int, error) {
		args := append([]string{"run", "--project", project}, extra...)
		args = append(args, "/bin/true")
		return parseOptions(args, &bytes.Buffer{}, &bytes.Buffer{})
	}
	if _, code, err := parse(); err == nil || code != 2 {
		t.Fatalf("malformed project config succeeded: code=%d err=%v", code, err)
	}
	opts, code, err := parse("--no-project-config")
	if err != nil || code != 0 || opts.Network != "host" || len(opts.ConfigFiles) != 1 {
		t.Fatalf("--no-project-config result: %#v code=%d err=%v", opts, code, err)
	}
	writeTestFile(t, userPath, `this is not toml`)
	opts, code, err = parse("--no-config")
	if err != nil || code != 0 || opts.Network != "private" || len(opts.ConfigFiles) != 0 {
		t.Fatalf("--no-config result: %#v code=%d err=%v", opts, code, err)
	}
}

func TestUserConfigPathUsesXDGAndHomeFallback(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "xdg"))
	path, err := userConfigPath()
	if err != nil || path != filepath.Join(root, "xdg", "bwrap-agent", "config.toml") {
		t.Fatalf("XDG user config path = %q, %v", path, err)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", filepath.Join(root, "home"))
	path, err = userConfigPath()
	if err != nil || path != filepath.Join(root, "home", ".config", "bwrap-agent", "config.toml") {
		t.Fatalf("home user config path = %q, %v", path, err)
	}
}

func TestProjectConfigDoesNotSearchAncestors(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "parent", "child")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(root, "parent", ".bwrap-agent.toml"), `network = "host"`)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "empty-config"))
	opts, code, err := parseOptions([]string{"run", "--project", project, "/bin/true"}, &bytes.Buffer{}, &bytes.Buffer{})
	if err != nil || code != 0 || opts.Network != "private" || len(opts.ConfigFiles) != 0 {
		t.Fatalf("ancestor config was loaded: %#v code=%d err=%v", opts, code, err)
	}
}

func TestConfigFileMustBeRegular(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, found, err := loadConfigFile(path); err == nil || found {
		t.Fatalf("directory config unexpectedly loaded: found=%v err=%v", found, err)
	}
}

func TestHelpDoesNotLoadConfiguration(t *testing.T) {
	configDirectory := filepath.Join(t.TempDir(), "bwrap-agent")
	if err := os.MkdirAll(configDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(configDirectory, "config.toml"), `this is not toml`)
	t.Setenv("XDG_CONFIG_HOME", filepath.Dir(configDirectory))
	var stdout, stderr bytes.Buffer
	_, code, err := parseOptions([]string{"run", "--help"}, &stdout, &stderr)
	if !errorsIs(err, errCLIHandled) || code != 0 || !strings.Contains(stdout.String(), "Usage:") {
		t.Fatalf("help result: code=%d err=%v stdout=%q stderr=%q", code, err, stdout.String(), stderr.String())
	}
}

func TestAgentConfigFlagsAndEmptyEnvironment(t *testing.T) {
	for _, test := range []struct {
		args            []string
		agentConfig     string
		environmentWant []string
	}{
		{[]string{"--agent-config", "off", "--env", "EMPTY=", "/bin/true"}, "off", []string{"EMPTY="}},
		{[]string{"--agent-config", "auto", "/bin/true"}, "auto", nil},
	} {
		args := append([]string{"run", "--no-config"}, test.args...)
		opts, code, err := parseOptions(args, &bytes.Buffer{}, &bytes.Buffer{})
		if err != nil || code != 0 || opts.AgentConfig != test.agentConfig || !reflect.DeepEqual(opts.Env, test.environmentWant) {
			t.Fatalf("parseOptions(%q) = %#v code=%d err=%v", args, opts, code, err)
		}
	}
}

func TestDryRunJSONIncludesConfigurationFiles(t *testing.T) {
	plan := LaunchPlan{
		ConfigFiles: []ConfigSource{{Scope: "project", Path: "/work/.bwrap-agent.toml"}},
		Seccomp:     SeccompStatus{Requested: "auto", Effective: "enabled", Profile: seccompProfileDevelopment},
	}
	var output bytes.Buffer
	if err := writePlanJSON(&output, plan); err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		ConfigFiles []ConfigSource `json:"config_files"`
		Seccomp     SeccompStatus  `json:"seccomp"`
	}
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded.ConfigFiles, plan.ConfigFiles) {
		t.Fatalf("config files = %#v, want %#v", decoded.ConfigFiles, plan.ConfigFiles)
	}
	if decoded.Seccomp != plan.Seccomp {
		t.Fatalf("seccomp status = %#v, want %#v", decoded.Seccomp, plan.Seccomp)
	}
}

func TestDryRunJSONUsesEmptyConfigFileArray(t *testing.T) {
	var output bytes.Buffer
	if err := writePlanJSON(&output, LaunchPlan{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"config_files": []`) {
		t.Fatalf("config_files is not an empty array: %s", output.String())
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAgentConfigModesAndPrecedence(t *testing.T) {
	project, config := t.TempDir(), t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", config)
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	for _, scenario := range []struct {
		name, user, project, cli, want string
	}{
		{name: "default", want: "auto"},
		{name: "user", user: "opencode", want: "opencode"},
		{name: "project", user: "opencode", project: "pi", want: "pi"},
		{name: "cli", user: "opencode", project: "pi", cli: "off", want: "off"},
		{name: "cli-auto", project: "off", cli: "auto", want: "auto"},
		{name: "cli-opencode", cli: "opencode", want: "opencode"},
		{name: "cli-pi", cli: "pi", want: "pi"},
		{name: "cli-codex", cli: "codex", want: "codex"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			content := func(mode string) string {
				if mode == "" {
					return ""
				}
				return `agent_config = "` + mode + `"`
			}
			writeAgentTestFile(t, filepath.Join(config, "bwrap-agent", "config.toml"), content(scenario.user))
			writeAgentTestFile(t, filepath.Join(project, projectConfigName), content(scenario.project))
			trustTestConfig(t, project)
			args := []string{"run", "--project", project}
			if scenario.cli != "" {
				args = append(args, "--agent-config", scenario.cli)
			}
			args = append(args, "tmux", "opencode", "--agent-config", "off")
			opts, code, err := parseOptions(args, &bytes.Buffer{}, &bytes.Buffer{})
			if err != nil || code != 0 || opts.AgentConfig != scenario.want {
				t.Fatalf("options=%#v code=%d err=%v", opts, code, err)
			}
			if !reflect.DeepEqual(opts.Command, []string{"tmux", "opencode", "--agent-config", "off"}) {
				t.Fatalf("command arguments changed: %q", opts.Command)
			}
		})
	}
}

func TestAgentConfigRejectsInvalidAndLegacyValues(t *testing.T) {
	for _, value := range []string{`true`, `false`, `""`, `"unknown"`, `"AUTO"`, `1`} {
		if _, err := decodeConfig(strings.NewReader("agent_config = "+value), t.TempDir()); err == nil {
			t.Errorf("accepted agent_config = %s", value)
		}
	}
	for _, flags := range [][]string{
		{"--no-agent-config"}, {"--agent-config"}, {"--agent-config=true"},
		{"--agent-config=false"}, {"--agent-config="}, {"--agent-config", "unknown"},
	} {
		args := append([]string{"run", "--no-config"}, flags...)
		args = append(args, "/bin/true")
		if _, _, err := parseOptions(args, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
			t.Errorf("accepted legacy or invalid flags: %q", flags)
		}
	}
}
