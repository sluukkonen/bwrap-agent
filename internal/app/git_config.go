package app

import (
	"fmt"
	"os"
	"path/filepath"
)

// prepareGitConfigMounts exposes only the standard user files. Git resolves
// includes and other path settings itself within the sandbox filesystem.
func prepareGitConfigMounts(context hostContext, enabled bool) ([]resourceMount, error) {
	if !enabled {
		return nil, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("locate Git user configuration: %w", err)
	}
	configHome := envValue(context.hostEnv, "XDG_CONFIG_HOME", "")
	if configHome == "" {
		configHome = filepath.Join(home, ".config")
	}
	var mounts []resourceMount
	for _, candidate := range []resourceMount{
		{Source: filepath.Join(home, ".gitconfig"), Destination: filepath.Join(context.state, "home", ".gitconfig")},
		{Source: filepath.Join(configHome, "git", "config"), Destination: filepath.Join(context.state, "config", "git", "config")},
	} {
		source, found, err := context.sources.resolveSource(candidate.Source, false, false)
		if err != nil {
			return nil, fmt.Errorf("resolve Git user configuration %s: %w", candidate.Source, err)
		}
		if !found {
			continue
		}
		// Check readability now so failures identify the configuration source
		// rather than surfacing later as a generic Bubblewrap mount error.
		file, err := openInjectedFile(source)
		if err != nil {
			return nil, fmt.Errorf("open Git user configuration %s: %w", candidate.Source, err)
		}
		if err := file.Close(); err != nil {
			return nil, fmt.Errorf("close Git user configuration %s: %w", candidate.Source, err)
		}
		if err := validateStateMountpoint(context.state, candidate.Destination, source); err != nil {
			return nil, fmt.Errorf("prepare Git user configuration destination: %w", err)
		}
		mounts = append(mounts, resourceMount{Source: source, Destination: candidate.Destination})
	}
	return mounts, nil
}
