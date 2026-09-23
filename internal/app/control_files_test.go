package app

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func controlTestOptions(project, instance string) Options {
	return Options{
		Project: project, Instance: instance, Network: "host", Podman: "off",
		WorkspaceMode: "write-through", TTY: "never", Command: []string{"/bin/true"},
	}
}

func TestWorkspaceLeavesMissingControlPathsAbsent(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	for _, existingOpenCode := range []bool{false, true} {
		project := t.TempDir()
		if existingOpenCode {
			if err := os.Mkdir(filepath.Join(project, ".opencode"), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		plan, err := BuildPlan(controlTestOptions(project, "missing-"+filepath.Base(project)))
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.ProtectedPaths) != 0 {
			t.Fatalf("protected absent paths: %v", plan.ProtectedPaths)
		}
		for _, path := range []string{projectConfigName, "AGENTS.md", ".codex", "opencode.json", "opencode.jsonc", ".pi", ".opencode/.gitignore"} {
			if _, err := os.Lstat(filepath.Join(project, path)); !os.IsNotExist(err) {
				t.Fatalf("created %s: %v", path, err)
			}
			if strings.Contains(strings.Join(plan.Bwrap, "\x00"), filepath.Join(project, path)) {
				t.Fatalf("mount for absent path %s", path)
			}
		}
		var output bytes.Buffer
		if err := writePlanJSON(&output, plan); err != nil {
			t.Fatal(err)
		}
		var decoded struct {
			ProtectedPaths []string `json:"protected_paths"`
		}
		if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
			t.Fatal(err)
		}
		if len(decoded.ProtectedPaths) != 0 {
			t.Fatal(decoded.ProtectedPaths)
		}
	}
}

func TestCodexControlPathsAreProtected(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	project := t.TempDir()
	writeTestFile(t, filepath.Join(project, "AGENTS.md"), "instructions")
	if err := os.Mkdir(filepath.Join(project, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(project, ".codex", "config.toml"), "sandbox_mode = \"read-only\"")
	opts := controlTestOptions(project, "codex-protected")
	plan, err := BuildPlan(opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{"AGENTS.md", ".codex"} {
		path := filepath.Join(project, relative)
		if !strings.Contains(strings.Join(plan.ProtectedPaths, "\x00"), path) {
			t.Errorf("missing Codex control protection %s: %#v", relative, plan.ProtectedPaths)
		}
	}
	opts.AllowControlFileWrites = true
	opts.Instance = "codex-writable"
	plan, err = BuildPlan(opts)
	if err != nil || len(plan.ProtectedPaths) != 0 {
		t.Fatalf("explicit control writes = %#v, %v", plan.ProtectedPaths, err)
	}
}

func TestOpenCodeGitignoreBackingPreservesHostContents(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	project := t.TempDir()
	hostContent := []byte("node_modules/\ncustom-cache/\n")
	if err := os.Mkdir(filepath.Join(project, ".opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(project, ".opencode", ".gitignore"), string(hostContent))
	plan, err := BuildPlan(controlTestOptions(project, "opencode-existing-gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(filepath.Dir(plan.State), "control-masks", "opencode", ".gitignore")
	content, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(content, hostContent) {
		t.Fatalf("OpenCode gitignore backing content = %q, want %q", content, hostContent)
	}

}

func TestOpenCodeGitignoreCopyIsSizeLimited(t *testing.T) {
	t.Run("accepts limit", func(t *testing.T) {
		t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
		project := t.TempDir()
		directory := filepath.Join(project, ".opencode")
		if err := os.Mkdir(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		content := bytes.Repeat([]byte{'x'}, maxOpenCodeGitignoreSize)
		if err := os.WriteFile(filepath.Join(directory, ".gitignore"), content, 0o600); err != nil {
			t.Fatal(err)
		}
		plan, err := BuildPlan(controlTestOptions(project, "opencode-gitignore-at-limit"))
		if err != nil {
			t.Fatal(err)
		}
		source := filepath.Join(filepath.Dir(plan.State), "control-masks", "opencode", ".gitignore")
		info, err := os.Stat(source)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() != maxOpenCodeGitignoreSize {
			t.Fatalf("OpenCode gitignore backing size = %d, want %d", info.Size(), maxOpenCodeGitignoreSize)
		}
	})

	t.Run("rejects oversized sparse file", func(t *testing.T) {
		t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
		project := t.TempDir()
		directory := filepath.Join(project, ".opencode")
		if err := os.Mkdir(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(directory, ".gitignore")
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Truncate(maxOpenCodeGitignoreSize + 1); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		_, err = BuildPlan(controlTestOptions(project, "opencode-gitignore-over-limit"))
		if err == nil || !strings.Contains(err.Error(), "file exceeds 1048576 bytes") {
			t.Fatalf("oversized OpenCode gitignore error = %v", err)
		}
	})
}

func TestExistingPiResourcesProtectOnlyControlSurfaces(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	project := t.TempDir()
	if err := os.MkdirAll(filepath.Join(project, ".pi", "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{".pi/extensions", ".pi/npm", ".pi/git"} {
		if err := os.MkdirAll(filepath.Join(project, relative), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(t, filepath.Join(project, ".pi/settings.json"), "{}")
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
	opts.Instance = "protected-read-only"
	opts.AllowControlFileWrites = false
	opts.WorkspaceMode = "read-only"
	plan, err = BuildPlan(opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.ProtectedPaths) != 0 {
		t.Fatalf("read-only has redundant protections: %#v", plan.ProtectedPaths)
	}
}

func TestProjectConfigSymlinkCannotBypassTrust(t *testing.T) {
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
	_, code, err := parseOptions([]string{"run", projectArg, "--allow-control-file-writes", "/bin/true"}, &stdout, &stderr)
	if err == nil || code != 2 || !strings.Contains(err.Error(), "is a symlink") {
		t.Fatalf("escape bypassed project config symlink rejection: code %d, err %v", code, err)
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

// An explicit writable ancestor must not hide the later control-file mount.
func TestControlProtectionFollowsExplicitWritableBind(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	project := t.TempDir()
	pi := filepath.Join(project, ".pi")
	if err := os.Mkdir(pi, 0o700); err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(pi, "settings.json")
	writeTestFile(t, settings, "{}")
	opts := controlTestOptions(project, "control-mount-order")
	opts.RWBind = []string{pi}
	plan, err := BuildPlan(opts)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan.Bwrap, "\x00")
	writable := strings.Index(joined, strings.Join([]string{"--bind", pi, pi}, "\x00"))
	protected := strings.Index(joined, strings.Join([]string{"--ro-bind", settings, settings}, "\x00"))
	if writable < 0 || protected <= writable {
		t.Fatalf("control protection must follow writable ancestor: %#v", plan.Bwrap)
	}
	found := false
	for _, path := range plan.ProtectedPaths {
		if path == settings {
			found = true
		}
	}
	if !found {
		t.Fatalf("protected settings missing from plan metadata: %#v", plan.ProtectedPaths)
	}
}
