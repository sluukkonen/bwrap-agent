package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

const maxOpenCodeGitignoreSize = 1024 * 1024

type controlPathSpec struct {
	root     string
	relative string
}

type controlMount struct {
	option      string
	source      string
	destination string
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

// OpenCode writes .opencode/.gitignore while loading project configuration.
// Give that housekeeping file a writable, instance-local backing file without
// modifying the existing host file.
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

func prepareControlFileProtection(opts Options, identity instanceIdentity) ([]controlMount, []string, error) {
	if opts.AllowControlFileWrites || opts.WorkspaceMode != "write-through" {
		return nil, nil, nil
	}
	project := identity.Project
	var pins []controlMount
	specs := []controlPathSpec{
		{root: project, relative: projectConfigName},
		{root: project, relative: "AGENTS.md"},
		{root: project, relative: ".codex"},
		{root: project, relative: "opencode.json"},
		{root: project, relative: "opencode.jsonc"},
	}
	if _, found, err := inspectControlPath(project, ".opencode"); err != nil {
		return nil, nil, err
	} else if found {
		pins = append(pins, controlMount{option: "--bind", source: filepath.Join(project, ".opencode"), destination: filepath.Join(project, ".opencode")})
		specs = append(specs,
			controlPathSpec{root: project, relative: ".opencode/opencode.json"},
			controlPathSpec{root: project, relative: ".opencode/opencode.jsonc"},
			controlPathSpec{root: project, relative: ".opencode/plugins"},
			controlPathSpec{root: project, relative: ".opencode/tools"},
		)
	}

	individualPi, err := piNeedsIndividualProtection(project)
	if err != nil {
		return nil, nil, err
	}
	if !individualPi {
		specs = append(specs, controlPathSpec{root: project, relative: ".pi"})
	} else {
		pins = append(pins, controlMount{option: "--bind", source: filepath.Join(project, ".pi"), destination: filepath.Join(project, ".pi")})
		specs = append(specs,
			controlPathSpec{root: project, relative: ".pi/settings.json"},
			controlPathSpec{root: project, relative: ".pi/extensions"},
			controlPathSpec{root: project, relative: ".pi/npm"},
			controlPathSpec{root: project, relative: ".pi/git"},
		)
	}

	git := identity.Git
	if git.DotGit != "" {
		if git.DotGitFile {
			specs = append(specs, controlPathSpec{root: project, relative: ".git"})
		}
		if git.CommonDir != "" {
			specs = append(specs,
				controlPathSpec{root: git.CommonDir, relative: "config"},
				controlPathSpec{root: git.CommonDir, relative: "hooks"},
			)
		}
		if git.GitDir != "" {
			pins = append(pins, controlMount{option: "--bind", source: git.GitDir, destination: git.GitDir})
			specs = append(specs, controlPathSpec{root: git.GitDir, relative: "config.worktree"})
		}
	}

	seen := map[string]bool{}
	mounts := append([]controlMount(nil), pins...)
	var protected []string
	for _, spec := range specs {
		destination, found, err := inspectControlPath(spec.root, spec.relative)
		if err != nil {
			return nil, nil, err
		}
		if seen[destination] {
			continue
		}
		seen[destination] = true
		if !found {
			continue
		}

		mounts = append(mounts, controlMount{option: "--ro-bind", source: destination, destination: destination})
		protected = append(protected, destination)
	}
	gitignoreDestination, gitignoreFound, err := inspectControlPath(project, ".opencode/.gitignore")
	if err != nil {
		return nil, nil, err
	}
	if gitignoreFound {
		content, err := readSmallRegularFile(gitignoreDestination, maxOpenCodeGitignoreSize)
		if err != nil {
			return nil, nil, fmt.Errorf("read OpenCode housekeeping file: %w", err)
		}
		_, backing, err := openCodeControlMask(identity, []byte(content))
		if err != nil {
			return nil, nil, fmt.Errorf("create OpenCode housekeeping backing: %w", err)
		}
		mounts = append(mounts, controlMount{option: "--bind", source: backing, destination: gitignoreDestination})
	}

	sort.Strings(protected)
	return mounts, protected, nil
}
