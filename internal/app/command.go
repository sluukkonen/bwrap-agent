package app

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// preparedCommand keeps executable arguments and the resources they require
// together, independently of agent configuration.
type preparedCommand struct {
	Argv        []string
	Mounts      []resourceMount
	Environment map[string]string
}

type externalCommandResolver func(hostContext, []string, string) (preparedCommand, error)

func resolveCommand(context hostContext, requested []string, resolveExternal externalCommandResolver) (preparedCommand, error) {
	found, err := exec.LookPath(requested[0])
	if err != nil {
		return preparedCommand{}, fmt.Errorf("command not found: %s", requested[0])
	}
	absolute, err := filepath.Abs(found)
	if err != nil {
		return preparedCommand{}, err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return preparedCommand{}, err
	}
	for _, prefix := range []string{"/usr/", "/bin/", "/sbin/", "/lib/", "/lib64/"} {
		if strings.HasPrefix(resolved, prefix) {
			return preparedCommand{Argv: append([]string(nil), requested...)}, nil
		}
	}
	if resolveExternal != nil {
		return resolveExternal(context, requested, resolved)
	}
	return standaloneCommand(requested, resolved), nil
}

func standaloneCommand(requested []string, resolved string) preparedCommand {
	const destination = "/run/bwrap-agent/command"
	return preparedCommand{
		Argv:   append([]string{destination}, requested[1:]...),
		Mounts: []resourceMount{{Source: resolved, Destination: destination, Executable: true}},
	}
}
