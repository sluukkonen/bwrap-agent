package app

import (
	"reflect"
	"testing"
)

func TestSandboxEnvironmentPrecedence(t *testing.T) {
	input := environmentInputs{home: homeLayout{home: "/home/test", canonicalHome: "/home/test", state: "/state"}, identity: instanceIdentity{State: "/state", Instance: "instance"}, defaultAccount: "fallback", hostEnv: []string{"TERM=host", "LANG=host-lang", "TZ=host-tz", "SECRET=hidden"}, agent: map[string]string{"TERM": "agent", "LANG": "agent-lang"}, command: map[string]string{"TERM": "command", "LANG": "command-lang"}}
	for _, tc := range []struct {
		name       string
		env, unset []string
		want       string
		present    bool
	}{
		{"command wins", nil, nil, "command", true},
		{"user wins", []string{"TERM=user"}, nil, "user", true},
		{"empty value", []string{"TERM="}, nil, "", true},
		{"last assignment", []string{"TERM=first", "TERM=last=part"}, nil, "last=part", true},
		{"unset wins", []string{"TERM=user"}, []string{"TERM"}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, err := buildSandboxEnvironment(Options{Network: "private", WorkspaceMode: "read-only", Env: tc.env, UnsetEnv: tc.unset}, input)
			if err != nil {
				t.Fatal(err)
			}
			if value, found := env["TERM"]; value != tc.want || found != tc.present {
				t.Fatalf("TERM = %q, %v", value, found)
			}
			for key, want := range map[string]string{"LANG": "host-lang", "TZ": "host-tz", "HTTP_PROXY": "http://127.0.0.1:65532", "GIT_OPTIONAL_LOCKS": "0", "HOME": "/home/test", "BWRAP_AGENT_INSTANCE": "instance", "USER": "fallback"} {
				if env[key] != want {
					t.Errorf("%s = %q, want %q", key, env[key], want)
				}
			}
			if _, found := env["SECRET"]; found {
				t.Fatal("inherited unselected host secret")
			}
			env["TERM"] = "mutated"
		})
	}
	if input.agent["TERM"] != "agent" || input.command["TERM"] != "command" || input.hostEnv[0] != "TERM=host" {
		t.Fatal("mutated environment inputs")
	}
	// Removing a higher layer exposes the next lower layer.
	input.command = nil
	env, _ := buildSandboxEnvironment(Options{}, input)
	if env["TERM"] != "agent" {
		t.Fatal("agent did not override host terminal value")
	}
	input.agent = nil
	env, _ = buildSandboxEnvironment(Options{}, input)
	if env["TERM"] != "host" {
		t.Fatal("host terminal value was lost")
	}
}

func TestSandboxEnvironmentAbsentAndEmptyHostValues(t *testing.T) {
	for _, host := range [][]string{nil, {"USER=", "LOGNAME=", "LANG=", "TZ=", "LC_CTYPE=", "OPENTUI_TEST=", "PATH="}} {
		input := environmentInputs{home: homeLayout{home: "/home/test", canonicalHome: "/home/test", state: "/state"}, defaultAccount: "fallback", hostEnv: host, agent: map[string]string{"LANG": "agent"}}
		env, err := buildSandboxEnvironment(Options{}, input)
		if err != nil {
			t.Fatal(err)
		}
		if host == nil {
			if env["USER"] != "fallback" || env["LOGNAME"] != "fallback" || env["LANG"] != "agent" {
				t.Fatalf("absent host values: %#v", env)
			}
			if _, found := env["TZ"]; found {
				t.Fatal("inherited absent TZ")
			}
		} else {
			for _, key := range []string{"USER", "LOGNAME", "LANG", "TZ", "LC_CTYPE", "OPENTUI_TEST"} {
				if value, found := env[key]; !found || value != "" {
					t.Fatalf("empty %s = %q, %v", key, value, found)
				}
			}
		}
	}
}

func TestSandboxEnvironmentValidation(t *testing.T) {
	for _, assignment := range []string{"missing-equals", "=value", "KEY=bad\x00value"} {
		if _, err := buildSandboxEnvironment(Options{Env: []string{assignment}}, environmentInputs{home: homeLayout{home: "/home/test", canonicalHome: "/home/test", state: "/state"}}); err == nil {
			t.Fatalf("accepted %q", assignment)
		}
	}
	for _, name := range []string{"", "KEY=value", "bad\x00name"} {
		if _, err := buildSandboxEnvironment(Options{UnsetEnv: []string{name}}, environmentInputs{home: homeLayout{home: "/home/test", canonicalHome: "/home/test", state: "/state"}}); err == nil {
			t.Fatalf("accepted unset %q", name)
		}
	}
}

