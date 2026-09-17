package app

import (
	"fmt"
	"os"
	"path/filepath"
)

// homeLayout separates persistent host storage from the paths used by programs
// inside the sandbox. Resource preparation continues to use backing paths.
type homeLayout struct {
	state         string
	home          string
	canonicalHome string
}

func resolveHomeLayout(identity instanceIdentity) (homeLayout, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return homeLayout{}, fmt.Errorf("locate sandbox home: %w", err)
	}
	if !filepath.IsAbs(home) || filepath.Clean(home) == "/" {
		return homeLayout{}, fmt.Errorf("sandbox home must be an absolute directory other than /: %q", home)
	}
	home = filepath.Clean(home)
	canonical, err := resolveExisting(home)
	if err != nil {
		return homeLayout{}, fmt.Errorf("resolve sandbox home: %w", err)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return homeLayout{}, err
	}
	if !info.IsDir() {
		return homeLayout{}, fmt.Errorf("sandbox home is not a directory: %s", home)
	}
	for _, candidate := range []string{home, canonical} {
		for _, reserved := range []string{"/usr", "/etc", "/proc", "/sys", "/dev", "/run", "/bin", "/sbin", "/lib", "/lib64"} {
			if pathsOverlap(candidate, reserved) {
				return homeLayout{}, fmt.Errorf("sandbox home %s overlaps reserved path %s", candidate, reserved)
			}
		}
		for _, reserved := range []string{"/tmp", "/var/tmp"} {
			if pathWithin(candidate, reserved) {
				return homeLayout{}, fmt.Errorf("sandbox home %s conflicts with sandbox path %s", candidate, reserved)
			}
		}
		if pathWithin(identity.State, candidate) {
			return homeLayout{}, fmt.Errorf("sandbox home %s is inside instance state %s", candidate, identity.State)
		}
		paths := append([]string{identity.Project, identity.GitCommon}, identity.ROBind...)
		paths = append(paths, identity.RWBind...)
		for _, path := range paths {
			if path != "" && pathWithin(path, candidate) {
				return homeLayout{}, fmt.Errorf("mount %s would replace private sandbox home %s; select a narrower path", path, candidate)
			}
		}
	}
	return homeLayout{state: identity.State, home: home, canonicalHome: canonical}, nil
}

func (layout homeLayout) backingHome() string {
	return filepath.Join(layout.state, "home")
}

func (layout homeLayout) destination(backing string) string {
	if pathWithin(layout.backingHome(), backing) {
		relative, _ := filepath.Rel(layout.backingHome(), backing)
		return filepath.Join(layout.canonicalHome, relative)
	}
	return backing
}

func (layout homeLayout) prepare() error {
	for _, relative := range []string{"home/.config", "home/.cache", "home/.local/share", "home/.local/state"} {
		if _, err := ensureStateDirectory(layout.state, relative, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// Validate the private backing entry, never the host file that happens to have
// the same pathname as a sandbox destination.
func (layout homeLayout) validateMount(source, destination string) error {
	if layout.home != layout.canonicalHome && pathWithin(layout.home, destination) {
		relative, _ := filepath.Rel(layout.home, destination)
		destination = filepath.Join(layout.canonicalHome, relative)
	}
	if pathWithin(layout.canonicalHome, destination) {
		relative, _ := filepath.Rel(layout.canonicalHome, destination)
		return validateStateMountpoint(layout.state, filepath.Join(layout.backingHome(), relative), source)
	}
	return nil
}

func (layout homeLayout) mountResource(mounts *mountBuilder, bind resourceMount) error {
	destination := layout.destination(bind.Destination)
	if err := layout.validateMount(bind.Source, destination); err != nil {
		return err
	}
	mounts.mount("--ro-bind", bind.Source, destination)
	return nil
}
