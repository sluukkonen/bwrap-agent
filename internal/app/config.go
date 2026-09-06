package app

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

type fileConfig struct {
	AgentConfig *bool          `toml:"agent_config"`
	Network     *string        `toml:"network"`
	Publish     []string       `toml:"publish"`
	Podman      *string        `toml:"podman"`
	WritePolicy *string        `toml:"write_policy"`
	ROBind      []string       `toml:"ro_bind"`
	RWBind      []string       `toml:"rw_bind"`
	Env         map[string]any `toml:"env"`
	UnsetEnv    []string       `toml:"unset_env"`
	TTY         *string        `toml:"tty"`
}

type envDirectiveKind uint8

const (
	envLiteral envDirectiveKind = iota
	envInherit
	envUnset
)

type envDirective struct {
	kind  envDirectiveKind
	value string
}

type optionLayer struct {
	agentConfig *bool
	network     *string
	publish     []string
	podman      *string
	writePolicy *string
	roBind      []string
	rwBind      []string
	environment map[string]envDirective
	tty         *string
}

func userConfigPath() (string, error) {
	directory, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate user configuration: %w", err)
	}
	return filepath.Join(directory, "bwrap-agent", "config.toml"), nil
}

func loadConfiguration(project string, noConfig, noProjectConfig bool) ([]optionLayer, []ConfigSource, error) {
	if noConfig {
		return nil, nil, nil
	}
	userPath, err := userConfigPath()
	if err != nil {
		return nil, nil, err
	}
	candidates := []ConfigSource{{Scope: "user", Path: userPath}}
	if !noProjectConfig {
		candidates = append(candidates, ConfigSource{Scope: "project", Path: filepath.Join(project, projectConfigName)})
	}
	layers := make([]optionLayer, 0, len(candidates))
	sources := make([]ConfigSource, 0, len(candidates))
	for _, source := range candidates {
		layer, found, err := loadConfigFile(source.Path)
		if err != nil {
			return nil, nil, fmt.Errorf("configuration %s: %w", source.Path, err)
		}
		if found {
			layers = append(layers, layer)
			sources = append(sources, source)
		}
	}
	return layers, sources, nil
}

func loadConfigFile(path string) (optionLayer, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return optionLayer{}, false, nil
		}
		return optionLayer{}, false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return optionLayer{}, false, err
	}
	if !info.Mode().IsRegular() {
		return optionLayer{}, false, fmt.Errorf("not a regular file")
	}
	var decoded fileConfig
	decoder := toml.NewDecoder(file).DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return optionLayer{}, false, err
	}
	layer, err := makeConfigLayer(decoded, filepath.Dir(path))
	if err != nil {
		return optionLayer{}, false, err
	}
	return layer, true, nil
}

func makeConfigLayer(config fileConfig, baseDirectory string) (optionLayer, error) {
	if err := validateChoice("network", config.Network, "private", "host", "none"); err != nil {
		return optionLayer{}, err
	}
	if err := validateChoice("podman", config.Podman, "auto", "on", "off"); err != nil {
		return optionLayer{}, err
	}
	if err := validateChoice("write_policy", config.WritePolicy, "workspace", "state-only"); err != nil {
		return optionLayer{}, err
	}
	if err := validateChoice("tty", config.TTY, "auto", "always", "never"); err != nil {
		return optionLayer{}, err
	}
	roBind, err := configPaths(config.ROBind, baseDirectory)
	if err != nil {
		return optionLayer{}, fmt.Errorf("ro_bind: %w", err)
	}
	rwBind, err := configPaths(config.RWBind, baseDirectory)
	if err != nil {
		return optionLayer{}, fmt.Errorf("rw_bind: %w", err)
	}
	environment := make(map[string]envDirective, len(config.Env)+len(config.UnsetEnv))
	for name, raw := range config.Env {
		if err := validateEnvName(name); err != nil {
			return optionLayer{}, fmt.Errorf("env.%s: %w", name, err)
		}
		directive, err := decodeConfigEnv(raw)
		if err != nil {
			return optionLayer{}, fmt.Errorf("env.%s: %w", name, err)
		}
		environment[name] = directive
	}
	for _, name := range config.UnsetEnv {
		if err := validateEnvName(name); err != nil {
			return optionLayer{}, fmt.Errorf("unset_env: %w", err)
		}
		environment[name] = envDirective{kind: envUnset}
	}
	return optionLayer{
		agentConfig: config.AgentConfig,
		network:     config.Network,
		publish:     config.Publish,
		podman:      config.Podman,
		writePolicy: config.WritePolicy,
		roBind:      roBind,
		rwBind:      rwBind,
		environment: environment,
		tty:         config.TTY,
	}, nil
}

func validateChoice(name string, value *string, choices ...string) error {
	if value == nil {
		return nil
	}
	for _, choice := range choices {
		if *value == choice {
			return nil
		}
	}
	return fmt.Errorf("%s must be one of %s, got %q", name, strings.Join(choices, ", "), *value)
}