func TestCodexHomeCannotBeRedirected(t *testing.T) {
	input := environmentInputs{
		home:  homeLayout{home: "/home/test", canonicalHome: "/home/test", state: "/state"},
		agent: map[string]string{"CODEX_HOME": "/state/home/.codex"},
	}
	for _, opts := range []Options{
		{Env: []string{"CODEX_HOME=/host/.codex"}},
		{UnsetEnv: []string{"CODEX_HOME"}},
	} {
		if _, err := buildSandboxEnvironment(opts, input); err == nil {
			t.Fatalf("accepted CODEX_HOME override: %#v", opts)
		}
	}
	env, err := buildSandboxEnvironment(Options{Env: []string{"CODEX_HOME=/home/test/.codex"}}, input)
	if err != nil || env["CODEX_HOME"] != "/home/test/.codex" {
		t.Fatalf("private CODEX_HOME = %q, %v", env["CODEX_HOME"], err)
	}
}

func TestEnvironmentSecurityAndLauncherBoundary(t *testing.T) {
	for _, podman := range []bool{false, true} {
		input := environmentInputs{home: homeLayout{home: "/home/test", canonicalHome: "/home/test", state: "/state"}, podman: podman, hostAccount: "real-account", hostEnv: []string{"PATH=/host/bin"}, storageConfig: "/state/storage.conf", containersConfig: "/state/containers.conf"}
		opts := Options{Env: []string{"USER=sandbox", "LOGNAME=sandbox", "PATH=/sandbox/bin", "BWRAP_AGENT_PODMAN=wrong", "PODMAN_NO_PAUSE_PROCESS=0", "CONTAINERS_CONF=/untrusted", "CONTAINERS_STORAGE_CONF=/untrusted", "CONTAINERS_CONF_OVERRIDE=/untrusted", "CONTAINERS_GRAPHROOT=/untrusted", "CONTAINERS_RUNROOT=/untrusted", internalLandlockEnvironment + "=untrusted"}, UnsetEnv: []string{"BWRAP_AGENT_PODMAN", "CONTAINERS_STORAGE_CONF"}}
		sandbox, err := buildSandboxEnvironment(opts, input)
		if err != nil {
			t.Fatal(err)
		}
		if sandbox["BWRAP_AGENT_PODMAN"] != boolString(podman) {
			t.Fatal("user override changed Podman status")
		}
		if _, found := sandbox[internalLandlockEnvironment]; found {
			t.Fatal("accepted user Landlock payload")
		}
		if podman {
			for key, want := range map[string]string{"PODMAN_NO_PAUSE_PROCESS": "1", "CONTAINERS_STORAGE_CONF": input.storageConfig, "CONTAINERS_CONF_OVERRIDE": input.containersConfig} {
				if sandbox[key] != want {
					t.Errorf("%s = %q", key, sandbox[key])
				}
			}
			for _, key := range []string{"CONTAINERS_CONF", "CONTAINERS_GRAPHROOT", "CONTAINERS_RUNROOT"} {
				if _, found := sandbox[key]; found {
					t.Errorf("retained %s", key)
				}
			}
		}
		before := cloneMap(sandbox)
		launch := buildLauncherEnvironment(sandbox, input)
		if !reflect.DeepEqual(sandbox, before) {
			t.Fatal("launcher derivation mutated sandbox")
		}
		if podman {
			if launch["USER"] != "real-account" || launch["LOGNAME"] != "real-account" || launch["PATH"] != "/host/bin" || launch["HOME"] != podmanBootstrapPlaceholder+"/home" {
				t.Fatalf("launcher identity or paths: %#v", launch)
			}
		} else {
			if launch["USER"] != "sandbox" || launch["PATH"] != "/sandbox/bin" {
				t.Fatal("changed non-Podman launcher values")
			}
			if _, found := launch["XDG_RUNTIME_DIR"]; found {
				t.Fatal("private runtime leaked to launcher")
			}
		}
		launch["USER"] = "mutated"
		if sandbox["USER"] != "sandbox" {
			t.Fatal("launcher aliases sandbox")
		}
	}
}
