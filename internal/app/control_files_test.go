package app

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func controlTestOptions(project, instance string) Options {
	return Options{
		Project: project, Instance: instance, Network: "host", Podman: "off",
		WritePolicy: "workspace", TTY: "never", Command: []string{"/bin/true"},
	}
}

func TestWorkspaceProtectsMissingControlPathsWithReadOnlyMasks(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	project := t.TempDir()
	plan, err := BuildPlan(controlTestOptions(project, "protected-missing"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		filepath.Join(project, projectConfigName),
		filepath.Join(project, "opencode.json"),
		filepath.Join(project, "opencode.jsonc"),
		filepath.Join(project, ".opencode"),
		filepath.Join(project, ".pi"),
	}
	joined := strings.Join(plan.ProtectedPaths, "\x00")
	bwrap := strings.Join(plan.Bwrap, "\x00")
	for _, path := range want {
		if !strings.Contains(joined, path) {
			t.Errorf("protected paths omit %s: %#v", path, plan.ProtectedPaths)
		}
		if !strings.Contains(bwrap, "--ro-bind\x00") || !strings.Contains(bwrap, "\x00"+path) {
			t.Errorf("bwrap plan does not mask %s: %#v", path, plan.Bwrap)
		}
	}
	if strings.Contains(joined, ".mcp.json") {
		t.Fatalf("Claude-specific .mcp.json was protected: %#v", plan.ProtectedPaths)
	}
	var output bytes.Buffer
	if err := writePlanJSON(&output, plan); err != nil {
		t.Fatal(err)
	}
	var dryRun struct {
		ProtectedPaths []string `json:"protected_paths"`
	}
	if err := json.Unmarshal(output.Bytes(), &dryRun); err != nil {
		t.Fatal(err)
	}
	if len(dryRun.ProtectedPaths) != len(plan.ProtectedPaths) {
		t.Fatalf("dry-run protected paths = %#v, want %#v", dryRun.ProtectedPaths, plan.ProtectedPaths)
	}
}

func TestExistingPiResourcesProtectOnlyControlSurfaces(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	project := t.TempDir()
	if err := os.MkdirAll(filepath.Join(project, ".pi", "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(controlTestOptions(project, "protected-pi"))
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan.ProtectedPaths, "\x00")
	for _, relative := range []string{".pi/settings.json", ".pi/extensions", ".pi/npm", ".pi/git"} {
		if !strings.Contains(joined, filepath.Join(project, relative)) {
			t.Errorf("missing Pi control protection %s: %#v", relative, plan.ProtectedPaths)
		}
	}
	if strings.Contains(joined, filepath.Join(project, ".pi", "prompts")) {
		t.Fatalf("editable Pi prompts were protected: %#v", plan.ProtectedPaths)
	}
}

func TestGitControlPathsAreProtected(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	project := t.TempDir()
	initializeGitRepository(t, project)
	plan, err := BuildPlan(controlTestOptions(project, "protected-git"))
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan.ProtectedPaths, "\x00")
	for _, path := range []string{
		filepath.Join(project, ".git", "config"),
		filepath.Join(project, ".git", "hooks"),
		filepath.Join(project, ".git", "config.worktree"),
	} {
		if !strings.Contains(joined, path) {
			t.Errorf("missing Git protection %s: %#v", path, plan.ProtectedPaths)
		}
	}
}

func TestControlProtectionRejectsSymlinkHop(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	project := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(project, ".opencode")); err != nil {
		t.Fatal(err)
	}
	_, err := BuildPlan(controlTestOptions(project, "protected-symlink"))
	if err == nil || !strings.Contains(err.Error(), "traverses a symlink") {
		t.Fatalf("symlink protection error = %v", err)
	}
}

func TestControlProtectionEscapeAndStateOnly(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	project := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(project, ".opencode")); err != nil {
		t.Fatal(err)
	}
	opts := controlTestOptions(project, "protected-escape")
	opts.AllowControlFileWrites = true
	plan, err := BuildPlan(opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.ProtectedPaths) != 0 {
		t.Fatalf("escape hatch retained protections: %#v", plan.ProtectedPaths)
	}
	opts.Instance = "protected-state-only"
	opts.AllowControlFileWrites = false
	opts.WritePolicy = "state-only"
	plan, err = BuildPlan(opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.ProtectedPaths) != 0 {
		t.Fatalf("state-only has redundant protections: %#v", plan.ProtectedPaths)
	}
}

