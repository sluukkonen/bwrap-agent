package app

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestResolveCommandPreservesArgumentsAndResources(t *testing.T) {
	_, host := testHostContext(t)
	directory := t.TempDir()
	executable := filepath.Join(directory, "executable")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(directory, "alias")
	if err := os.Symlink(executable, alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory)
	for _, test := range []struct {
		name        string
		program     string
		resolver    externalCommandResolver
		destination string
	}{
		{"system", "/bin/true", func(hostContext, []string, string) (preparedCommand, error) {
			t.Error("system command called external resolver")
			return preparedCommand{}, nil
		}, "/bin/true"},
		{"standalone", "alias", nil, "/run/bwrap-agent/command"},
		{"Pi standalone fallback", "alias", resolvePiExternalCommand, "/run/bwrap-agent/command"},
	} {
		t.Run(test.name, func(t *testing.T) {
			requested := []string{test.program, "--literal=two words", "", "quote'and\""}
			original := append([]string(nil), requested...)
			command, err := resolveCommand(host, requested, test.resolver)
			if err != nil {
				t.Fatal(err)
			}
			want := append([]string{test.destination}, requested[1:]...)
			if !reflect.DeepEqual(command.Argv, want) {
				t.Fatalf("argv = %#v, want %#v", command.Argv, want)
			}
			if test.name == "system" {
				if len(command.Mounts) != 0 {
					t.Fatalf("system command added mounts: %#v", command.Mounts)
				}
			} else {
				wantMounts := []resourceMount{{Source: executable, Destination: test.destination, Executable: true}}
				if !reflect.DeepEqual(command.Mounts, wantMounts) {
					t.Fatalf("mounts = %#v, want %#v", command.Mounts, wantMounts)
				}
			}
			if len(command.Environment) != 0 {
				t.Fatalf("unexpected environment: %#v", command.Environment)
			}
			command.Argv[1] = "changed"
			if !reflect.DeepEqual(requested, original) {
				t.Fatalf("requested arguments modified: %#v", requested)
			}
		})
	}
}

func TestResolveCommandPropagatesExternalResolverError(t *testing.T) {
	directory := t.TempDir()
	executable := filepath.Join(directory, "command")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("external preparation failed")
	_, err := resolveCommand(hostContext{}, []string{executable}, func(_ hostContext, requested []string, resolved string) (preparedCommand, error) {
		if resolved != executable || requested[0] != executable {
			t.Errorf("resolver inputs = %#v, %q", requested, resolved)
		}
		return preparedCommand{}, sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("resolver error = %v", err)
	}
	_, err = resolveCommand(hostContext{}, []string{filepath.Join(directory, "missing")}, nil)
	if err == nil || err.Error() != "command not found: "+filepath.Join(directory, "missing") {
		t.Fatalf("missing command error = %v", err)
	}
}