func configPaths(paths []string, baseDirectory string) ([]string, error) {
	resolved := make([]string, 0, len(paths))
	for _, path := range paths {
		if path == "" {
			return nil, fmt.Errorf("path must not be empty")
		}
		expanded, err := expandUser(path)
		if err != nil {
			return nil, err
		}
		if !filepath.IsAbs(expanded) {
			expanded = filepath.Join(baseDirectory, expanded)
		}
		absolute, err := filepath.Abs(expanded)
		if err != nil {
			return nil, err
		}
		resolved = append(resolved, filepath.Clean(absolute))
	}
	return resolved, nil
}

func decodeConfigEnv(raw any) (envDirective, error) {
	switch value := raw.(type) {
	case string:
		if strings.ContainsRune(value, 0) {
			return envDirective{}, fmt.Errorf("literal value contains NUL")
		}
		return envDirective{kind: envLiteral, value: value}, nil
	case map[string]any:
		if len(value) != 1 {
			return envDirective{}, fmt.Errorf("inherit entry must contain only inherit = true")
		}
		inherit, found := value["inherit"]
		if !found {
			return envDirective{}, fmt.Errorf("expected a string or { inherit = true }")
		}
		enabled, ok := inherit.(bool)
		if !ok || !enabled {
			return envDirective{}, fmt.Errorf("inherit must be true")
		}
		return envDirective{kind: envInherit}, nil
	default:
		return envDirective{}, fmt.Errorf("expected a string or { inherit = true }")
	}
}

func parseCLIEnvironment(assignments, unset []string) (map[string]envDirective, error) {
	environment := make(map[string]envDirective, len(assignments)+len(unset))
	for _, assignment := range assignments {
		name, value, literal := strings.Cut(assignment, "=")
		if err := validateEnvName(name); err != nil || literal && strings.ContainsRune(value, 0) {
			return nil, fmt.Errorf("invalid --env assignment: %q", assignment)
		}
		if literal {
			environment[name] = envDirective{kind: envLiteral, value: value}
		} else {
			environment[name] = envDirective{kind: envInherit}
		}
	}
	for _, name := range unset {
		if err := validateEnvName(name); err != nil {
			return nil, fmt.Errorf("invalid --unsetenv name: %q", name)
		}
		environment[name] = envDirective{kind: envUnset}
	}
	return environment, nil
}

func validateEnvName(name string) error {
	if name == "" || strings.Contains(name, "=") || strings.ContainsRune(name, 0) {
		return fmt.Errorf("invalid environment variable name %q", name)
	}
	return nil
}

func resolveEnvironment(directives map[string]envDirective, hostEnvironment []string) ([]string, []string) {
	host := make(map[string]string, len(hostEnvironment))
	for _, assignment := range hostEnvironment {
		name, value, found := strings.Cut(assignment, "=")
		if found {
			host[name] = value
		}
	}
	names := make([]string, 0, len(directives))
	for name := range directives {
		names = append(names, name)
	}
	sort.Strings(names)
	var set, unset []string
	for _, name := range names {
		directive := directives[name]
		switch directive.kind {
		case envLiteral:
			set = append(set, name+"="+directive.value)
		case envInherit:
			if value, found := host[name]; found {
				set = append(set, name+"="+value)
			} else {
				unset = append(unset, name)
			}
		case envUnset:
			unset = append(unset, name)
		}
	}
	return set, unset
}

func mergeOptions(cli cliOptions, project string, layers []optionLayer, sources []ConfigSource, hostEnvironment []string) (Options, error) {
	opts := Options{
		Project:     project,
		Network:     "private",
		Podman:      "auto",
		WritePolicy: "workspace",
		TTY:         "auto",
		DryRun:      cli.DryRun,
		Command:     append([]string(nil), cli.Command...),
		ConfigFiles: append([]ConfigSource(nil), sources...),
	}
	if cli.Instance != nil {
		opts.Instance = *cli.Instance
	}
	if cli.StateDir != nil {
		opts.StateDir = *cli.StateDir
	}
	agentConfig := true
	environment := map[string]envDirective{}
	apply := func(layer optionLayer) {
		if layer.agentConfig != nil {
			agentConfig = *layer.agentConfig
		}
		if layer.network != nil {
			opts.Network = *layer.network
		}
		if layer.podman != nil {
			opts.Podman = *layer.podman
		}
		if layer.writePolicy != nil {
			opts.WritePolicy = *layer.writePolicy
		}
		if layer.tty != nil {
			opts.TTY = *layer.tty
		}
		opts.Publish = append(opts.Publish, layer.publish...)
		opts.ROBind = append(opts.ROBind, layer.roBind...)
		opts.RWBind = append(opts.RWBind, layer.rwBind...)
		for name, directive := range layer.environment {
			environment[name] = directive
		}
	}
	for _, layer := range layers {
		apply(layer)
	}
	cliEnvironment, err := parseCLIEnvironment(cli.Env, cli.UnsetEnv)
	if err != nil {
		return Options{}, err
	}
	apply(optionLayer{
		agentConfig: cli.AgentConfig,
		network:     cli.Network,
		publish:     cli.Publish,
		podman:      cli.Podman,
		writePolicy: cli.WritePolicy,
		roBind:      cli.ROBind,
		rwBind:      cli.RWBind,
		environment: cliEnvironment,
		tty:         cli.TTY,
	})
	opts.NoAgentConfig = !agentConfig
	opts.Env, opts.UnsetEnv = resolveEnvironment(environment, hostEnvironment)
	return opts, nil
}
