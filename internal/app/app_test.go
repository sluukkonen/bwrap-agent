package app

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParsePort(t *testing.T) {
	tests := []struct {
		input    string
		protocol string
		host     int
		guest    int
	}{
		{"3000", "tcp", 3000, 3000},
		{"15432:5432/udp", "udp", 15432, 5432},
	}
	for _, test := range tests {
		port, err := parsePort(test.input)
		if err != nil {
			t.Fatal(err)
		}
		if port.Protocol != test.protocol || port.Host != test.host || port.Guest != test.guest {
			t.Fatalf("parsePort(%q) = %#v", test.input, port)
		}
	}
	for _, invalid := range []string{"70000", "abc", "1:2:s", "3000/sctp"} {
		if _, err := parsePort(invalid); err == nil {
			t.Errorf("parsePort(%q) unexpectedly succeeded", invalid)
		}
	}
}

func TestNames(t *testing.T) {
	if _, err := safeName("worktree-1.foo"); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"", ".", "..", "../../escape", "snow-雪", ".deleting-example"} {
		if _, err := safeName(invalid); err == nil {
			t.Errorf("safeName(%q) unexpectedly succeeded", invalid)
		}
	}
	name := defaultName("/tmp/project name-雪")
	if _, err := safeName(name); err != nil {
		t.Fatalf("unsafe generated name %q: %v", name, err)
	}
	if name != "project-name" || len(name) > 80 {
		t.Fatalf("unexpected generated name %q", name)
	}
	if other := defaultName("/tmp/project name-雪"); other != name {
		t.Fatalf("default name is not stable: %q != %q", name, other)
	}
	if other := defaultName("/var/tmp/project name-雪"); other != name {
		t.Fatalf("matching basenames produced different default names: %q != %q", name, other)
	}
	if other := collisionName("/var/tmp/project name-雪"); other == collisionName("/tmp/project name-雪") || !strings.HasPrefix(other, name+"-") {
		t.Fatalf("unexpected collision name %q", other)
	}
}

