package app

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func testAgentContext(t *testing.T) (string, agentContext) {
	t.Helper()
	root := t.TempDir()
	project := filepath.Join(root, "project")
	state := filepath.Join(root, "state")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := secureMkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	return root, agentContext{state: state, project: project, hostEnv: os.Environ(), config: true}
}

func writeAgentTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mountMap(mounts []agentMount) map[string]string {
	result := make(map[string]string, len(mounts))
	for _, mount := range mounts {
		result[mount.Destination] = mount.Source
	}
	return result
}

func TestAgentDetection(t *testing.T) {
	for _, program := range []string{"opencode", "/opt/bin/opencode.exe"} {
		if adapter := detectAgent(program); adapter == nil || adapter.name != "opencode" {
			t.Fatalf("detectAgent(%q) = %#v", program, adapter)
		}
	}
	for _, program := range []string{"pi", "/opt/bin/pi.exe"} {
		if adapter := detectAgent(program); adapter == nil || adapter.name != "pi" {
			t.Fatalf("detectAgent(%q) = %#v", program, adapter)
		}
	}
	if adapter := detectAgent("my-opencode"); adapter != nil {
		t.Fatalf("unexpected alias detection: %#v", adapter)
	}
}

func TestOpenCodeUsesReadOnlyConfigAndSeedsAuthOnce(t *testing.T) {
	root, context := testAgentContext(t)
	home := filepath.Join(root, "home")
	config := filepath.Join(root, "host-config")
	data := filepath.Join(root, "host-data")
	writeAgentTestFile(t, filepath.Join(config, "opencode", "opencode.jsonc"), `{"model":"first"}`)
	writeAgentTestFile(t, filepath.Join(data, "opencode", "auth.json"), `{"token":"first"}`)
	t.Setenv("HOME", home)
	context.hostEnv = []string{"XDG_CONFIG_HOME=" + config, "XDG_DATA_HOME=" + data}

	setup, err := prepareAgent(detectAgent("opencode"), context)
	if err != nil {
		t.Fatal(err)
	}
	mounts := mountMap(setup.Mounts)
	destination := filepath.Join(context.state, "config", "opencode")
	wantSource, _ := filepath.EvalSymlinks(filepath.Join(config, "opencode"))
	if mounts[destination] != wantSource {
		t.Fatalf("OpenCode config mount = %q, want %q", mounts[destination], wantSource)
	}
	if _, err := os.Stat(filepath.Join(destination, "opencode.jsonc")); !os.IsNotExist(err) {
		t.Fatalf("host config was copied into instance state: %v", err)
	}
	authPath := filepath.Join(context.state, "data", "opencode", "auth.json")
	if content, err := os.ReadFile(authPath); err != nil || string(content) != `{"token":"first"}` {
		t.Fatalf("seeded auth = %q, %v", content, err)
	}
	writeAgentTestFile(t, filepath.Join(data, "opencode", "auth.json"), `{"token":"changed"}`)
	if _, err := prepareAgent(detectAgent("opencode"), context); err != nil {
		t.Fatal(err)
	}
	if content, _ := os.ReadFile(authPath); string(content) != `{"token":"first"}` {
		t.Fatalf("auth was seeded more than once: %s", content)
	}
}

func TestOpenCodeCustomConfigOverridesAreMapped(t *testing.T) {
	root, context := testAgentContext(t)
	configFile := filepath.Join(root, "custom.json")
	tuiFile := filepath.Join(root, "tui.json")
	configDirectory := filepath.Join(root, "custom-directory")
	writeAgentTestFile(t, configFile, `{}`)
	writeAgentTestFile(t, tuiFile, `{}`)
	if err := os.MkdirAll(configDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	context.hostEnv = []string{
		"HOME=" + filepath.Join(root, "home"),
		"OPENCODE_CONFIG=" + configFile,
		"OPENCODE_TUI_CONFIG=" + tuiFile,
		"OPENCODE_CONFIG_DIR=" + configDirectory,
	}
	setup, err := prepareAgent(detectAgent("opencode"), context)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"OPENCODE_CONFIG":     "/run/bwrap-agent/agent-config/opencode/config",
		"OPENCODE_TUI_CONFIG": "/run/bwrap-agent/agent-config/opencode/tui-config",
		"OPENCODE_CONFIG_DIR": "/run/bwrap-agent/agent-config/opencode/config-dir",
	} {
		if setup.Environment[name] != want {
			t.Errorf("%s = %q, want %q", name, setup.Environment[name], want)
		}
	}
}

