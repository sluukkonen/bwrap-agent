package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const codexBypassFlag = "--dangerously-bypass-approvals-and-sandbox"

// npm installs use a Node launcher in @openai/codex. Recent releases put the
// native executable in a sibling platform package; older ones vendor it in the
// Codex package itself. Both layouts need their package-relative paths intact.
func resolveCodexExternalCommand(context hostContext, requested []string, resolved string) (preparedCommand, error) {
	for directory := filepath.Dir(resolved); directory != filepath.Dir(directory); directory = filepath.Dir(directory) {
		content, err := os.ReadFile(filepath.Join(directory, "package.json"))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return preparedCommand{}, err
		}
		var manifest struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(content, &manifest) != nil || manifest.Name != "@openai/codex" {
			continue
		}
		packageRoot, _, err := context.sources.resolveSource(directory, true, true)
		if err != nil {
			return preparedCommand{}, fmt.Errorf("resolve Codex npm package: %w", err)
		}
		relative, err := filepath.Rel(packageRoot, resolved)
		if err != nil || relative == "." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return preparedCommand{}, fmt.Errorf("Codex entrypoint is outside package root")
		}
		node, err := exec.LookPath("node")
		if err != nil {
			return preparedCommand{}, fmt.Errorf("Codex npm installation requires node in the host PATH")
		}
		node, _, err = context.sources.resolveSource(node, false, true)
		if err != nil {
			return preparedCommand{}, fmt.Errorf("resolve Codex node runtime: %w", err)
		}
		const destination = "/run/bwrap-agent/command-package/node_modules/@openai/codex"
		setup := preparedCommand{
			Argv: append([]string{filepath.Join(destination, relative)}, requested[1:]...),
			Mounts: []resourceMount{
				{Source: node, Destination: "/run/bwrap-agent/agent-runtime/node", Executable: true},
				{Source: packageRoot, Destination: destination},
			},
			Environment: map[string]string{
				"PATH": "/run/bwrap-agent/agent-runtime:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
			},
		}
		platformPackage := ""
		switch runtime.GOARCH {
		case "amd64":
			platformPackage = "codex-linux-x64"
		case "arm64":
			platformPackage = "codex-linux-arm64"
		}
		if platformPackage != "" {
			platformRoot, found, err := findCodexPlatformPackage(context, packageRoot, platformPackage)
			if err != nil {
				return preparedCommand{}, err
			}
			if found {
				setup.Mounts = append(setup.Mounts, resourceMount{
					Source: platformRoot, Destination: filepath.Join(filepath.Dir(destination), platformPackage),
				})
			}
		}
		return setup, nil
	}
	return standaloneCommand(requested, resolved), nil
}

func findCodexPlatformPackage(context hostContext, packageRoot, packageName string) (string, bool, error) {
	for directory := packageRoot; directory != filepath.Dir(directory); directory = filepath.Dir(directory) {
		candidates := []string{filepath.Join(directory, packageName)}
		if filepath.Base(directory) != "@openai" {
			candidates = []string{filepath.Join(directory, "node_modules", "@openai", packageName)}
		}
		for _, candidate := range candidates {
			source, found, err := context.sources.resolveSource(candidate, true, false)
			if err != nil {
				return "", false, fmt.Errorf("resolve Codex platform package: %w", err)
			}
			if found {
				return source, true, nil
			}
		}
	}
	return "", false, nil
}

