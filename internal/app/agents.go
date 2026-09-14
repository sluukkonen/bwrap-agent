package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

type agentMount struct {
	Source      string
	Destination string
	Executable  bool
}

type agentSetup struct {
	Mounts      []agentMount
	Environment map[string]string
}

type agentContext struct {
	state     string
	project   string
	gitCommon string
	rwBind    []string
	hostEnv   []string
	config    bool
}

type agentAdapter struct {
	name                   string
	programNames           map[string]bool
	prepare                func(agentContext) (agentSetup, error)
	resolveExternalCommand func(agentContext, []string, string) ([]string, agentSetup, error)
}

var agentAdapters = []*agentAdapter{
	{
		name:         "opencode",
		programNames: map[string]bool{"opencode": true, "opencode.exe": true},
		prepare:      prepareOpenCode,
	},
	{
		name:                   "pi",
		programNames:           map[string]bool{"pi": true, "pi.exe": true},
		prepare:                preparePi,
		resolveExternalCommand: resolvePiExternalCommand,
	},
}

func detectAgent(program string) *agentAdapter {
	base := filepath.Base(program)
	for _, adapter := range agentAdapters {
		if adapter.programNames[base] {
			return adapter
		}
	}
	return nil
}

func prepareAgent(adapter *agentAdapter, context agentContext) (agentSetup, error) {
	if adapter == nil || adapter.prepare == nil {
		return agentSetup{}, nil
	}
	setup, err := adapter.prepare(context)
	if err != nil {
		return agentSetup{}, fmt.Errorf("prepare %s integration: %w", adapter.name, err)
	}
	if setup.Environment == nil {
		setup.Environment = map[string]string{}
	}
	for _, mount := range setup.Mounts {
		if pathWithin(context.state, mount.Destination) {
			if err := validateAgentStateMountpoint(context.state, mount.Destination, mount.Source); err != nil {
				return agentSetup{}, err
			}
		}
	}
	return setup, nil
}