func TestPiSeparatesReadOnlyConfigAndMutableState(t *testing.T) {
	root, context := testAgentContext(t)
	home := filepath.Join(root, "home")
	hostAgent := filepath.Join(home, ".pi", "agent")
	for name, content := range map[string]string{
		"settings.json":     `{"defaultModel":"test"}`,
		"auth.json":         `{"provider":"secret"}`,
		"trust.json":        `{"/work":true}`,
		"models-store.json": `{"models":[]}`,
		"extension.json":    `{"enabled":true}`,
	} {
		writeAgentTestFile(t, filepath.Join(hostAgent, name), content)
	}
	for _, directory := range []string{"sessions", "extensions", "npm"} {
		if err := os.MkdirAll(filepath.Join(hostAgent, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	globalSkills := filepath.Join(home, ".agents", "skills")
	if err := os.MkdirAll(globalSkills, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	context.hostEnv = []string{"PI_TRUE_COLOR=1", "PI_TUI_ESC_TIMEOUT=50"}

	setup, err := prepareAgent(detectAgent("pi"), context)
	if err != nil {
		t.Fatal(err)
	}
	sandboxAgent := filepath.Join(context.state, "home", ".pi", "agent")
	if setup.Environment["PI_CODING_AGENT_DIR"] != sandboxAgent || setup.Environment["PI_CODING_AGENT_SESSION_DIR"] != filepath.Join(sandboxAgent, "sessions") {
		t.Fatalf("Pi directories = %#v", setup.Environment)
	}
	if setup.Environment["PI_TRUE_COLOR"] != "1" || setup.Environment["PI_TUI_ESC_TIMEOUT"] != "50" {
		t.Fatalf("Pi terminal environment = %#v", setup.Environment)
	}
	mounts := mountMap(setup.Mounts)
	for _, name := range []string{"settings.json", "extension.json", "extensions", "npm"} {
		if mounts[filepath.Join(sandboxAgent, name)] == "" {
			t.Errorf("missing read-only Pi config mount for %s", name)
		}
	}
	for _, name := range []string{"auth.json", "trust.json", "models-store.json", "sessions"} {
		if mounts[filepath.Join(sandboxAgent, name)] != "" {
			t.Errorf("mutable Pi entry was mounted from host: %s", name)
		}
	}
	if mounts[filepath.Join(context.state, "home", ".agents", "skills")] == "" {
		t.Error("global Pi skills were not mounted")
	}
	for name, want := range map[string]string{
		"auth.json":         `{"provider":"secret"}`,
		"trust.json":        `{"/work":true}`,
		"models-store.json": `{"models":[]}`,
	} {
		content, err := os.ReadFile(filepath.Join(sandboxAgent, name))
		if err != nil || string(content) != want {
			t.Errorf("seeded %s = %q, %v", name, content, err)
		}
	}
	if entries, err := os.ReadDir(filepath.Join(sandboxAgent, "sessions")); err != nil || len(entries) != 0 {
		t.Fatalf("instance sessions are not initially empty: %#v, %v", entries, err)
	}
}

func TestDisabledAgentConfigDoesNotExposeOrSeedHost(t *testing.T) {
	root, context := testAgentContext(t)
	home := filepath.Join(root, "home")
	writeAgentTestFile(t, filepath.Join(home, ".pi", "agent", "auth.json"), `{"secret":true}`)
	t.Setenv("HOME", home)
	context.config = false
	context.hostEnv = []string{"PI_TRUE_COLOR=1"}
	setup, err := prepareAgent(detectAgent("pi"), context)
	if err != nil {
		t.Fatal(err)
	}
	if len(setup.Mounts) != 0 {
		t.Fatalf("disabled agent config mounts = %#v", setup.Mounts)
	}
	if _, err := os.Stat(filepath.Join(context.state, "home", ".pi", "agent", "auth.json")); !os.IsNotExist(err) {
		t.Fatalf("disabled agent config seeded auth: %v", err)
	}
	if setup.Environment["PI_CODING_AGENT_DIR"] == "" || setup.Environment["PI_TRUE_COLOR"] != "1" {
		t.Fatalf("Pi runtime environment missing when config disabled: %#v", setup.Environment)
	}
}

func TestAgentConfigSourceCannotBeRetargetedThroughProject(t *testing.T) {
	root, context := testAgentContext(t)
	secret := filepath.Join(root, "secret")
	if err := os.Mkdir(secret, 0o700); err != nil {
		t.Fatal(err)
	}
	configBase := filepath.Join(context.project, "config")
	if err := os.Mkdir(configBase, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(configBase, "opencode")); err != nil {
		t.Fatal(err)
	}
	context.hostEnv = []string{"XDG_CONFIG_HOME=" + configBase}
	if _, err := prepareAgent(detectAgent("opencode"), context); err == nil || !strings.Contains(err.Error(), "overlaps") {
		t.Fatalf("unsafe source was accepted: %v", err)
	}
}

func TestAgentConfigSourceRejectsProtectedIntermediateSymlinkHop(t *testing.T) {
	root, context := testAgentContext(t)
	secret := filepath.Join(root, "secret")
	configBase := filepath.Join(root, "host-config")
	if err := os.MkdirAll(secret, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(configBase, 0o700); err != nil {
		t.Fatal(err)
	}
	redirect := filepath.Join(context.project, "redirect")
	if err := os.Symlink(secret, redirect); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(redirect, filepath.Join(configBase, "opencode")); err != nil {
		t.Fatal(err)
	}
	context.hostEnv = []string{"XDG_CONFIG_HOME=" + configBase}
	if _, err := prepareAgent(detectAgent("opencode"), context); err == nil || !strings.Contains(err.Error(), "overlaps") {
		t.Fatalf("protected intermediate symlink hop was accepted: %v", err)
	}
}

func TestAgentConfigSourceRejectsProtectedSymlinkHopBeforeDotDot(t *testing.T) {
	root, context := testAgentContext(t)
	hostDirectory := filepath.Join(root, "host")
	secretDirectory := filepath.Join(root, "hidden", "opencode")
	if err := os.MkdirAll(hostDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(secretDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "hidden", "discarded"), 0o700); err != nil {
		t.Fatal(err)
	}
	redirect := filepath.Join(context.project, "redirect")
	if err := os.Symlink(filepath.Join(root, "hidden", "discarded"), redirect); err != nil {
		t.Fatal(err)
	}
	// Kernel resolution follows redirect before applying ..; purely lexical
	// cleaning would erase the protected hop and leave only hidden/opencode.
	target := redirect + string(filepath.Separator) + ".." + string(filepath.Separator) + "opencode"
	if err := os.Symlink(target, filepath.Join(hostDirectory, "opencode")); err != nil {
		t.Fatal(err)
	}
	context.hostEnv = []string{"XDG_CONFIG_HOME=" + hostDirectory}
	if _, err := prepareAgent(detectAgent("opencode"), context); err == nil || !strings.Contains(err.Error(), "overlaps") {
		t.Fatalf("protected symlink hop before .. was accepted: %v", err)
	}
}

func TestPiChildMountRejectsProtectedIntermediateSymlinkHop(t *testing.T) {
	root, context := testAgentContext(t)
	home := filepath.Join(root, "home")
	hostAgent := filepath.Join(home, ".pi", "agent")
	secret := filepath.Join(root, "secret.json")
	writeAgentTestFile(t, secret, `{"secret":true}`)
	if err := os.MkdirAll(hostAgent, 0o700); err != nil {
		t.Fatal(err)
	}
	redirect := filepath.Join(context.project, "redirect")
	if err := os.Symlink(secret, redirect); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(redirect, filepath.Join(hostAgent, "settings.json")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	if _, err := prepareAgent(detectAgent("pi"), context); err == nil || !strings.Contains(err.Error(), "overlaps") {
		t.Fatalf("unsafe Pi child source was accepted: %v", err)
	}
}

func TestAgentConfigMountDestinationCannotBeRetargetedThroughState(t *testing.T) {
	root, context := testAgentContext(t)
	home := filepath.Join(root, "home")
	writeAgentTestFile(t, filepath.Join(home, ".pi", "agent", "settings.json"), `{}`)
	t.Setenv("HOME", home)
	stateAgent := filepath.Join(context.state, "home", ".pi", "agent")
	if err := os.MkdirAll(stateAgent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(context.project, filepath.Join(stateAgent, "settings.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareAgent(detectAgent("pi"), context); err == nil || !strings.Contains(err.Error(), "unsafe agent configuration mount destination") {
		t.Fatalf("unsafe mount destination was accepted: %v", err)
	}
}

func TestPiNPMEntrypointMountsPackageRoot(t *testing.T) {
	root := t.TempDir()
	runtimeDirectory := filepath.Join(root, "runtime")
	runtime := filepath.Join(runtimeDirectory, "node")
	writeAgentTestFile(t, runtime, "#!/bin/sh\n")
	if err := os.Chmod(runtime, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", runtimeDirectory)
	packageRoot := filepath.Join(root, "node_modules", "@earendil-works", "pi-coding-agent")
	entrypoint := filepath.Join(packageRoot, "dist", "cli.js")
	writeAgentTestFile(t, filepath.Join(packageRoot, "package.json"), `{"name":"@earendil-works/pi-coding-agent"}`)
	writeAgentTestFile(t, entrypoint, `#!/usr/bin/env node`)
	requested := []string{"pi", "--version"}
	_, context := testAgentContext(t)
	command, setup, err := resolvePiExternalCommand(context, requested, entrypoint)
	if err != nil {
		t.Fatal(err)
	}
	wantCommand := []string{"/run/bwrap-agent/command-package/dist/cli.js", "--version"}
	if !reflect.DeepEqual(command, wantCommand) {
		t.Fatalf("command = %#v, want %#v", command, wantCommand)
	}
	mounts := mountMap(setup.Mounts)
	if mounts["/run/bwrap-agent/command-package"] != packageRoot || mounts["/run/bwrap-agent/agent-runtime/node"] == "" {
		t.Fatalf("mounts = %#v", setup.Mounts)
	}
	if !strings.HasPrefix(setup.Environment["PATH"], "/run/bwrap-agent/agent-runtime:") {
		t.Fatalf("Pi PATH = %q", setup.Environment["PATH"])
	}
}
