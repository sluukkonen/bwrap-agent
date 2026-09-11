package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

type controlPathKind uint8

const (
	controlPlainFile controlPathKind = iota + 1
	controlJSONFile
	controlDirectory
)

const maxOpenCodeGitignoreSize = 1024 * 1024

type controlPathSpec struct {
	root     string
	relative string
	kind     controlPathKind
	mask     string
}

type controlMount struct {
	option      string
	source      string
	destination string
}

type controlCleanup struct {
	path string
	kind controlPathKind
}

// inspectControlPath walks beneath a previously canonicalized root without
// following symlinks. A sandbox-writable ancestor must never be able to
// retarget a protection mount toward unrelated host data on the next launch.
func inspectControlPath(root, relative string) (string, bool, error) {
	parts, err := cleanRelative(filepath.Clean(relative))
	if err != nil || len(parts) == 0 {
		return "", false, fmt.Errorf("invalid protected path %q", relative)
	}
	current := root
	for index, part := range parts {
		current = filepath.Join(current, part)
		info, inspectErr := os.Lstat(current)
		if errors.Is(inspectErr, os.ErrNotExist) {
			return filepath.Join(root, relative), false, nil
		}
		if inspectErr != nil {
			return "", false, inspectErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", false, fmt.Errorf("protected control path %s traverses a symlink; use --allow-control-file-writes to bypass this protection", current)
		}
		if index < len(parts)-1 && !info.IsDir() {
			return "", false, fmt.Errorf("protected control path ancestor is not a directory: %s", current)
		}
		if index == len(parts)-1 && !info.Mode().IsRegular() && !info.IsDir() {
			return "", false, fmt.Errorf("protected control path is not a regular file or directory: %s", current)
		}
	}
	return filepath.Join(root, relative), true, nil
}

func controlMask(identity instanceIdentity, kind controlPathKind) (string, error) {
	switch kind {
	case controlDirectory:
		path, err := ensureStateDirectory(identity.Root, "control-masks/empty-dir", 0o700)
		if err != nil {
			return "", err
		}
		if err := os.Chmod(path, 0o500); err != nil {
			return "", err
		}
		return path, nil
	case controlJSONFile:
		return writeStateFile(identity.Root, "control-masks/empty.json", []byte("{}\n"), 0o400)
	case controlPlainFile:
		return writeStateFile(identity.Root, "control-masks/empty-file", nil, 0o400)
	default:
		return "", errors.New("unknown control-path mask kind")
	}
}

// OpenCode writes .opencode/.gitignore while loading project configuration.
// Give that housekeeping file a writable, instance-local backing file while
// the surrounding project control directory remains read-only.
func openCodeControlMask(identity instanceIdentity, content []byte) (string, string, error) {
	directory, err := ensureStateDirectory(identity.Root, "control-masks/opencode", 0o700)
	if err != nil {
		return "", "", err
	}
	gitignore, err := writeStateFile(identity.Root, "control-masks/opencode/.gitignore", content, 0o600)
	if err != nil {
		return "", "", err
	}
	return directory, gitignore, nil
}

func piNeedsIndividualProtection(project string) (bool, error) {
	piPath, found, err := inspectControlPath(project, ".pi")
	if err != nil || !found {
		return false, err
	}
	info, err := os.Lstat(piPath)
	if err != nil {
		return false, err
	}
	if !info.IsDir() {
		return false, nil
	}
	for _, relative := range []string{
		".pi/settings.json", ".pi/extensions", ".pi/skills", ".pi/prompts",
		".pi/themes", ".pi/SYSTEM.md", ".pi/APPEND_SYSTEM.md",
	} {
		_, exists, inspectErr := inspectControlPath(project, relative)
		if inspectErr != nil {
			return false, inspectErr
		}
		if exists {
			return true, nil
		}
	}
	return false, nil
}

