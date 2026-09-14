package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// hostContext supplies host resources independently of any agent adapter.
type hostContext struct {
	state   string
	hostEnv []string
	sources hostSourcePolicy
}

// hostSourcePolicy rejects sources that a sandbox could retarget, including
// symlink hops through protected roots even when the final target is outside.
type hostSourcePolicy struct {
	protectedPaths []string
}

// The instance identity supplies already-resolved project, state, and bind paths.
func newHostSourcePolicy(identity instanceIdentity) hostSourcePolicy {
	paths := []string{identity.Project, identity.State}
	if identity.GitCommon != "" {
		paths = append(paths, identity.GitCommon)
	}
	return hostSourcePolicy{protectedPaths: append(paths, identity.RWBind...)}
}

// resolvePath validates an absolute source path without imposing a file type.
// Callers retain their own regular-file, directory, or Unix-socket checks.
func (policy hostSourcePolicy) resolvePath(path string) (string, error) {
	candidates, err := pathResolutionCandidates(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	if resolved != path {
		candidates = append(candidates, resolved)
	}
	for _, candidate := range candidates {
		for _, protected := range policy.protectedPaths {
			if pathsOverlap(candidate, protected) {
				return "", fmt.Errorf("host source %s overlaps sandbox-writable or project path %s", path, protected)
			}
		}
	}
	return resolved, nil
}

type resourceMount struct {
	Source      string
	Destination string
	Executable  bool
}

func hostEnvValue(source []string, name string) (string, bool) {
	for _, assignment := range source {
		key, value, found := strings.Cut(assignment, "=")
		if found && key == name {
			return value, true
		}
	}
	return "", false
}

func (policy hostSourcePolicy) resolveSource(path string, wantDirectory bool, required bool) (string, bool, error) {
	expanded, err := expandUser(path)
	if err != nil {
		return "", false, err
	}
	lexical, err := filepath.Abs(expanded)
	if err != nil {
		return "", false, err
	}
	lexical = filepath.Clean(lexical)
	info, err := os.Lstat(lexical)
	if errors.Is(err, os.ErrNotExist) && !required {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	resolved, err := policy.resolvePath(lexical)
	if err != nil {
		return "", false, err
	}
	info, err = os.Stat(resolved)
	if err != nil {
		return "", false, err
	}
	if wantDirectory && !info.IsDir() {
		return "", false, fmt.Errorf("host source is not a directory: %s", lexical)
	}
	if !wantDirectory && !info.Mode().IsRegular() {
		return "", false, fmt.Errorf("host source is not a regular file: %s", lexical)
	}
	return resolved, true, nil
}

func pathWithin(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func pathsOverlap(left, right string) bool {
	return pathWithin(left, right) || pathWithin(right, left)
}

func pathResolutionCandidates(path string) ([]string, error) {
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("path is not absolute: %s", path)
	}
	seen := map[string]bool{}
	var candidates []string
	addCandidate := func(candidate string) {
		candidate = filepath.Clean(candidate)
		if !seen[candidate] {
			seen[candidate] = true
			candidates = append(candidates, candidate)
		}
	}
	addCandidate(path)

	parts := splitPathComponents(path)
	prefix := string(filepath.Separator)
	symlinkCount := 0
	for len(parts) > 0 {
		part := parts[0]
		parts = parts[1:]
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			prefix = filepath.Dir(prefix)
			continue
		}
		next := filepath.Join(prefix, part)
		info, err := os.Lstat(next)
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			prefix = next
			continue
		}
		// Record the symlink itself, not only the eventually resolved path. A
		// writable directory containing this hop can otherwise retarget a
		// future resolution without appearing in the final path.
		addCandidate(next)
		symlinkCount++
		if symlinkCount > 255 {
			return nil, fmt.Errorf("too many symlinks while resolving %s", path)
		}
		target, err := os.Readlink(next)
		if err != nil {
			return nil, err
		}
		if filepath.IsAbs(target) {
			prefix = string(filepath.Separator)
		}
		parts = append(splitPathComponents(target), parts...)
		rewritten := prefix
		if len(parts) != 0 {
			rewritten = filepath.Join(prefix, filepath.Join(parts...))
		}
		addCandidate(rewritten)
	}
	return candidates, nil
}

func splitPathComponents(path string) []string {
	trimmed := strings.TrimLeft(path, string(filepath.Separator))
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, string(filepath.Separator))
}
