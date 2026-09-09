package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
)

const internalInitMode = "__bwrap_agent_sandbox_init"
const internalPodmanInitMode = "__bwrap_agent_podman_sandbox_init"
const internalRuntimeMode = "__bwrap_agent_sandbox_runtime"
const internalPodmanServiceMode = "__bwrap_agent_podman_service"
const internalLaunchMode = "__bwrap_agent_launch_bubblewrap"
const internalLandlockEnvironment = "BWRAP_AGENT_INTERNAL_LANDLOCK_WRITES"

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
	WorkspaceMode  string
	Landlock       LandlockStatus
	Seccomp        SeccompStatus
	Bubblewrap     BubblewrapStatus
	NetworkAllow   []string
	ProxyGuestPort int
	Command        []string
	Outer          []string
	Launcher       []string
	Bwrap          []string
	Ports          []PortMapping
	LaunchEnv      map[string]string
	TTY            bool
	ConfigFiles    []ConfigSource
	ProtectedPaths []string
	ControlCleanup []controlCleanup
	Warnings       []string
}

type LandlockStatus struct {
	Requested string `json:"requested"`
	Effective string `json:"effective"`
	ABI       int    `json:"abi,omitempty"`
}

type SeccompStatus struct {
	Requested string `json:"requested"`
	Effective string `json:"effective"`
	Profile   string `json:"profile,omitempty"`
}

type BubblewrapStatus struct {
	Version string `json:"version"`
	Legacy  bool   `json:"legacy"`
}

func (p LaunchPlan) Argv() []string {
	argv := make([]string, 0, len(p.Outer)+len(p.Launcher)+len(p.Bwrap))
	argv = append(argv, p.Outer...)
	argv = append(argv, p.Launcher...)
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
		WorkspaceMode  string            `json:"workspace_mode"`
		Landlock       LandlockStatus    `json:"landlock"`
		Seccomp        SeccompStatus     `json:"seccomp"`
		Bubblewrap     BubblewrapStatus  `json:"bubblewrap"`
		NetworkAllow   []string          `json:"network_allow"`
		ProxyGuestPort int               `json:"proxy_guest_port,omitempty"`
		Command        []string          `json:"command"`
		Ports          []PortMapping     `json:"ports"`
		Environment    map[string]string `json:"environment"`
		TTY            bool              `json:"tty"`
		ConfigFiles    []ConfigSource    `json:"config_files"`
		ProtectedPaths []string          `json:"protected_paths"`
		Warnings       []string          `json:"warnings,omitempty"`
		Argv           []string          `json:"argv"`
	}{
		Instance: p.Instance, Project: p.Project, State: p.State, WorkspaceMode: p.WorkspaceMode,
		Landlock: p.Landlock, Seccomp: p.Seccomp, Bubblewrap: p.Bubblewrap,
		NetworkAllow: append([]string{}, p.NetworkAllow...), ProxyGuestPort: p.ProxyGuestPort,
		Command: p.Command, Ports: p.Ports, Environment: p.LaunchEnv, TTY: p.TTY,
		ConfigFiles: configFiles, ProtectedPaths: append([]string{}, p.ProtectedPaths...),
		Warnings: append([]string{}, p.Warnings...), Argv: p.Argv(),
	}
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
