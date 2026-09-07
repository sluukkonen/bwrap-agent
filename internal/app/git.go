package app

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const maxGitPointerSize = 64 * 1024

func readSmallRegularFile(path string, limit int64) (string, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return "", err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return "", fmt.Errorf("not a regular file: %s", path)
	}
	if stat.Size > limit {
		return "", fmt.Errorf("file exceeds %d bytes: %s", limit, path)
	}
	content, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return "", err
	}
	if int64(len(content)) > limit {
		return "", fmt.Errorf("file exceeds %d bytes: %s", limit, path)
	}
	return string(content), nil
}

func singleLine(value, description string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, "\r\n\x00") {
		return "", fmt.Errorf("invalid %s", description)
	}
	return value, nil
}

func resolveGitPath(base, value string) (string, error) {
	if !filepath.IsAbs(value) {
		value = filepath.Join(base, value)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(value))
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("not a directory: %s", resolved)
	}
	return resolved, nil
}

func pathWithin(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func externalPath(path, project string) string {
	if pathWithin(project, path) {
		return ""
	}
	return path
}

// validatedExternalGitCommonDir accepts only Git directory pointers with a
// verifiable relationship back to the selected worktree. This prevents an
// untrusted project from naming an unrelated repository for an extra mount.
func validatedExternalGitCommonDir(project string) (string, error) {
	dotGit := filepath.Join(project, ".git")
	info, err := os.Lstat(dotGit)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("inspect project Git metadata: %w", err)
	}
	if info.IsDir() {
		return "", nil
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("unsafe project Git metadata: %s must be a regular file or directory", dotGit)
	}
	content, err := readSmallRegularFile(dotGit, maxGitPointerSize)
	if err != nil {
		return "", fmt.Errorf("read project Git metadata: %w", err)
	}
	line, err := singleLine(content, "project .git pointer")
	if err != nil || !strings.HasPrefix(line, "gitdir: ") {
		return "", fmt.Errorf("unsafe project Git metadata: invalid .git pointer")
	}
	gitDir, err := resolveGitPath(project, strings.TrimPrefix(line, "gitdir: "))
	if err != nil {
		return "", fmt.Errorf("unsafe project Git metadata: resolve gitdir: %w", err)
	}

	commonText, err := readSmallRegularFile(filepath.Join(gitDir, "commondir"), maxGitPointerSize)
	if err == nil {
		commonValue, valueErr := singleLine(commonText, "Git commondir pointer")
		if valueErr != nil {
			return "", fmt.Errorf("unsafe project Git metadata: %w", valueErr)
		}
		common, resolveErr := resolveGitPath(gitDir, commonValue)
		if resolveErr != nil {
			return "", fmt.Errorf("unsafe project Git metadata: resolve commondir: %w", resolveErr)
		}
		if filepath.Dir(gitDir) != filepath.Join(common, "worktrees") {
			return "", fmt.Errorf("unsafe project Git metadata: gitdir is not registered beneath the common directory")
		}
		backlinkText, readErr := readSmallRegularFile(filepath.Join(gitDir, "gitdir"), maxGitPointerSize)
		if readErr != nil {
			return "", fmt.Errorf("unsafe project Git metadata: read worktree backlink: %w", readErr)
		}
		backlink, valueErr := singleLine(backlinkText, "Git worktree backlink")
		if valueErr != nil {
			return "", fmt.Errorf("unsafe project Git metadata: %w", valueErr)
		}
		if !filepath.IsAbs(backlink) {
			backlink = filepath.Join(gitDir, backlink)
		}
		backlink, resolveErr = filepath.EvalSymlinks(filepath.Clean(backlink))
		if resolveErr != nil || backlink != dotGit {
			return "", fmt.Errorf("unsafe project Git metadata: worktree backlink does not reference %s", dotGit)
		}
		return externalPath(common, project), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("unsafe project Git metadata: read commondir: %w", err)
	}

	config := filepath.Join(gitDir, "config")
	configInfo, err := os.Lstat(config)
	if err != nil || !configInfo.Mode().IsRegular() {
		return "", fmt.Errorf("unsafe project Git metadata: external gitdir has no regular config")
	}
	command := exec.Command("git", "config", "--file", config, "--no-includes", "--path", "--get", "core.worktree")
	command.Env = []string{"LC_ALL=C"}
	command.Stderr = nil
	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("unsafe project Git metadata: external gitdir is not associated with this project")
	}
	worktreeValue, err := singleLine(string(output), "core.worktree")
	if err != nil {
		return "", fmt.Errorf("unsafe project Git metadata: %w", err)
	}
	worktree, err := resolveGitPath(gitDir, worktreeValue)
	if err != nil || worktree != project {
		return "", fmt.Errorf("unsafe project Git metadata: external gitdir core.worktree does not reference %s", project)
	}
	return externalPath(gitDir, project), nil
}
