package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
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
	if err := os.MkdirAll(filepath.Join(state, "config", "etc"), 0o700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(root, "victim")
	if err := os.WriteFile(victim, []byte("do not overwrite"), 0o600); err != nil {
		t.Fatal(err)
	}
	generated := filepath.Join(state, "config", "etc", "resolv.conf")
	if err := os.Symlink(victim, generated); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareGeneratedEtc(state, "private", false, "agent-test"); err != nil {
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

func TestMountExternalEtcLinkTarget(t *testing.T) {
	root := t.TempDir()
	etc := filepath.Join(root, "etc")
	run := filepath.Join(root, "run")
	if err := os.MkdirAll(etc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(run, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(run, "resolv.conf")
	if err := os.WriteFile(target, []byte("nameserver 192.0.2.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(etc, "resolv.conf")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	mounts := mountBuilder{made: map[string]bool{"/": true}}
	if err := mountExternalEtcLinkTarget(&mounts, link); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(mounts.finish(), "\x00")
	want := "--ro-bind\x00" + target + "\x00" + target
	if !strings.Contains(joined, want) {
		t.Fatalf("external resolver target was not mounted: %#v", mounts.args)
	}
}

func TestMountExternalEtcLinkTargetSkipsDanglingSymlink(t *testing.T) {
	root := t.TempDir()
	link := filepath.Join(root, "resolv.conf")
	if err := os.Symlink(filepath.Join(root, "missing", "resolv.conf"), link); err != nil {
		t.Fatal(err)
	}
	mounts := mountBuilder{made: map[string]bool{"/": true}}
	if err := mountExternalEtcLinkTarget(&mounts, link); err != nil {
		t.Fatal(err)
	}
	if arguments := mounts.finish(); len(arguments) != 0 {
		t.Fatalf("dangling resolver target added mounts: %#v", arguments)
	}
}

func TestMountExternalEtcLinkTargetPreservesNamedTargetPath(t *testing.T) {
	root := t.TempDir()
	etc := filepath.Join(root, "etc")
	run := filepath.Join(root, "run")
	varDirectory := filepath.Join(root, "var")
	if err := os.MkdirAll(etc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(run, "NetworkManager"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(varDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	canonicalTarget := filepath.Join(run, "NetworkManager", "resolv.conf")
	if err := os.WriteFile(canonicalTarget, []byte("nameserver 192.0.2.2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "run"), filepath.Join(varDirectory, "run")); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(etc, "resolv.conf")
	linkTarget := filepath.Join("..", "var", "run", "NetworkManager", "resolv.conf")
	if err := os.Symlink(linkTarget, link); err != nil {
		t.Fatal(err)
	}
	mounts := mountBuilder{made: map[string]bool{"/": true}}
	if err := mountExternalEtcLinkTarget(&mounts, link); err != nil {
		t.Fatal(err)
	}
	namedTarget := filepath.Clean(filepath.Join(etc, linkTarget))
	joined := strings.Join(mounts.finish(), "\x00")
	want := "--ro-bind\x00" + canonicalTarget + "\x00" + namedTarget
	if !strings.Contains(joined, want) {
		t.Fatalf("resolver target mount = %#v, want %q", mounts.finish(), want)
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
	if err := os.Symlink(outside, filepath.Join(state, "podman")); err != nil {
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
	if opts.WorkspaceMode != "write-through" || opts.Landlock != "auto" || opts.Seccomp != "auto" {
		t.Fatalf("workspace/Landlock/seccomp defaults = %q/%q/%q", opts.WorkspaceMode, opts.Landlock, opts.Seccomp)
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
	for _, args := range [][]string{{"--"}, {"opencode"}, {"--init-config"}, {"config"}, {"run"}, {"run", "--podman", "invalid", "/bin/true"}, {"run", "--workspace-mode", "invalid", "/bin/true"}, {"run", "--landlock", "invalid", "/bin/true"}, {"run", "--seccomp", "invalid", "/bin/true"}, {"run", "--network-allow", "ssh://example.com", "/bin/true"}, {"run", "--network", "host", "--network-allow", "https://example.com", "/bin/true"}, {"run", "--no-podman", "/bin/true"}, {"run", "--podman-socket", "/bin/true"}, {"run", "--no-git-common-dir", "/bin/true"}, {"run", "--write-policy", "workspace", "/bin/true"}} {
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
		"--network-allow=ORIGIN",
		"--podman=auto|on|off",
		"--workspace-mode=write-through|copy-on-write|read-only",
		"--landlock=auto|required|off",
		"--seccomp=auto|required|off",
		"--allow-control-file-writes",
		"--tty=auto|always|never",
		"private (HTTP/HTTPS allowlist enforced), host (shared and unrestricted), or none (disabled)",
		"auto (enable if compatible and found), on (require), or off (disable)",
		"auto (when stdin and stdout are terminals), always, or never",
		"HOST_PORT=0 chooses a free port",
		"inherit NAME from the host",
	} {
		if !strings.Contains(normalizedHelp, expected) {
			t.Errorf("help does not contain %q:\n%s", expected, help)
		}
	}
	for _, obsolete := range []string{"--no-podman", "--podman-socket", "--no-git-common-dir", "--state-dir", "--write-policy"} {
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
		{[]string{"config", "create", "--help"}, []string{"project", "user"}},
		{[]string{"config", "create", "project", "--help"}, []string{"Create a documented .bwrap-agent.toml"}},
		{[]string{"config", "create", "user", "--help"}, []string{"current user across all projects"}},
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
		runGit(t, arguments...)
	}
	if err := os.WriteFile(filepath.Join(repository, "tracked"), []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, arguments := range [][]string{
		{"-C", repository, "add", "tracked"},
		{"-C", repository, "commit", "-m", "initial"},
		{"-C", repository, "worktree", "add", "--detach", worktree, "HEAD"},
	} {
		runGit(t, arguments...)
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
		Network: "host", Podman: "off", WorkspaceMode: "read-only", TTY: "never", Command: []string{"/bin/true"},
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
	plan, err := BuildPlan(Options{
		Project: ".", Instance: "plan-test", Network: "host", Podman: "off", Seccomp: "required", TTY: "never", Command: []string{"/bin/true"},
		Env: []string{"BWRAP_AGENT_PODMAN=1"},
	})
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
	if plan.Seccomp.Effective != "enabled" || plan.Seccomp.Profile != seccompProfileDevelopment || !strings.Contains(joined, "--seccomp\x008") {
		t.Fatalf("development seccomp profile is missing: %#v", plan)
	}
	if len(plan.Launcher) != 9 || plan.Launcher[2] != "--seccomp-profile" || plan.Launcher[3] != seccompProfileDevelopment || plan.Launcher[8] != "--" {
		t.Fatalf("development seccomp descriptor source is missing: %#v", plan.Launcher)
	}
}

func TestBuildPlanMountsPasswdSafeHomeAlias(t *testing.T) {
	stateHome := filepath.Join(t.TempDir(), "state:home")
	t.Setenv("BWRAP_AGENT_STATE_HOME", stateHome)
	plan, err := BuildPlan(Options{
		Project: ".", Instance: "passwd-home-alias", Network: "host", Podman: "off", TTY: "never", Command: []string{"/bin/true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	realHome := filepath.Join(plan.State, "home")
	if plan.LaunchEnv["HOME"] != realHome {
		t.Fatalf("HOME = %q, want %q", plan.LaunchEnv["HOME"], realHome)
	}
	passwd, err := os.ReadFile(filepath.Join(plan.State, "config", "etc", "passwd"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(passwd), ":"+sandboxPasswdHome+":/bin/sh") {
		t.Fatalf("generated passwd omitted safe home alias: %q", passwd)
	}
	if joined := strings.Join(plan.Bwrap, "\x00"); !strings.Contains(joined, "--bind\x00"+realHome+"\x00"+sandboxPasswdHome) {
		t.Fatalf("passwd home alias is not mounted: %#v", plan.Bwrap)
	}
}

func TestSeccompOffOmitsFilter(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	plan, err := BuildPlan(Options{
		Project: ".", Instance: "seccomp-off", Network: "host", Podman: "off", Seccomp: "off",
		TTY: "never", Command: []string{"/bin/true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Seccomp.Effective != "off" || plan.Seccomp.Profile != "" {
		t.Fatalf("seccomp status = %#v", plan.Seccomp)
	}
	if strings.Contains(strings.Join(plan.Bwrap, "\x00"), "--seccomp") {
		t.Fatalf("disabled seccomp remained in Bubblewrap plan: %#v", plan.Bwrap)
	}
	if len(plan.Launcher) != 7 || plan.Launcher[6] != "--" {
		t.Fatalf("disabled seccomp remained in launcher plan: %#v", plan.Launcher)
	}
}

func TestReadOnlyWorkspaceMode(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	plan, err := BuildPlan(Options{
		Project: ".", Instance: "read-only-test", Network: "host", Podman: "off",
		WorkspaceMode: "read-only", TTY: "never", Command: []string{"/bin/true"},
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
	if plan.WorkspaceMode != "read-only" || plan.LaunchEnv["GIT_OPTIONAL_LOCKS"] != "0" {
		t.Fatalf("unexpected read-only plan: %#v", plan)
	}
	var output bytes.Buffer
	if err := writePlanJSON(&output, plan); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"workspace_mode": "read-only"`) {
		t.Fatalf("dry-run JSON omitted workspace mode: %s", output.String())
	}
}

func TestReadOnlyWorkspaceAllowsExplicitRWBind(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	extra := t.TempDir()
	plan, err := BuildPlan(Options{
		Project: ".", Instance: "read-only-rw-bind", Network: "host", Podman: "off",
		WorkspaceMode: "read-only", RWBind: []string{extra}, TTY: "never", Command: []string{"/bin/true"},
	})
	if err != nil || !strings.Contains(strings.Join(plan.Bwrap, "\x00"), "--bind\x00"+extra+"\x00"+extra) {
		t.Fatalf("explicit writable bind was not preserved: %v %#v", err, plan.Bwrap)
	}
}

func TestCopyOnWriteWorkspaceUsesTemporaryOverlays(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	plan, err := BuildPlan(Options{
		Project: ".", Instance: "copy-on-write-plan", Network: "host", Podman: "off",
		WorkspaceMode: "copy-on-write", TTY: "never", Command: []string{"/bin/true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	project, err := resolveExisting(".")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan.Bwrap, "\x00")
	if !strings.Contains(joined, "--overlay-src\x00"+project+"\x00--tmp-overlay\x00"+project) {
		t.Fatalf("copy-on-write workspace overlay missing: %#v", plan.Bwrap)
	}
	if len(plan.ProtectedPaths) != 0 {
		t.Fatalf("copy-on-write mode should not add persistent control-path masks: %#v", plan.ProtectedPaths)
	}
}

func TestDestinationDirectoriesPrecedeUntrustedWorkspaceMounts(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	plan, err := BuildPlan(Options{
		Project: ".", Instance: "mount-order", Network: "private", Podman: "off",
		TTY: "never", Command: []string{"/bin/true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	project, err := resolveExisting(".")
	if err != nil {
		t.Fatal(err)
	}
	workspaceIndex := -1
	for index := 0; index+2 < len(plan.Bwrap); index++ {
		if plan.Bwrap[index] == "--bind" && plan.Bwrap[index+1] == project && plan.Bwrap[index+2] == project {
			workspaceIndex = index
			break
		}
	}
	if workspaceIndex < 0 {
		t.Fatalf("workspace mount missing: %#v", plan.Bwrap)
	}
	for index := workspaceIndex + 3; index < len(plan.Bwrap); index++ {
		if plan.Bwrap[index] == "--dir" {
			t.Fatalf("destination directory emitted after workspace mount at indexes %d and %d: %#v", workspaceIndex, index, plan.Bwrap)
		}
	}
}

func TestReadOnlyAndRequiredLandlockResolveAutomaticPodmanOff(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	tests := []Options{
		{Project: ".", Instance: "readonly-auto-podman", Network: "host", Podman: "auto", WorkspaceMode: "read-only", TTY: "never", Command: []string{"/bin/true"}},
	}
	if abi, err := queryLandlockABI(); err == nil && abi >= minimumLandlockABI {
		tests = append(tests, Options{Project: ".", Instance: "required-auto-podman", Network: "host", Podman: "auto", Landlock: "required", TTY: "never", Command: []string{"/bin/true"}})
	} else {
		t.Logf("required-Landlock BuildPlan case skipped: ABI=%d error=%v", abi, err)
	}
	for _, test := range tests {
		plan, err := BuildPlan(test)
		if err != nil {
			t.Fatal(err)
		}
		if plan.LaunchEnv["BWRAP_AGENT_PODMAN"] != "0" || len(plan.Outer) != 0 {
			t.Fatalf("automatic Podman was not disabled: %#v", plan)
		}
		if test.Landlock == "required" && plan.Landlock.Effective != "enabled" {
			t.Fatalf("required Landlock was not enabled: %#v", plan.Landlock)
		}
	}
}

func TestReadOnlyAndRequiredLandlockRejectExplicitPodman(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	for _, test := range []Options{
		{Project: ".", Instance: "readonly-on-podman", Network: "host", Podman: "on", WorkspaceMode: "read-only", TTY: "never", Command: []string{"/bin/true"}},
		{Project: ".", Instance: "required-on-podman", Network: "host", Podman: "on", Landlock: "required", TTY: "never", Command: []string{"/bin/true"}},
	} {
		if _, err := BuildPlan(test); err == nil || !strings.Contains(err.Error(), "incompatible") {
			t.Fatalf("incompatible Podman selection result: %v", err)
		}
	}
}

func TestInternalLandlockEnvironmentIsLauncherOwned(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	for _, test := range []Options{
		{Project: ".", Instance: "landlock-env-off", Network: "host", Podman: "off", Landlock: "off", TTY: "never", Command: []string{"/bin/true"}, Env: []string{internalLandlockEnvironment + "=invalid"}},
		{Project: ".", Instance: "landlock-env-podman", Network: "host", Podman: "on", Landlock: "auto", TTY: "never", Command: []string{"/bin/true"}, Env: []string{internalLandlockEnvironment + "=invalid"}},
	} {
		plan, err := BuildPlan(test)
		if err != nil {
			t.Fatal(err)
		}
		if _, exists := plan.LaunchEnv[internalLandlockEnvironment]; exists {
			t.Fatalf("user-controlled internal Landlock policy survived with status %#v", plan.Landlock)
		}
	}

	abi, err := queryLandlockABI()
	if err != nil || abi < minimumLandlockABI {
		t.Logf("enabled Landlock policy case skipped: ABI=%d error=%v", abi, err)
		return
	}
	plan, err := BuildPlan(Options{
		Project: ".", Instance: "landlock-env-enabled", Network: "host", Podman: "off", Landlock: "required",
		TTY: "never", Command: []string{"/bin/true"}, Env: []string{internalLandlockEnvironment + "=invalid"},
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, exists := plan.LaunchEnv[internalLandlockEnvironment]
	if !exists || encoded == "invalid" || !json.Valid([]byte(encoded)) {
		t.Fatalf("enabled Landlock policy was not launcher-generated: %q", encoded)
	}
}

func TestReadOnlyGitOptionalLocksCanBeOverridden(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	base := Options{
		Project: ".", Instance: "read-only-env", Network: "host", Podman: "off",
		WorkspaceMode: "read-only", TTY: "never", Command: []string{"/bin/true"},
	}
	base.Env = []string{"GIT_OPTIONAL_LOCKS=1"}
	plan, err := BuildPlan(base)
	if err != nil {
		t.Fatal(err)
	}
	if plan.LaunchEnv["GIT_OPTIONAL_LOCKS"] != "1" {
		t.Fatalf("explicit environment did not override default: %#v", plan.LaunchEnv)
	}
	base.Instance = "read-only-env-unset"
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
	base := Options{
		Project: ".", Instance: "podman-plan", Network: "host", Podman: "on", Seccomp: "required", TTY: "never", Command: []string{"/bin/true"},
		UnsetEnv: []string{"BWRAP_AGENT_PODMAN"},
	}
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
	if plan.LaunchEnv["PODMAN_NO_PAUSE_PROCESS"] != "1" {
		t.Fatalf("outer Podman pause-process mode = %q", plan.LaunchEnv["PODMAN_NO_PAUSE_PROCESS"])
	}
	if !strings.Contains(strings.Join(plan.Bwrap, "\x00"), "--setenv\x00PODMAN_NO_PAUSE_PROCESS\x001") {
		t.Fatalf("sandbox Podman pause-process mode is missing: %#v", plan.Bwrap)
	}
	if !strings.Contains(strings.Join(plan.Bwrap, "\x00"), "--setenv\x00XDG_RUNTIME_DIR\x00/run/bwrap-agent/runtime") {
		t.Fatalf("sandbox runtime directory is not private and short: %#v", plan.Bwrap)
	}
	joined := strings.Join(plan.Bwrap, "\x00")
	if plan.Seccomp.Effective != "enabled" || plan.Seccomp.Profile != seccompProfilePodman || !strings.Contains(joined, "--seccomp\x008") {
		t.Fatalf("Podman seccomp profile is missing: %#v", plan)
	}
	if !strings.Contains(joined, "/run/bwrap-agent/init\x00"+internalPodmanInitMode) || strings.Contains(joined, "/run/bwrap-agent/init\x00"+internalInitMode) {
		t.Fatalf("Podman plan did not select the protected init mode: %#v", plan.Bwrap)
	}
	for _, path := range []string{"/usr"} {
		if !strings.Contains(joined, "--ro-bind\x00"+path+"\x00"+path) {
			t.Errorf("read-only system view missing for %s: %#v", path, plan.Bwrap)
		}
	}
	if strings.Contains(joined, "--ro-bind\x00/etc\x00/etc") || strings.Contains(joined, "--ro-bind\x00/opt\x00/opt") || strings.Contains(joined, "--ro-bind\x00/nix/store\x00/nix/store") {
		t.Fatalf("broad system view remained in plan: %#v", plan.Bwrap)
	}
	for fd, destination := range map[string]string{"4": "/etc/passwd", "5": "/etc/group", "6": "/etc/nsswitch.conf", "7": "/etc/hosts"} {
		if !strings.Contains(joined, "--perms\x000444\x00--ro-bind-data\x00"+fd+"\x00"+destination) {
			t.Errorf("generated system file missing for %s: %#v", destination, plan.Bwrap)
		}
	}
	if !strings.Contains(joined, "--bind\x00/sys\x00/sys") {
		t.Fatalf("Podman-compatible sysfs bind missing: %#v", plan.Bwrap)
	}
	if len(plan.ProtectedPaths) == 0 {
		t.Fatal("write-through Podman plan omitted protected control paths")
	}
	if !strings.Contains(joined, "--perms\x000555\x00--ro-bind-data\x003\x00/run/bwrap-agent/init") || strings.Contains(joined, "--file") || len(plan.Launcher) != 9 || plan.Launcher[1] != internalLaunchMode || plan.Launcher[2] != "--seccomp-profile" || plan.Launcher[3] != seccompProfilePodman || plan.Launcher[8] != "--" {
		t.Fatalf("fd-backed sandbox init is missing: %#v", plan.Bwrap)
	}
	containersConfig, err := os.ReadFile(filepath.Join(plan.State, "podman", "config", "containers.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(containersConfig, []byte("host.containers.internal")) {
		t.Fatalf("host-network Podman config unexpectedly contains private proxy settings: %s", containersConfig)
	}
	if bytes.Contains(containersConfig, []byte("label = false")) {
		t.Fatalf("write-through Podman unexpectedly disabled container labeling: %s", containersConfig)
	}
}

func TestPodmanPlanInjectsExternalCommandReadOnly(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	executable := filepath.Join(t.TempDir(), "external-command")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(Options{
		Project: ".", Instance: "podman-external-command", Network: "host", Podman: "on",
		Seccomp: "required", TTY: "never", Command: []string{executable},
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan.Bwrap, "\x00")
	if !strings.Contains(joined, "--perms\x000555\x00--ro-bind-data\x008\x00/run/bwrap-agent/command") {
		t.Fatalf("external executable is not injected read-only: %#v", plan.Bwrap)
	}
	if !strings.Contains(joined, "--seccomp\x009") {
		t.Fatalf("seccomp descriptor collided with external executable: %#v", plan.Bwrap)
	}
	if len(plan.Launcher) != 10 || plan.Launcher[2] != "--seccomp-profile" || plan.Launcher[3] != seccompProfilePodman || plan.Launcher[8] != executable || plan.Launcher[9] != "--" {
		t.Fatalf("external executable descriptor source missing: %#v", plan.Launcher)
	}
}

func TestPodmanPlanSupportsReadOnlyUnixSocket(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "agent.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	plan, err := BuildPlan(Options{
		Project: ".", Instance: "podman-socket-bind", Network: "host", Podman: "on",
		ROBind: []string{path}, TTY: "never", Command: []string{"/bin/true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(plan.Bwrap, "\x00"), "--ro-bind\x00"+path+"\x00"+path) {
		t.Fatalf("read-only socket bind missing: %#v", plan.Bwrap)
	}
}

func TestCopyOnWritePodmanDisablesNestedSELinuxLabeling(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	plan, err := BuildPlan(Options{
		Project: ".", Instance: "podman-copy-on-write-label", Network: "host", Podman: "on",
		WorkspaceMode: "copy-on-write", TTY: "never", Command: []string{"/bin/true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	containersConfig, err := os.ReadFile(filepath.Join(plan.State, "podman", "config", "containers.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(containersConfig, []byte("label = false")) {
		t.Fatalf("copy-on-write Podman did not disable incompatible labeling: %s", containersConfig)
	}
}

func TestPrivatePodmanPlanConfiguresContainerProxyRoute(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	plan, err := BuildPlan(Options{Project: ".", Instance: "private-podman-plan", Network: "private", Podman: "on", TTY: "never", Command: []string{"/bin/true"}})
	if err != nil {
		t.Fatal(err)
	}
	containersConfig, err := os.ReadFile(filepath.Join(plan.State, "podman", "config", "containers.conf"))
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"http_proxy = false", "host.containers.internal:65532", "--map-host-loopback", "169.254.1.2"} {
		if !bytes.Contains(containersConfig, []byte(expected)) {
			t.Errorf("private Podman config lacks %q: %s", expected, containersConfig)
		}
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
	plan, err := BuildPlan(Options{Project: ".", Instance: "port-test", Network: "private", NetworkAllow: []string{"https://registry.example"}, Podman: "off", TTY: "never", Publish: []string{"18080:8080", "15432:5432/udp"}, Command: []string{"/bin/true"}})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan.Outer, "\x00")
	for _, expected := range []string{"127.0.0.1/18080:8080", "127.0.0.1/15432:5432"} {
		if !strings.Contains(joined, expected) {
			t.Errorf("missing %q in %#v", expected, plan.Outer)
		}
	}
	for _, expected := range []string{
		"--splice-only", strconv.Itoa(proxyGuestPort) + ":" + proxyPortPlaceholder,
		strconv.Itoa(dnsGuestPort) + ":" + dnsPortPlaceholder,
	} {
		if !strings.Contains(joined, expected) {
			t.Errorf("missing %q in %#v", expected, plan.Outer)
		}
	}
	if strings.Contains(joined, "--host-lo-to-ns-lo") || strings.Contains(joined, "--dns-forward") {
		t.Fatalf("private plan exposes unintended host networking: %#v", plan.Outer)
	}
	if len(plan.Launcher) < 5 || plan.Launcher[0] != "/bin/sh" || plan.Launcher[1] != "-c" || plan.Launcher[2] != `exec "$0" "$@"` || plan.Launcher[4] != internalLaunchMode {
		t.Fatalf("private launcher does not use the system trampoline: %#v", plan.Launcher)
	}
	proxyURL := fmt.Sprintf("http://127.0.0.1:%d", proxyGuestPort)
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		if plan.LaunchEnv[name] != proxyURL {
			t.Errorf("%s = %q, want %q", name, plan.LaunchEnv[name], proxyURL)
		}
	}
	if plan.ProxyGuestPort != proxyGuestPort || !reflect.DeepEqual(plan.NetworkAllow, []string{"https://registry.example"}) {
		t.Fatalf("private policy metadata = %#v", plan)
	}
}

func TestSystemShellIgnoresPATH(t *testing.T) {
	directory := t.TempDir()
	untrusted := filepath.Join(directory, "sh")
	if err := os.WriteFile(untrusted, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory)

	found, err := requireSystemShell()
	if err != nil {
		t.Fatal(err)
	}
	if found != "/bin/sh" {
		t.Fatalf("system shell = %q, want /bin/sh", found)
	}
}

func TestHostPlanDoesNotUseLauncherTrampoline(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	plan, err := BuildPlan(Options{
		Project: ".", Instance: "host-launcher", Network: "host", Podman: "off", TTY: "never", Command: []string{"/bin/true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Launcher) < 2 || plan.Launcher[1] != internalLaunchMode || filepath.Base(plan.Launcher[0]) == "sh" {
		t.Fatalf("host launcher unexpectedly uses a trampoline: %#v", plan.Launcher)
	}
}

func TestNetworkAllowModeCompatibility(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	_, err := BuildPlan(Options{Project: ".", Instance: "host-allow-test", Network: "host", NetworkAllow: []string{"https://example.com"}, Podman: "off", TTY: "never", Command: []string{"/bin/true"}})
	if err == nil || !strings.Contains(err.Error(), "cannot be used with --network=host") {
		t.Fatalf("host allowlist error = %v", err)
	}
	plan, err := BuildPlan(Options{Project: ".", Instance: "none-allow-test", Network: "none", NetworkAllow: []string{"https://example.com"}, Podman: "off", TTY: "never", Command: []string{"/bin/true"}})
	if err != nil {
		t.Fatal(err)
	}
	if plan.ProxyGuestPort != 0 || strings.Contains(strings.Join(plan.Bwrap, "\x00"), "HTTP_PROXY") {
		t.Fatalf("none mode activated proxy support: %#v", plan)
	}
}

func TestPodmanSupervisorUsesHostAccount(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	t.Setenv("USER", "")
	t.Setenv("LOGNAME", "")
	if err := os.Unsetenv("USER"); err != nil {
		t.Fatal(err)
	}
	if err := os.Unsetenv("LOGNAME"); err != nil {
		t.Fatal(err)
	}
	account, _ := currentAccount()
	for _, override := range []bool{false, true} {
		opts := Options{Project: ".", Instance: fmt.Sprintf("account-%t", override), Network: "host", Podman: "on", TTY: "never", Command: []string{"/bin/true"}}
		wantSandbox := account
		if override {
			opts.Env = []string{"USER=custom", "LOGNAME=custom"}
			wantSandbox = "custom"
		}
		plan, err := BuildPlan(opts)
		if err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"USER", "LOGNAME"} {
			if plan.LaunchEnv[key] != account {
				t.Errorf("supervisor %s = %q, want %q", key, plan.LaunchEnv[key], account)
			}
			if !strings.Contains(strings.Join(plan.Bwrap, "\x00"), "--setenv\x00"+key+"\x00"+wantSandbox+"\x00") {
				t.Errorf("sandbox %s does not match %q", key, wantSandbox)
			}
		}
	}
}

func TestSandboxDoesNotExposeFuseDevice(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	for _, mode := range []string{"on", "off"} {
		plan, err := BuildPlan(Options{Project: ".", Instance: "fuse-" + mode, Network: "host", Podman: mode, TTY: "never", Command: []string{"/bin/true"}})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(strings.Join(plan.Bwrap, "\x00"), "/dev/fuse") {
			t.Errorf("Podman %s: sandbox exposes the FUSE device", mode)
		}
	}
}

func TestSandboxUnsetsOuterEnvironment(t *testing.T) {
	for _, podman := range []string{"on", "off"} {
		for _, network := range []string{"host", "private", "none"} {
			t.Run(podman+"-"+network, func(t *testing.T) {
				t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
				removed := []string{"USER", "LOGNAME", "XDG_RUNTIME_DIR"}
				opts := Options{Project: ".", Instance: "unset-outer", Network: network, Podman: podman, TTY: "never", Command: []string{"/bin/true"},
					Env:      []string{"USER=custom", "LOGNAME=custom"},
					UnsetEnv: append(append([]string{}, removed...), "BWRAP_AGENT_PODMAN", "CONTAINERS_STORAGE_CONF"),
				}
				plan, err := BuildPlan(opts)
				if err != nil {
					t.Fatal(err)
				}
				joined := strings.Join(plan.Bwrap, "\x00")
				for _, key := range removed {
					if !strings.Contains(joined, "--unsetenv\x00"+key+"\x00") || strings.Contains(joined, "--setenv\x00"+key+"\x00") {
						t.Errorf("sandbox must unset %s", key)
					}
				}
				if plan.LaunchEnv["XDG_RUNTIME_DIR"] != filepath.Join(plan.State, "run") {
					t.Fatal("outer runtime directory lost")
				}
				if strings.Contains(joined, "--unsetenv\x00BWRAP_AGENT_PODMAN\x00") {
					t.Fatal("launcher-owned status was unset")
				}
				if podman == "on" {
					account, err := hostAccountName(os.Environ())
					if err != nil {
						t.Fatal(err)
					}
					for _, key := range []string{"USER", "LOGNAME"} {
						if plan.LaunchEnv[key] != account {
							t.Errorf("outer %s lost host identity", key)
						}
					}
					if strings.Contains(joined, "--unsetenv\x00CONTAINERS_STORAGE_CONF\x00") {
						t.Fatal("required Podman configuration was unset")
					}
				}
			})
		}
	}
}