func prepareCodex(context hostContext, config bool) (agentSetup, error) {
	privateHome, err := ensureStateDirectory(context.state, "home/.codex", 0o700)
	if err != nil {
		return agentSetup{}, err
	}
	setup := agentSetup{Environment: map[string]string{"CODEX_HOME": privateHome}}
	if !config {
		return setup, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return setup, err
	}
	hostCodexHome := filepath.Join(home, ".codex")
	if override, set := hostEnvValue(context.hostEnv, "CODEX_HOME"); set && override != "" {
		if !filepath.IsAbs(override) {
			return setup, fmt.Errorf("host CODEX_HOME must be an absolute path: %q", override)
		}
		hostCodexHome = override
	}
	root, found, err := context.sources.resolveSource(hostCodexHome, true, false)
	if err != nil {
		return setup, fmt.Errorf("Codex home: %w", err)
	}
	if found {
		if err := seedAgentFile(context, filepath.Join(root, "auth.json"), "home/.codex/auth.json"); err != nil {
			return setup, fmt.Errorf("Codex authentication: %w", err)
		}
		files := []string{"config.toml", "AGENTS.md", "hooks.json"}
		entries, err := os.ReadDir(root)
		if err != nil {
			return setup, err
		}
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".config.toml") {
				files = append(files, entry.Name())
			}
		}
		for _, name := range files {
			source, present, err := context.sources.resolveSource(filepath.Join(root, name), false, false)
			if err != nil {
				return setup, fmt.Errorf("Codex %s: %w", name, err)
			}
			if present {
				setup.Mounts = append(setup.Mounts, resourceMount{Source: source, Destination: filepath.Join(privateHome, name)})
			}
		}
		for _, name := range []string{"rules", "skills", "plugins", "agents", "prompts"} {
			source, present, err := context.sources.resolveSource(filepath.Join(root, name), true, false)
			if err != nil {
				return setup, fmt.Errorf("Codex %s: %w", name, err)
			}
			if present {
				setup.Mounts = append(setup.Mounts, resourceMount{Source: source, Destination: filepath.Join(privateHome, name)})
			}
		}
	}
	globalSkills, found, err := context.sources.resolveSource(filepath.Join(home, ".agents", "skills"), true, false)
	if err != nil {
		return setup, fmt.Errorf("Codex global skills: %w", err)
	}
	if found {
		setup.Mounts = append(setup.Mounts, resourceMount{Source: globalSkills, Destination: filepath.Join(context.state, "home", ".agents", "skills")})
	}
	return setup, nil
}

// Codex cannot create its inner user namespace in the Podman-off sandbox.
// The CLI's bypass flag is meant for environments with an outer sandbox.
func prepareCodexCommand(requested []string, command preparedCommand, podmanEnabled bool) (preparedCommand, error) {
	if podmanEnabled {
		return command, nil
	}
	if len(requested) > 1 && requested[1] == "sandbox" {
		return command, nil
	}
	bypassRequested := false
	for index, arg := range requested[1:] {
		if arg == "--" {
			break
		}
		if arg == codexBypassFlag {
			bypassRequested = true
			continue
		}
		if arg == "--sandbox" || arg == "-s" || strings.HasPrefix(arg, "--sandbox=") ||
			arg == "--ask-for-approval" || arg == "-a" || strings.HasPrefix(arg, "--ask-for-approval=") ||
			arg == "--full-auto" {
			return preparedCommand{}, fmt.Errorf("Codex sandbox and approval flags conflict with bwrap-agent's Podman-off sandbox; remove them or enable Podman")
		}
		if arg == "-c" || arg == "--config" {
			if index+2 < len(requested) && codexSecurityOverride(requested[index+2]) {
				return preparedCommand{}, fmt.Errorf("Codex sandbox and approval configuration conflicts with bwrap-agent's Podman-off sandbox; remove it or enable Podman")
			}
		} else if strings.HasPrefix(arg, "-c=") || strings.HasPrefix(arg, "--config=") {
			_, value, _ := strings.Cut(arg, "=")
			if codexSecurityOverride(value) {
				return preparedCommand{}, fmt.Errorf("Codex sandbox and approval configuration conflicts with bwrap-agent's Podman-off sandbox; remove it or enable Podman")
			}
		}
	}
	if bypassRequested {
		return command, nil
	}
	command.Argv = append([]string{command.Argv[0], codexBypassFlag}, command.Argv[1:]...)
	return command, nil
}

func codexSecurityOverride(value string) bool {
	return strings.HasPrefix(value, "sandbox_mode=") || strings.HasPrefix(value, "approval_policy=")
}
