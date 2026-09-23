package app

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestCodexResourcesAndSeedOnce(t *testing.T) {
	root, context := testHostContext(t)
	home := filepath.Join(root, "home")
	hostCodex := filepath.Join(root, "custom-codex")
	t.Setenv("HOME", home)
	context.hostEnv = []string{"CODEX_HOME=" + hostCodex}
	for _, name := range []string{"config.toml", "work.config.toml", "AGENTS.md", "hooks.json", "auth.json"} {
		writeAgentTestFile(t, filepath.Join(hostCodex, name), "host-"+name)
	}
	for _, name := range []string{"rules", "skills", "plugins", "agents", "prompts"} {
		writeAgentTestFile(t, filepath.Join(hostCodex, name, "resource"), name)
	}
	writeAgentTestFile(t, filepath.Join(home, ".agents", "skills", "custom", "SKILL.md"), "skill")
	writeAgentTestFile(t, filepath.Join(hostCodex, "history.jsonl"), "host-history")

	setup, err := prepareAgent(detectAgent("codex"), context, true)
	if err != nil {
		t.Fatal(err)
	}
	privateHome := filepath.Join(context.state, "home", ".codex")
	if setup.Environment["CODEX_HOME"] != privateHome {
		t.Fatalf("CODEX_HOME = %q", setup.Environment["CODEX_HOME"])
	}
	mounts := mountMap(setup.Mounts)
	for _, name := range []string{"config.toml", "work.config.toml", "AGENTS.md", "hooks.json", "rules", "skills", "plugins", "agents", "prompts"} {
		if mounts[filepath.Join(privateHome, name)] != filepath.Join(hostCodex, name) {
			t.Errorf("missing read-only Codex resource %s: %#v", name, mounts)
		}
	}
	if mounts[filepath.Join(context.state, "home", ".agents", "skills")] != filepath.Join(home, ".agents", "skills") {
		t.Error("host personal skills were not shared")
	}
	for _, name := range []string{"auth.json", "history.jsonl", "sessions", "cache"} {
		if mounts[filepath.Join(privateHome, name)] != "" {
			t.Errorf("host state was mounted: %s", name)
		}
	}
	auth := filepath.Join(privateHome, "auth.json")
	if content, err := os.ReadFile(auth); err != nil || string(content) != "host-auth.json" {
		t.Fatalf("seeded auth = %q, %v", content, err)
	}
	if info, err := os.Stat(auth); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("seeded auth permissions = %v, %v", info, err)
	}
	writeAgentTestFile(t, auth, "instance-auth")
	writeAgentTestFile(t, filepath.Join(hostCodex, "auth.json"), "changed-host-auth")
	if _, err := prepareAgent(detectAgent("codex"), context, true); err != nil {
		t.Fatal(err)
	}
	if content, _ := os.ReadFile(auth); string(content) != "instance-auth" {
		t.Fatalf("auth was reseeded: %q", content)
	}
}

func TestCodexDisabledAndMissingHostConfiguration(t *testing.T) {
	root, context := testHostContext(t)
	t.Setenv("HOME", filepath.Join(root, "home"))
	hostCodex := filepath.Join(root, "host-codex")
	writeAgentTestFile(t, filepath.Join(hostCodex, "auth.json"), "secret")
	context.hostEnv = []string{"CODEX_HOME=" + hostCodex}
	setup, err := prepareAgent(detectAgent("codex"), context, false)
	if err != nil || len(setup.Mounts) != 0 {
		t.Fatalf("disabled Codex setup = %#v, %v", setup, err)
	}
	if _, err := os.Stat(filepath.Join(context.state, "home", ".codex", "auth.json")); !os.IsNotExist(err) {
		t.Fatalf("disabled Codex seeded auth: %v", err)
	}
	context.hostEnv = []string{"CODEX_HOME=" + filepath.Join(root, "missing")}
	setup, err = prepareAgent(detectAgent("codex"), context, true)
	if err != nil || len(setup.Mounts) != 0 {
		t.Fatalf("missing host setup = %#v, %v", setup, err)
	}
	context.hostEnv = []string{"CODEX_HOME=relative"}
	if _, err := prepareAgent(detectAgent("codex"), context, true); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative host CODEX_HOME accepted: %v", err)
	}
}

