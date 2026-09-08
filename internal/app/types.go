package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
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
	Instance       string
	Project        string
	State          string
	WritePolicy    string
	NetworkAllow   []string
	ProxyGuestPort int
	Command        []string
	Outer          []string
	Bwrap          []string
	Ports          []PortMapping
	LaunchEnv      map[string]string
	TTY            bool
	ConfigFiles    []ConfigSource
	ProtectedPaths []string
	ControlCleanup []controlCleanup
}

func (p LaunchPlan) Argv() []string {
	argv := make([]string, 0, len(p.Outer)+len(p.Bwrap))
	argv = append(argv, p.Outer...)
	argv = append(argv, p.Bwrap...)
	return argv
}

func (p LaunchPlan) runtimeArgv(proxyHostPort int) ([]string, error) {
	argv := p.Argv()
	if p.ProxyGuestPort == 0 {
		return argv, nil
	}
	if proxyHostPort < 1 || proxyHostPort > 65535 {
		return nil, fmt.Errorf("invalid runtime proxy port %d", proxyHostPort)
	}
	replaced := false
	expected := strconv.Itoa(p.ProxyGuestPort) + ":" + proxyPortPlaceholder
	replacement := strconv.Itoa(p.ProxyGuestPort) + ":" + strconv.Itoa(proxyHostPort)
	for index := range p.Outer {
		if argv[index] == expected {
			argv[index] = replacement
			replaced = true
		}
	}
	if !replaced {
		return nil, errors.New("private network plan has no proxy port placeholder")
	}
	return argv, nil
}

func writePlanJSON(w io.Writer, p LaunchPlan) error {
	configFiles := append([]ConfigSource{}, p.ConfigFiles...)
	value := struct {
		Instance       string            `json:"instance"`
		Project        string            `json:"project"`
		State          string            `json:"state"`
		WritePolicy    string            `json:"write_policy"`
		NetworkAllow   []string          `json:"network_allow"`
		ProxyGuestPort int               `json:"proxy_guest_port,omitempty"`
		Command        []string          `json:"command"`
		Ports          []PortMapping     `json:"ports"`
		Environment    map[string]string `json:"environment"`
		TTY            bool              `json:"tty"`
		ConfigFiles    []ConfigSource    `json:"config_files"`
		ProtectedPaths []string          `json:"protected_paths"`
		Argv           []string          `json:"argv"`
	}{
		Instance: p.Instance, Project: p.Project, State: p.State, WritePolicy: p.WritePolicy,
		NetworkAllow: append([]string{}, p.NetworkAllow...), ProxyGuestPort: p.ProxyGuestPort,
		Command: p.Command, Ports: p.Ports, Environment: p.LaunchEnv, TTY: p.TTY,
		ConfigFiles: configFiles, ProtectedPaths: append([]string{}, p.ProtectedPaths...), Argv: p.Argv(),
	}
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