func validateAgentStateMountpoint(state, destination, source string) error {
	relative, err := filepath.Rel(state, destination)
	if err != nil {
		return err
	}
	parts, err := cleanRelative(relative)
	if err != nil || len(parts) == 0 {
		return fmt.Errorf("invalid agent configuration mount destination %s", destination)
	}
	parentFD, err := openStateDirectory(state, filepath.Join(parts[:len(parts)-1]...), 0o700)
	if err != nil {
		return err
	}
	defer unix.Close(parentFD)
	var destinationStat unix.Stat_t
	err = unix.Fstatat(parentFD, parts[len(parts)-1], &destinationStat, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	var sourceStat unix.Stat_t
	if err := unix.Stat(source, &sourceStat); err != nil {
		return err
	}
	destinationType := destinationStat.Mode & unix.S_IFMT
	sourceType := sourceStat.Mode & unix.S_IFMT
	if destinationType == unix.S_IFLNK || destinationType != sourceType {
		return fmt.Errorf("unsafe agent configuration mount destination %s", destination)
	}
	return nil
}

func hostEnvValue(source []string, name string) (string, bool) {
	for _, assignment := range source {
		key, value, found := strings.Cut(assignment, "=")
		if found && key == name {
			return value, true
		}
	}
	return "", false
}

func agentProtectedPaths(context agentContext) []string {
	paths := []string{context.project, context.state}
	if context.gitCommon != "" {
		paths = append(paths, context.gitCommon)
	}
	return append(paths, context.rwBind...)
}

func resolveAgentSource(context agentContext, path string, wantDirectory bool, required bool) (string, bool, error) {
	expanded, err := expandUser(path)
	if err != nil {
		return "", false, err
	}
	lexical, err := filepath.Abs(expanded)
	if err != nil {
		return "", false, err
	}
	lexical = filepath.Clean(lexical)
	info, err := os.Lstat(lexical)
	if errors.Is(err, os.ErrNotExist) && !required {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	candidates, err := pathResolutionCandidates(lexical)
	if err != nil {
		return "", false, err
	}
	resolved, err := filepath.EvalSymlinks(lexical)
	if err != nil {
		return "", false, err
	}
	if resolved != lexical {
		candidates = append(candidates, resolved)
	}
	for _, candidate := range candidates {
		for _, protected := range agentProtectedPaths(context) {
			if pathsOverlap(candidate, protected) {
				return "", false, fmt.Errorf("agent configuration source %s overlaps sandbox-writable or project path %s", lexical, protected)
			}
		}
	}
	info, err = os.Stat(resolved)
	if err != nil {
		return "", false, err
	}
	if wantDirectory && !info.IsDir() {
		return "", false, fmt.Errorf("agent configuration source is not a directory: %s", lexical)
	}
	if !wantDirectory && !info.Mode().IsRegular() {
		return "", false, fmt.Errorf("agent configuration source is not a regular file: %s", lexical)
	}
	return resolved, true, nil
}

func stateRegularFileExists(state, relative string) (bool, error) {
	parts, err := cleanRelative(relative)
	if err != nil || len(parts) == 0 {
		return false, err
	}
	parentFD, err := openStateDirectory(state, filepath.Join(parts[:len(parts)-1]...), 0o700)
	if err != nil {
		return false, err
	}
	defer unix.Close(parentFD)
	fd, err := unix.Openat(parentFD, parts[len(parts)-1], unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("unsafe sandbox state file %s: %w", filepath.Join(state, relative), err)
	}
	defer unix.Close(fd)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return false, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return false, fmt.Errorf("sandbox state file is not regular: %s", filepath.Join(state, relative))
	}
	return true, nil
}

func seedAgentFile(context agentContext, source, destination string) error {
	exists, err := stateRegularFileExists(context.state, destination)
	if err != nil || exists {
		return err
	}
	resolved, found, err := resolveAgentSource(context, source, false, false)
	if err != nil || !found {
		return err
	}
	content, err := os.ReadFile(resolved)
	if err != nil {
		return err
	}
	_, err = writeStateFile(context.state, destination, content, 0o600)
	return err
}

func prepareOpenCode(context agentContext) (agentSetup, error) {
	setup := agentSetup{Environment: map[string]string{}}
	if _, err := ensureStateDirectory(context.state, "config/opencode", 0o700); err != nil {
		return setup, err
	}
	if _, err := ensureStateDirectory(context.state, "data/opencode", 0o700); err != nil {
		return setup, err
	}
	if !context.config {
		return setup, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return setup, err
	}
	configBase := envValue(context.hostEnv, "XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	configSource, found, err := resolveAgentSource(context, filepath.Join(configBase, "opencode"), true, false)
	if err != nil {
		return setup, err
	}
	if found {
		setup.Mounts = append(setup.Mounts, agentMount{Source: configSource, Destination: filepath.Join(context.state, "config", "opencode")})
	}
	dataBase := envValue(context.hostEnv, "XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	if err := seedAgentFile(context, filepath.Join(dataBase, "opencode", "auth.json"), "data/opencode/auth.json"); err != nil {
		return setup, err
	}
	for _, override := range []struct {
		name      string
		directory bool
		target    string
	}{
		{"OPENCODE_CONFIG", false, "/run/bwrap-agent/agent-config/opencode/config"},
		{"OPENCODE_TUI_CONFIG", false, "/run/bwrap-agent/agent-config/opencode/tui-config"},
		{"OPENCODE_CONFIG_DIR", true, "/run/bwrap-agent/agent-config/opencode/config-dir"},
	} {
		value, set := hostEnvValue(context.hostEnv, override.name)
		if !set || value == "" {
			continue
		}
		source, _, err := resolveAgentSource(context, value, override.directory, true)
		if err != nil {
			return setup, fmt.Errorf("%s: %w", override.name, err)
		}
		setup.Mounts = append(setup.Mounts, agentMount{Source: source, Destination: override.target})
		setup.Environment[override.name] = override.target
	}
	return setup, nil
}

var piMutableEntries = map[string]bool{
	"auth.json":         true,
	"sessions":          true,
	"trust.json":        true,
	"models-store.json": true,
}

func preparePi(context agentContext) (agentSetup, error) {
	agentDirectory := filepath.Join(context.state, "home", ".pi", "agent")
	sessionDirectory := filepath.Join(agentDirectory, "sessions")
	setup := agentSetup{Environment: map[string]string{
		"PI_CODING_AGENT_DIR":         agentDirectory,
		"PI_CODING_AGENT_SESSION_DIR": sessionDirectory,
	}}
	if _, err := ensureStateDirectory(context.state, "home/.pi/agent/sessions", 0o700); err != nil {
		return setup, err
	}
	for _, name := range []string{"PI_HARDWARE_CURSOR", "PI_HYPERLINKS", "PI_IMAGE_PROTOCOL", "PI_TRUE_COLOR", "PI_TUI_ESC_TIMEOUT"} {
		if value, found := hostEnvValue(context.hostEnv, name); found {
			setup.Environment[name] = value
		}
	}
	if !context.config {
		return setup, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return setup, err
	}
	hostDirectory := filepath.Join(home, ".pi", "agent")
	if value, found := hostEnvValue(context.hostEnv, "PI_CODING_AGENT_DIR"); found && value != "" {
		hostDirectory = value
	}
	sourceDirectory, found, err := resolveAgentSource(context, hostDirectory, true, false)
	if err != nil {
		return setup, err
	}
	if found {
		for _, name := range []string{"auth.json", "trust.json", "models-store.json"} {
			if err := seedAgentFile(context, filepath.Join(sourceDirectory, name), filepath.Join("home/.pi/agent", name)); err != nil {
				return setup, err
			}
		}
		entries, err := os.ReadDir(sourceDirectory)
		if err != nil {
			return setup, err
		}
		for _, entry := range entries {
			if piMutableEntries[entry.Name()] {
				continue
			}
			source := filepath.Join(sourceDirectory, entry.Name())
			info, err := os.Stat(source)
			if err != nil {
				return setup, err
			}
			if !info.IsDir() && !info.Mode().IsRegular() {
				continue
			}
			resolved, _, err := resolveAgentSource(context, source, info.IsDir(), true)
			if err != nil {
				return setup, err
			}
			setup.Mounts = append(setup.Mounts, agentMount{Source: resolved, Destination: filepath.Join(agentDirectory, entry.Name())})
		}
	}
	globalSkills, found, err := resolveAgentSource(context, filepath.Join(home, ".agents", "skills"), true, false)
	if err != nil {
		return setup, err
	}
	if found {
		setup.Mounts = append(setup.Mounts, agentMount{Source: globalSkills, Destination: filepath.Join(context.state, "home", ".agents", "skills")})
	}
	return setup, nil
}

func resolvePiExternalCommand(context agentContext, requested []string, resolved string) ([]string, agentSetup, error) {
	for directory := filepath.Dir(resolved); directory != filepath.Dir(directory); directory = filepath.Dir(directory) {
		content, err := os.ReadFile(filepath.Join(directory, "package.json"))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, agentSetup{}, err
		}
		var manifest struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(content, &manifest) != nil || (manifest.Name != "@earendil-works/pi-coding-agent" && manifest.Name != "@mariozechner/pi-coding-agent") {
			continue
		}
		runtime, err := exec.LookPath("node")
		if err != nil {
			return nil, agentSetup{}, fmt.Errorf("Pi requires node in the host PATH")
		}
		runtime, _, err = resolveAgentSource(context, runtime, false, true)
		if err != nil {
			return nil, agentSetup{}, fmt.Errorf("resolve Pi node runtime: %w", err)
		}
		relative, err := filepath.Rel(directory, resolved)
		if err != nil || relative == "." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return nil, agentSetup{}, fmt.Errorf("Pi entrypoint is outside package root")
		}
		destination := "/run/bwrap-agent/command-package"
		command := append([]string{filepath.Join(destination, relative)}, requested[1:]...)
		commandSetup := agentSetup{
			Mounts: []agentMount{
				{Source: runtime, Destination: "/run/bwrap-agent/agent-runtime/node", Executable: true},
				{Source: directory, Destination: destination},
			},
			Environment: map[string]string{
				"PATH": "/run/bwrap-agent/agent-runtime:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
			},
		}
		return command, commandSetup, nil
	}
	command := append([]string{"/run/bwrap-agent/command"}, requested[1:]...)
	return command, agentSetup{Mounts: []agentMount{{Source: resolved, Destination: "/run/bwrap-agent/command", Executable: true}}}, nil
}