func TestCodexHostSourceAndDestinationSafety(t *testing.T) {
	root, context := testHostContext(t)
	t.Setenv("HOME", filepath.Join(root, "home"))
	secret := filepath.Join(root, "secret")
	writeAgentTestFile(t, filepath.Join(secret, "config.toml"), "secret")
	alias := filepath.Join(root, "project", "codex")
	if err := os.Symlink(secret, alias); err != nil {
		t.Fatal(err)
	}
	context.hostEnv = []string{"CODEX_HOME=" + alias}
	if _, err := prepareAgent(detectAgent("codex"), context, true); err == nil || !strings.Contains(err.Error(), "overlaps") {
		t.Fatalf("unsafe Codex source accepted: %v", err)
	}
	context.hostEnv = []string{"CODEX_HOME=" + secret}
	destination := filepath.Join(context.state, "home", ".codex", "config.toml")
	writeAgentTestFile(t, filepath.Join(root, "target"), "target")
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "target"), destination); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareAgent(detectAgent("codex"), context, true); err == nil || !strings.Contains(err.Error(), "unsafe state mount destination") {
		t.Fatalf("unsafe Codex destination accepted: %v", err)
	}
}

func TestCodexCommandSandboxSelection(t *testing.T) {
	for _, test := range []struct {
		name      string
		args      []string
		podman    bool
		want      []string
		wantError bool
	}{
		{"off injects", []string{"exec", "--json"}, false, []string{"codex", codexBypassFlag, "exec", "--json"}, false},
		{"podman preserves", []string{"exec"}, true, []string{"codex", "exec"}, false},
		{"already bypassed", []string{codexBypassFlag, "exec"}, false, []string{"codex", codexBypassFlag, "exec"}, false},
		{"bypass after separator is a prompt", []string{"exec", "--", codexBypassFlag}, false, []string{"codex", codexBypassFlag, "exec", "--", codexBypassFlag}, false},
		{"diagnostic sandbox", []string{"sandbox", "/bin/true"}, false, []string{"codex", "sandbox", "/bin/true"}, false},
		{"prompt says sandbox", []string{"exec", "sandbox"}, false, []string{"codex", codexBypassFlag, "exec", "sandbox"}, false},
		{"explicit sandbox", []string{"--sandbox", "workspace-write", "exec"}, false, nil, true},
		{"bypass cannot hide conflict", []string{codexBypassFlag, "--sandbox", "workspace-write"}, false, nil, true},
		{"explicit approval", []string{"-a", "on-request"}, false, nil, true},
		{"explicit config", []string{"-c", "sandbox_mode=\"workspace-write\""}, false, nil, true},
		{"full auto", []string{"--full-auto"}, false, nil, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			requested := append([]string{"codex"}, test.args...)
			original := append([]string(nil), requested...)
			command := preparedCommand{Argv: append([]string(nil), requested...)}
			prepared, err := prepareCodexCommand(requested, command, test.podman)
			if (err != nil) != test.wantError || err == nil && !reflect.DeepEqual(prepared.Argv, test.want) {
				t.Fatalf("prepared = %q, %v; want %q", prepared.Argv, err, test.want)
			}
			if !reflect.DeepEqual(requested, original) {
				t.Fatal("requested arguments were modified")
			}
		})
	}
}

func TestCodexPlanUsesEffectivePodmanMode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	program := filepath.Join(t.TempDir(), "codex")
	writeAgentTestFile(t, program, "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(program, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, podman, workspace, config string
	}{
		{"off", "off", "write-through", "auto"},
		{"auto forced off by read-only", "auto", "read-only", "auto"},
		{"config off retains runtime setup", "off", "write-through", "off"},
	} {
		t.Run(test.name, func(t *testing.T) {
			opts := Options{
				Project: t.TempDir(), Instance: "codex-" + strings.ReplaceAll(test.name, " ", "-"),
				Network: "host", Podman: test.podman, WorkspaceMode: test.workspace,
				AgentConfig: test.config, TTY: "never", NoGitConfig: true,
				Command: []string{program, "exec", "test"},
			}
			plan, err := BuildPlan(opts)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(plan.Command, []string{"/run/bwrap-agent/command", codexBypassFlag, "exec", "test"}) {
				t.Fatalf("Codex command = %q", plan.Command)
			}
			if plan.LaunchEnv["CODEX_HOME"] != filepath.Join(home, ".codex") {
				t.Fatalf("private CODEX_HOME = %q", plan.LaunchEnv["CODEX_HOME"])
			}
		})
	}
}