func TestProjectConfigSymlinkRequiresEscapeHatch(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	project := t.TempDir()
	target := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(target, []byte("network = \"host\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(project, projectConfigName)); err != nil {
		t.Fatal(err)
	}
	projectArg := "--project=" + project
	var stdout, stderr bytes.Buffer
	if _, code, err := parseOptions([]string{"run", projectArg, "/bin/true"}, &stdout, &stderr); err == nil || code != 2 || !strings.Contains(err.Error(), "is a symlink") {
		t.Fatalf("project symlink config = code %d, err %v, stderr %q", code, err, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	opts, code, err := parseOptions([]string{"run", projectArg, "--allow-control-file-writes", "/bin/true"}, &stdout, &stderr)
	if err != nil || code != 0 || opts.Network != "host" || !opts.AllowControlFileWrites {
		t.Fatalf("escaped project symlink config = %#v, code %d, err %v, stderr %q", opts, code, err, stderr.String())
	}
}

func TestControlWriteEscapeIsNotAConfigOption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("allow_control_file_writes = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadConfigFile(path); err == nil {
		t.Fatalf("config escape option error = %v", err)
	}
}

func TestTemporaryControlPlaceholdersAreRemoved(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	project := t.TempDir()
	plan, err := BuildPlan(controlTestOptions(project, "placeholder-lifecycle"))
	if err != nil {
		t.Fatal(err)
	}
	control, err := newLaunchControlFiles(project)
	if err != nil {
		t.Fatal(err)
	}
	if err := control.prepare(plan.ControlCleanup); err != nil {
		t.Fatal(err)
	}
	for _, cleanup := range plan.ControlCleanup {
		if _, err := os.Lstat(cleanup.path); err != nil {
			t.Fatalf("placeholder %s was not created: %v", cleanup.path, err)
		}
	}
	if err := control.Close(); err != nil {
		t.Fatal(err)
	}
	for _, cleanup := range plan.ControlCleanup {
		if _, err := os.Lstat(cleanup.path); !os.IsNotExist(err) {
			t.Fatalf("placeholder %s remained after cleanup: %v", cleanup.path, err)
		}
	}
}

func TestControlPlaceholdersAreSharedAcrossConcurrentInstances(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	project := t.TempDir()
	plan, err := BuildPlan(controlTestOptions(project, "placeholder-shared-one"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := newLaunchControlFiles(project)
	if err != nil {
		t.Fatal(err)
	}
	second, err := newLaunchControlFiles(project)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.prepare(plan.ControlCleanup); err != nil {
		t.Fatal(err)
	}
	if err := second.prepare(plan.ControlCleanup); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	for _, cleanup := range plan.ControlCleanup {
		if _, err := os.Lstat(cleanup.path); err != nil {
			t.Fatalf("shared placeholder %s was removed while still leased: %v", cleanup.path, err)
		}
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	for _, cleanup := range plan.ControlCleanup {
		if _, err := os.Lstat(cleanup.path); !os.IsNotExist(err) {
			t.Fatalf("shared placeholder %s remained after the last lease: %v", cleanup.path, err)
		}
	}
}

func TestStaleControlPlaceholdersAreReusedAndRecovered(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	project := t.TempDir()
	plan, err := BuildPlan(controlTestOptions(project, "placeholder-stale-one"))
	if err != nil {
		t.Fatal(err)
	}
	crashed, err := newLaunchControlFiles(project)
	if err != nil {
		t.Fatal(err)
	}
	if err := crashed.prepare(plan.ControlCleanup); err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(crashed.lifetimeFD, unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if err := unix.Close(crashed.lifetimeFD); err != nil {
		t.Fatal(err)
	}
	crashed.lifetimeFD = -1
	crashed.once.Do(func() {})

	secondPlan, err := BuildPlan(controlTestOptions(project, "placeholder-stale-two"))
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := newLaunchControlFiles(project)
	if err != nil {
		t.Fatal(err)
	}
	if err := recovery.prepare(secondPlan.ControlCleanup); err != nil {
		t.Fatal(err)
	}
	if err := recovery.Close(); err != nil {
		t.Fatal(err)
	}
	for _, cleanup := range plan.ControlCleanup {
		if _, err := os.Lstat(cleanup.path); !os.IsNotExist(err) {
			t.Fatalf("stale placeholder %s was not recovered: %v", cleanup.path, err)
		}
	}
}
