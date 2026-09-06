package app

import (
	"encoding/json"
	"io"
)

const internalInitMode = "__bwrap_agent_sandbox_init"
const internalPodmanServiceMode = "__bwrap_agent_podman_service"

type PortMapping struct {
	Protocol string `json:"protocol"`
	Host     int    `json:"host"`
	Guest    int    `json:"guest"`
}

type ConfigSource struct {
	Scope string `json:"scope"`
	Path  string `json:"path"`
}

type LaunchPlan struct {
	Instance    string
	Project     string
	State       string
	WritePolicy string
	Command     []string
	Outer       []string
	Bwrap       []string
	Ports       []PortMapping
	LaunchEnv   map[string]string
	TTY         bool
	ConfigFiles []ConfigSource
}

func (p LaunchPlan) Argv() []string {
	argv := make([]string, 0, len(p.Outer)+len(p.Bwrap))
	argv = append(argv, p.Outer...)
	argv = append(argv, p.Bwrap...)
	return argv
}

func writePlanJSON(w io.Writer, p LaunchPlan) error {
	configFiles := append([]ConfigSource{}, p.ConfigFiles...)
	value := struct {
		Instance    string            `json:"instance"`
		Project     string            `json:"project"`
		State       string            `json:"state"`
		WritePolicy string            `json:"write_policy"`
		Command     []string          `json:"command"`
		Ports       []PortMapping     `json:"ports"`
		Environment map[string]string `json:"environment"`
		TTY         bool              `json:"tty"`
		ConfigFiles []ConfigSource    `json:"config_files"`
		Argv        []string          `json:"argv"`
	}{
		Instance: p.Instance, Project: p.Project, State: p.State, WritePolicy: p.WritePolicy,
		Command: p.Command, Ports: p.Ports, Environment: p.LaunchEnv, TTY: p.TTY,
		ConfigFiles: configFiles, Argv: p.Argv(),
	}
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