func TestCodexNpmCommandMountsPackagePlatformAndNode(t *testing.T) {
	root, context := testHostContext(t)
	packageRoot := filepath.Join(root, "lib", "node_modules", "@openai", "codex")
	entrypoint := filepath.Join(packageRoot, "bin", "codex.js")
	writeAgentTestFile(t, filepath.Join(packageRoot, "package.json"), `{"name":"@openai/codex"}`)
	writeAgentTestFile(t, entrypoint, "#!/usr/bin/env node\n")
	if err := os.Chmod(entrypoint, 0o700); err != nil {
		t.Fatal(err)
	}
	platformName := "codex-linux-x64"
	if runtime.GOARCH == "arm64" {
		platformName = "codex-linux-arm64"
	}
	platformRoot := filepath.Join(root, "lib", "node_modules", "@openai", platformName)
	writeAgentTestFile(t, filepath.Join(platformRoot, "package.json"), `{"name":"@openai/`+platformName+`"}`)
	writeAgentTestFile(t, filepath.Join(platformRoot, "vendor", "binary"), "native")
	bin := filepath.Join(root, "bin")
	writeAgentTestFile(t, filepath.Join(bin, "node"), "#!/bin/sh\n")
	if err := os.Chmod(filepath.Join(bin, "node"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(entrypoint, filepath.Join(bin, "codex")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	setup, err := resolveCommand(context, []string{filepath.Join(bin, "codex"), "exec", "hello"}, resolveCodexExternalCommand)
	if err != nil {
		t.Fatal(err)
	}
	destination := "/run/bwrap-agent/command-package/node_modules/@openai/codex"
	if !reflect.DeepEqual(setup.Argv, []string{destination + "/bin/codex.js", "exec", "hello"}) {
		t.Fatalf("npm Codex argv = %q", setup.Argv)
	}
	mounts := mountMap(setup.Mounts)
	if mounts[destination] != packageRoot || mounts[filepath.Dir(destination)+"/"+platformName] != platformRoot || mounts["/run/bwrap-agent/agent-runtime/node"] != filepath.Join(bin, "node") {
		t.Fatalf("npm Codex mounts = %#v", mounts)
	}
	if !strings.HasPrefix(setup.Environment["PATH"], "/run/bwrap-agent/agent-runtime:") {
		t.Fatalf("npm Codex PATH = %q", setup.Environment["PATH"])
	}
}

func TestCodexNpmCommandFallsBackToVendoredPackage(t *testing.T) {
	root, context := testHostContext(t)
	packageRoot := filepath.Join(root, "node_modules", "@openai", "codex")
	entrypoint := filepath.Join(packageRoot, "bin", "codex.js")
	writeAgentTestFile(t, filepath.Join(packageRoot, "package.json"), `{"name":"@openai/codex"}`)
	writeAgentTestFile(t, filepath.Join(packageRoot, "vendor", "binary"), "native")
	writeAgentTestFile(t, entrypoint, "#!/usr/bin/env node\n")
	bin := filepath.Join(root, "bin")
	writeAgentTestFile(t, filepath.Join(bin, "node"), "#!/bin/sh\n")
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	setup, err := resolveCodexExternalCommand(context, []string{"codex"}, entrypoint)
	if err != nil {
		t.Fatal(err)
	}
	if len(setup.Mounts) != 2 || setup.Mounts[1].Source != packageRoot {
		t.Fatalf("vendored npm mounts = %#v", setup.Mounts)
	}
}

func TestCodexNpmNestedPlatformPackage(t *testing.T) {
	root, context := testHostContext(t)
	packageRoot := filepath.Join(root, "node_modules", "@openai", "codex")
	platform := "codex-linux-x64"
	if runtime.GOARCH == "arm64" {
		platform = "codex-linux-arm64"
	}
	nested := filepath.Join(packageRoot, "node_modules", "@openai", platform)
	writeAgentTestFile(t, filepath.Join(nested, "package.json"), `{"name":"@openai/`+platform+`"}`)
	source, found, err := findCodexPlatformPackage(context, packageRoot, platform)
	if err != nil || !found || source != nested {
		t.Fatalf("nested platform source = %q, %v, %v", source, found, err)
	}
	unsafe := filepath.Join(root, "project", "platform")
	writeAgentTestFile(t, filepath.Join(unsafe, "package.json"), "{}")
	if err := os.RemoveAll(nested); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(unsafe, nested); err != nil {
		t.Fatal(err)
	}
	if _, _, err := findCodexPlatformPackage(context, packageRoot, platform); err == nil || !strings.Contains(err.Error(), "overlaps") {
		t.Fatalf("unsafe platform source accepted: %v", err)
	}
}