func prepareControlFileProtection(opts Options, identity instanceIdentity) ([]controlMount, []string, []controlCleanup, error) {
	if opts.AllowControlFileWrites || opts.WorkspaceMode != "write-through" {
		return nil, nil, nil, nil
	}
	project := identity.Project
	var pins []controlMount
	specs := []controlPathSpec{
		{root: project, relative: projectConfigName, kind: controlPlainFile},
		{root: project, relative: "opencode.json", kind: controlJSONFile},
		{root: project, relative: "opencode.jsonc", kind: controlJSONFile},
	}
	if _, found, err := inspectControlPath(project, ".opencode"); err != nil {
		return nil, nil, nil, err
	} else if !found {
		// This host-only directory remains available if signal handling removes
		// the temporary project mountpoint while Bubblewrap is still starting.
		source := filepath.Join(identity.Root, "control-masks", "opencode")
		specs = append(specs, controlPathSpec{root: project, relative: ".opencode", kind: controlDirectory, mask: source})
	} else {
		pins = append(pins, controlMount{option: "--bind", source: filepath.Join(project, ".opencode"), destination: filepath.Join(project, ".opencode")})
		specs = append(specs,
			controlPathSpec{root: project, relative: ".opencode/opencode.json", kind: controlJSONFile},
			controlPathSpec{root: project, relative: ".opencode/opencode.jsonc", kind: controlJSONFile},
			controlPathSpec{root: project, relative: ".opencode/plugins", kind: controlDirectory},
			controlPathSpec{root: project, relative: ".opencode/tools", kind: controlDirectory},
		)
	}

	individualPi, err := piNeedsIndividualProtection(project)
	if err != nil {
		return nil, nil, nil, err
	}
	if !individualPi {
		specs = append(specs, controlPathSpec{root: project, relative: ".pi", kind: controlDirectory})
	} else {
		pins = append(pins, controlMount{option: "--bind", source: filepath.Join(project, ".pi"), destination: filepath.Join(project, ".pi")})
		specs = append(specs,
			controlPathSpec{root: project, relative: ".pi/settings.json", kind: controlJSONFile},
			controlPathSpec{root: project, relative: ".pi/extensions", kind: controlDirectory},
			controlPathSpec{root: project, relative: ".pi/npm", kind: controlDirectory},
			controlPathSpec{root: project, relative: ".pi/git", kind: controlDirectory},
		)
	}

	git := identity.Git
	if git.DotGit != "" {
		if git.DotGitFile {
			specs = append(specs, controlPathSpec{root: project, relative: ".git", kind: controlPlainFile})
		}
		if git.CommonDir != "" {
			specs = append(specs,
				controlPathSpec{root: git.CommonDir, relative: "config", kind: controlPlainFile},
				controlPathSpec{root: git.CommonDir, relative: "hooks", kind: controlDirectory},
			)
		}
		if git.GitDir != "" {
			pins = append(pins, controlMount{option: "--bind", source: git.GitDir, destination: git.GitDir})
			specs = append(specs, controlPathSpec{root: git.GitDir, relative: "config.worktree", kind: controlPlainFile})
		}
	}

	seen := map[string]bool{}
	mounts := append([]controlMount(nil), pins...)
	var protected []string
	var cleanup []controlCleanup
	for _, spec := range specs {
		destination, found, err := inspectControlPath(spec.root, spec.relative)
		if err != nil {
			return nil, nil, nil, err
		}
		if seen[destination] {
			continue
		}
		seen[destination] = true
		source := destination
		if !found {
			if spec.mask != "" {
				source = spec.mask
			} else {
				source, err = controlMask(identity, spec.kind)
				if err != nil {
					return nil, nil, nil, fmt.Errorf("create neutral mask for %s: %w", destination, err)
				}
			}
			cleanup = append(cleanup, controlCleanup{path: destination, kind: spec.kind})
		}
		mounts = append(mounts, controlMount{option: "--ro-bind", source: source, destination: destination})
		protected = append(protected, destination)
	}
	gitignoreDestination, gitignoreFound, err := inspectControlPath(project, ".opencode/.gitignore")
	if err != nil {
		return nil, nil, nil, err
	}
	var gitignoreContent []byte
	if gitignoreFound {
		content, err := readSmallRegularFile(gitignoreDestination, maxOpenCodeGitignoreSize)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("read OpenCode housekeeping file: %w", err)
		}
		gitignoreContent = []byte(content)
	} else {
		cleanup = append(cleanup, controlCleanup{path: gitignoreDestination, kind: controlPlainFile})
	}
	_, openCodeGitignore, err := openCodeControlMask(identity, gitignoreContent)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create OpenCode control mask: %w", err)
	}
	// This must follow the read-only .opencode mount so the nested mount is
	// writable without exposing any host control file to OpenCode.
	mounts = append(mounts, controlMount{option: "--bind", source: openCodeGitignore, destination: gitignoreDestination})
	sort.Strings(protected)
	return mounts, protected, cleanup, nil
}
