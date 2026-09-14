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

type agentSetup struct {
	Mounts      []resourceMount
	Environment map[string]string
}

type agentAdapter struct {
	name                   string
	programNames           map[string]bool
	prepare                func(hostContext, bool) (agentSetup, error)
	resolveExternalCommand func(hostContext, []string, string) ([]string, agentSetup, error)
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

func prepareAgent(adapter *agentAdapter, context hostContext, config bool) (agentSetup, error) {
	if adapter == nil || adapter.prepare == nil {
		return agentSetup{}, nil
	}
	setup, err := adapter.prepare(context, config)
	if err != nil {
		return agentSetup{}, fmt.Errorf("prepare %s integration: %w", adapter.name, err)
	}
	if setup.Environment == nil {
		setup.Environment = map[string]string{}
	}
	for _, mount := range setup.Mounts {
		if pathWithin(context.state, mount.Destination) {
			if err := validateStateMountpoint(context.state, mount.Destination, mount.Source); err != nil {
				return agentSetup{}, err
			}
		}
	}
	return setup, nil
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

func seedAgentFile(context hostContext, source, destination string) error {
	exists, err := stateRegularFileExists(context.state, destination)
	if err != nil || exists {
		return err
	}
	resolved, found, err := context.sources.resolveSource(source, false, false)
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

func prepareOpenCode(context hostContext, config bool) (agentSetup, error) {
	setup := agentSetup{Environment: map[string]string{}}
	if _, err := ensureStateDirectory(context.state, "config/opencode", 0o700); err != nil {
		return setup, err
	}
	if _, err := ensureStateDirectory(context.state, "data/opencode", 0o700); err != nil {
		return setup, err
	}
	if !config {
		return setup, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return setup, err
	}
	configBase := envValue(context.hostEnv, "XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	configSource, found, err := context.sources.resolveSource(filepath.Join(configBase, "opencode"), true, false)
	if err != nil {
		return setup, err
	}
	if found {
		setup.Mounts = append(setup.Mounts, resourceMount{Source: configSource, Destination: filepath.Join(context.state, "config", "opencode")})
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
		source, _, err := context.sources.resolveSource(value, override.directory, true)
		if err != nil {
			return setup, fmt.Errorf("%s: %w", override.name, err)
		}
		setup.Mounts = append(setup.Mounts, resourceMount{Source: source, Destination: override.target})
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

func preparePi(context hostContext, config bool) (agentSetup, error) {
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
	if !config {
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
	sourceDirectory, found, err := context.sources.resolveSource(hostDirectory, true, false)
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
			resolved, _, err := context.sources.resolveSource(source, info.IsDir(), true)
			if err != nil {
				return setup, err
			}
			setup.Mounts = append(setup.Mounts, resourceMount{Source: resolved, Destination: filepath.Join(agentDirectory, entry.Name())})
		}
	}
	globalSkills, found, err := context.sources.resolveSource(filepath.Join(home, ".agents", "skills"), true, false)
	if err != nil {
		return setup, err
	}
	if found {
		setup.Mounts = append(setup.Mounts, resourceMount{Source: globalSkills, Destination: filepath.Join(context.state, "home", ".agents", "skills")})
	}
	return setup, nil
}

func resolvePiExternalCommand(context hostContext, requested []string, resolved string) ([]string, agentSetup, error) {
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
		runtime, _, err = context.sources.resolveSource(runtime, false, true)
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
			Mounts: []resourceMount{
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
	return command, agentSetup{Mounts: []resourceMount{{Source: resolved, Destination: "/run/bwrap-agent/command", Executable: true}}}, nil
}