func TestInstanceLockIsExclusiveAndReleased(t *testing.T) {
	root := t.TempDir()
	identity := instanceIdentity{Instance: "locked-project", Root: root, State: filepath.Join(root, "state")}
	first, err := acquireInstanceLock(identity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireInstanceLock(identity); !errorsIs(err, errInstanceBusy) {
		first.Close()
		t.Fatalf("second lock = %v, want errInstanceBusy", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := acquireInstanceLock(identity)
	if err != nil {
		t.Fatalf("lock was not released: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestInstanceLockFollowsManagedRootNotDisplayName(t *testing.T) {
	root := t.TempDir()
	first, err := acquireInstanceLock(instanceIdentity{Instance: "first", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := acquireInstanceLock(instanceIdentity{Instance: "second", Root: root}); !errorsIs(err, errInstanceBusy) {
		t.Fatalf("same root with a different name was not locked: %v", err)
	}
}

func TestStateFileReplacesSymlinkWithoutFollowingIt(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	if err := os.MkdirAll(filepath.Join(state, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(root, "victim")
	if err := os.WriteFile(victim, []byte("do not overwrite"), 0o600); err != nil {
		t.Fatal(err)
	}
	generated := filepath.Join(state, "config", "resolv.conf")
	if err := os.Symlink(victim, generated); err != nil {
		t.Fatal(err)
	}
	if _, err := writePrivateResolvConf(state); err != nil {
		t.Fatal(err)
	}
	content, _ := os.ReadFile(victim)
	if string(content) != "do not overwrite" {
		t.Fatalf("victim overwritten: %q", content)
	}
	info, err := os.Lstat(generated)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("generated file is still a symlink: %v", err)
	}
}

func TestStateFileRejectsSymlinkedParent(t *testing.T) {
	root := t.TempDir()
	state, outside := filepath.Join(root, "state"), filepath.Join(root, "outside")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(state, "config")); err != nil {
		t.Fatal(err)
	}
	if _, err := writeStorageConfig(state); err == nil {
		t.Fatal("writeStorageConfig unexpectedly followed a symlinked parent")
	}
	entries, _ := os.ReadDir(outside)
	if len(entries) != 0 {
		t.Fatalf("outside directory modified: %v", entries)
	}
}

func TestCLIParsingAndPassthrough(t *testing.T) {
	var stdout, stderr bytes.Buffer
	opts, code, err := parseOptions([]string{"run", "--network", "host", "podman", "run", "--rm", "--name=inside"}, &stdout, &stderr)
	if err != nil || code != 0 {
		t.Fatalf("parse failed: code=%d err=%v stderr=%q", code, err, stderr.String())
	}
	want := []string{"podman", "run", "--rm", "--name=inside"}
	if strings.Join(opts.Command, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("command = %#v, want %#v", opts.Command, want)
	}
	if opts.Network != "host" {
		t.Fatalf("network = %q", opts.Network)
	}
	if opts.Podman != "auto" {
		t.Fatalf("podman mode = %q, want auto", opts.Podman)
	}
	if opts.WritePolicy != "workspace" {
		t.Fatalf("write policy = %q, want workspace", opts.WritePolicy)
	}
	stdout.Reset()
	stderr.Reset()
	opts, code, err = parseOptions([]string{"run", "run", "--looks-like-a-launcher-flag"}, &stdout, &stderr)
	if err != nil || code != 0 || strings.Join(opts.Command, " ") != "run --looks-like-a-launcher-flag" {
		t.Fatalf("target named run was not passed through: %#v code=%d err=%v", opts.Command, code, err)
	}

	stdout.Reset()
	stderr.Reset()
	opts, code, err = parseOptions([]string{"run", "--", "bash", "-l"}, &stdout, &stderr)
	if err != nil || code != 0 || strings.Join(opts.Command, " ") != "bash -l" {
		t.Fatalf("optional separator failed: %#v code=%d err=%v", opts.Command, code, err)
	}
}

func TestCLIRejectsInvalidInputs(t *testing.T) {
	for _, args := range [][]string{{"--"}, {"opencode"}, {"--init-config"}, {"config"}, {"run"}, {"run", "--podman", "invalid", "/bin/true"}, {"run", "--write-policy", "invalid", "/bin/true"}, {"run", "--no-podman", "/bin/true"}, {"run", "--podman-socket", "/bin/true"}, {"run", "--no-git-common-dir", "/bin/true"}} {
		var stdout, stderr bytes.Buffer
		_, code, err := parseOptions(args, &stdout, &stderr)
		if err == nil || code != 2 {
			t.Fatalf("parseOptions(%q) = code %d, err %v", args, code, err)
		}
	}
}

func TestEmptyCLIShowsRootHelp(t *testing.T) {
	parse := func(args []string) (string, string, int, error) {
		var stdout, stderr bytes.Buffer
		_, code, err := parseCLI(args, &stdout, &stderr)
		return stdout.String(), stderr.String(), code, err
	}
	emptyOutput, emptyErrorOutput, emptyCode, emptyErr := parse(nil)
	helpOutput, helpErrorOutput, helpCode, helpErr := parse([]string{"--help"})
	if !errorsIs(emptyErr, errCLIHandled) || emptyCode != 0 {
		t.Fatalf("empty CLI: code=%d err=%v stderr=%q", emptyCode, emptyErr, emptyErrorOutput)
	}
	if !errorsIs(helpErr, errCLIHandled) || helpCode != 0 {
		t.Fatalf("root help: code=%d err=%v stderr=%q", helpCode, helpErr, helpErrorOutput)
	}
	if emptyOutput != helpOutput || emptyErrorOutput != "" || helpErrorOutput != "" {
		t.Fatalf("empty CLI differs from --help:\nempty stdout=%q\nhelp stdout=%q\nempty stderr=%q\nhelp stderr=%q", emptyOutput, helpOutput, emptyErrorOutput, helpErrorOutput)
	}
}

func TestHelpIsHandledWithoutBuildingPlan(t *testing.T) {
	var stdout, stderr bytes.Buffer
	_, code, err := parseOptions([]string{"run", "--help"}, &stdout, &stderr)
	help := stdout.String()
	if !errorsIs(err, errCLIHandled) || code != 0 || !strings.Contains(help, "Usage:") {
		t.Fatalf("help result: code=%d err=%v stdout=%q stderr=%q", code, err, stdout.String(), stderr.String())
	}
	normalizedHelp := strings.Join(strings.Fields(help), " ")
	for _, expected := range []string{
		"--instance=NAME",
		"--[no-]agent-config",
		"--no-config",
		"--no-project-config",
		"--network=private|host|none",
		"--podman=auto|on|off",
		"--write-policy=workspace|state-only",
		"--allow-control-file-writes",
		"--tty=auto|always|never",
		"private (isolated via pasta), host (shared), or none (disabled)",
		"auto (enable if found), on (require), or off (disable)",
		"auto (when stdin and stdout are terminals), always, or never",
		"HOST_PORT=0 chooses a free port",
		"inherit NAME from the host",
	} {
		if !strings.Contains(normalizedHelp, expected) {
			t.Errorf("help does not contain %q:\n%s", expected, help)
		}
	}
	for _, obsolete := range []string{"--no-podman", "--podman-socket", "--no-git-common-dir", "--state-dir"} {
		if strings.Contains(help, obsolete) {
			t.Errorf("help still contains obsolete option %q", obsolete)
		}
	}
}

func TestRootAndConfigHelpExposeCommands(t *testing.T) {
	for _, test := range []struct {
		args []string
		want []string
	}{
		{[]string{"--help"}, []string{"run", "config", "instance"}},
		{[]string{"config", "--help"}, []string{"create"}},
		{[]string{"config", "create", "--help"}, []string{"Create a documented .bwrap-agent.toml"}},
		{[]string{"instance", "--help"}, []string{"list", "delete"}},
		{[]string{"instance", "list", "--help"}, []string{"--json"}},
		{[]string{"instance", "delete", "--help"}, []string{"--yes", "<name>"}},
	} {
		var stdout, stderr bytes.Buffer
		_, code, err := parseCLI(test.args, &stdout, &stderr)
		if !errorsIs(err, errCLIHandled) || code != 0 {
			t.Fatalf("parseCLI(%q): code=%d err=%v stderr=%q", test.args, code, err, stderr.String())
		}
		for _, want := range test.want {
			if !strings.Contains(stdout.String(), want) {
				t.Errorf("parseCLI(%q) help does not contain %q:\n%s", test.args, want, stdout.String())
			}
		}
		if len(test.args) == 1 && strings.Contains(stdout.String(), "--network") {
			t.Errorf("root help exposes run-only flags:\n%s", stdout.String())
		}
	}
}

func TestLinkedWorktreeCommonDirectoryIsMounted(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BWRAP_AGENT_STATE_HOME", filepath.Join(root, "state-home"))
	repository := filepath.Join(root, "repository")
	worktree := filepath.Join(root, "worktree")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, arguments := range [][]string{
		{"init", repository},
		{"-C", repository, "config", "user.name", "Test User"},
		{"-C", repository, "config", "user.email", "test@example.invalid"},
	} {
		if output, err := exec.Command("git", arguments...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", arguments, err, output)
		}
	}
	if err := os.WriteFile(filepath.Join(repository, "tracked"), []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, arguments := range [][]string{
		{"-C", repository, "add", "tracked"},
		{"-C", repository, "commit", "-m", "initial"},
		{"-C", repository, "worktree", "add", "--detach", worktree, "HEAD"},
	} {
		if output, err := exec.Command("git", arguments...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", arguments, err, output)
		}
	}
	common, err := filepath.EvalSymlinks(filepath.Join(repository, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	if detected, err := validatedExternalGitCommonDir(worktree); err != nil || detected != common {
		t.Fatalf("common directory = %q, want %q", detected, common)
	}
	if detected, err := validatedExternalGitCommonDir(repository); err != nil || detected != "" {
		t.Fatalf("ordinary repository produced external common directory %q", detected)
	}
	plan, err := BuildPlan(Options{
		Project: worktree, Instance: "worktree-test",
		Network: "host", Podman: "off", TTY: "never", Command: []string{"/bin/true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantMount := "--bind\x00" + common + "\x00" + common
	if !strings.Contains(strings.Join(plan.Bwrap, "\x00"), wantMount) {
		t.Fatalf("external Git common directory was not mounted: %#v", plan.Bwrap)
	}
	readOnlyPlan, err := BuildPlan(Options{
		Project: worktree, Instance: "read-only-worktree-test",
		Network: "host", Podman: "off", WritePolicy: "state-only", TTY: "never", Command: []string{"/bin/true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantReadOnlyMount := "--ro-bind\x00" + common + "\x00" + common
	if !strings.Contains(strings.Join(readOnlyPlan.Bwrap, "\x00"), wantReadOnlyMount) {
		t.Fatalf("external Git common directory was not mounted read-only: %#v", readOnlyPlan.Bwrap)
	}
}

func errorsIs(err, target error) bool {
	return errors.Is(err, target)
}

func TestTerminalEnvironment(t *testing.T) {
	selected := terminalEnv([]string{"TERM=screen-256color", "LC_CTYPE=en_US.UTF-8", "OPENTUI_GRAPHICS=0", "API_TOKEN=secret", "SSH_AUTH_SOCK=/tmp/agent"})
	if len(selected) != 3 || selected["TERM"] != "screen-256color" || selected["LC_CTYPE"] != "en_US.UTF-8" || selected["OPENTUI_GRAPHICS"] != "0" {
		t.Fatalf("unexpected terminal environment: %#v", selected)
	}
}

func TestBuildPlanWithoutPodman(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	plan, err := BuildPlan(Options{Project: ".", Instance: "plan-test", Network: "host", Podman: "off", TTY: "never", Command: []string{"/bin/true"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Outer) != 0 || plan.LaunchEnv["BWRAP_AGENT_PODMAN"] != "0" {
		t.Fatalf("unexpected no-Podman plan: %#v", plan)
	}
	if _, exists := plan.LaunchEnv["BWRAP_AGENT_PODMAN_SOCKET"]; exists {
		t.Fatalf("obsolete socket mode leaked into plan: %#v", plan.LaunchEnv)
	}
	joined := strings.Join(plan.Bwrap, "\x00")
	if strings.Contains(joined, "/bin/sh") || !strings.Contains(joined, internalInitMode) || !strings.Contains(joined, "--new-session") {
		t.Fatalf("unexpected bwrap argv: %#v", plan.Bwrap)
	}
}

func TestStateOnlyWritePolicy(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	plan, err := BuildPlan(Options{
		Project: ".", Instance: "state-only-test", Network: "host", Podman: "off",
		WritePolicy: "state-only", TTY: "never", Command: []string{"/bin/true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	project, err := resolveExisting(".")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan.Bwrap, "\x00")
	state := plan.State
	for _, mount := range []string{
		"--ro-bind\x00" + project + "\x00" + project,
		"--bind\x00" + state + "\x00" + state,
	} {
		if !strings.Contains(joined, mount) {
			t.Errorf("missing mount %q in %#v", mount, plan.Bwrap)
		}
	}
	if plan.WritePolicy != "state-only" || plan.LaunchEnv["GIT_OPTIONAL_LOCKS"] != "0" {
		t.Fatalf("unexpected state-only plan: %#v", plan)
	}
	var output bytes.Buffer
	if err := writePlanJSON(&output, plan); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"write_policy": "state-only"`) {
		t.Fatalf("dry-run JSON omitted write policy: %s", output.String())
	}
}

func TestStateOnlyPolicyRejectsRWBind(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	_, err := BuildPlan(Options{
		Project: ".", Instance: "state-only-rw-bind", Network: "host", Podman: "off",
		WritePolicy: "state-only", RWBind: []string{"."}, TTY: "never", Command: []string{"/bin/true"},
	})
	if err == nil || !strings.Contains(err.Error(), "--rw-bind is incompatible") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestStateOnlyGitOptionalLocksCanBeOverridden(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	base := Options{
		Project: ".", Instance: "state-only-env", Network: "host", Podman: "off",
		WritePolicy: "state-only", TTY: "never", Command: []string{"/bin/true"},
	}
	base.Env = []string{"GIT_OPTIONAL_LOCKS=1"}
	plan, err := BuildPlan(base)
	if err != nil {
		t.Fatal(err)
	}
	if plan.LaunchEnv["GIT_OPTIONAL_LOCKS"] != "1" {
		t.Fatalf("explicit environment did not override default: %#v", plan.LaunchEnv)
	}
	base.Instance = "state-only-env-unset"
	base.Env = nil
	base.UnsetEnv = []string{"GIT_OPTIONAL_LOCKS"}
	plan, err = BuildPlan(base)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := plan.LaunchEnv["GIT_OPTIONAL_LOCKS"]; exists {
		t.Fatalf("--unsetenv did not remove default: %#v", plan.LaunchEnv)
	}
}

func TestEnabledPodmanPlan(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	base := Options{Project: ".", Instance: "podman-plan", Network: "host", Podman: "on", TTY: "never", Command: []string{"/bin/true"}}
	plan, err := BuildPlan(base)
	if err != nil {
		t.Fatal(err)
	}
	if plan.LaunchEnv["BWRAP_AGENT_PODMAN"] != "1" || filepath.Base(plan.Outer[0]) != "podman" || plan.Outer[1] != "unshare" {
		t.Fatalf("unexpected default Podman plan: %#v", plan)
	}
	if _, exists := plan.LaunchEnv["BWRAP_AGENT_PODMAN_SOCKET"]; exists {
		t.Fatalf("obsolete socket mode leaked into plan: %#v", plan.LaunchEnv)
	}
	if plan.LaunchEnv["XDG_RUNTIME_DIR"] != filepath.Join(plan.State, "run") {
		t.Fatalf("outer runtime directory = %q", plan.LaunchEnv["XDG_RUNTIME_DIR"])
	}
	if !strings.Contains(strings.Join(plan.Bwrap, "\x00"), "--setenv\x00XDG_RUNTIME_DIR\x00/run/bwrap-agent/runtime") {
		t.Fatalf("sandbox runtime directory is not private and short: %#v", plan.Bwrap)
	}
}

func TestResolvePodmanModes(t *testing.T) {
	emptyPath := t.TempDir()
	t.Setenv("PATH", emptyPath)
	if found, err := resolvePodman(""); err != nil || found != "" {
		t.Fatalf("auto without Podman = %q, %v", found, err)
	}
	if found, err := resolvePodman("off"); err != nil || found != "" {
		t.Fatalf("off = %q, %v", found, err)
	}
	if _, err := resolvePodman("on"); err == nil {
		t.Fatal("on without Podman unexpectedly succeeded")
	}
	if _, err := resolvePodman("invalid"); err == nil {
		t.Fatal("invalid mode unexpectedly succeeded")
	}
}

func TestStateHomeSymlinkIsResolved(t *testing.T) {
	root := t.TempDir()
	realState := filepath.Join(root, "real")
	if err := os.Mkdir(realState, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(realState, link); err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveState(filepath.Join(link, "child"))
	if err != nil {
		t.Fatal(err)
	}
	if resolved != filepath.Join(realState, "child") {
		t.Fatalf("resolved state = %q", resolved)
	}
}

func TestPrivatePortsBindHostLoopback(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	plan, err := BuildPlan(Options{Project: ".", Instance: "port-test", Network: "private", Podman: "off", TTY: "never", Publish: []string{"18080:8080", "15432:5432/udp"}, Command: []string{"/bin/true"}})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan.Outer, "\x00")
	for _, expected := range []string{"127.0.0.1/18080:8080", "127.0.0.1/15432:5432"} {
		if !strings.Contains(joined, expected) {
			t.Errorf("missing %q in %#v", expected, plan.Outer)
		}
	}
}
