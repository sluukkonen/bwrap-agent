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
	return readSmallRegularFileAt(unix.AT_FDCWD, path, limit)
}

func readSmallRegularFileAt(directory int, path string, limit int64) (string, error) {
	// O_NONBLOCK lets us reject FIFOs without waiting for a writer.
	fd, err := unix.Openat(directory, path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
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

func externalPath(path, project string) string {
	if pathWithin(project, path) {
		return ""
	}
	return path
}

type gitMetadata struct {
	DotGit         string
	DotGitFile     bool
	GitDir         string
	CommonDir      string
	ExternalCommon string
}

// validatedGitMetadata accepts only Git directory pointers with a verifiable
// relationship back to the selected worktree. Besides deciding which external
// directory may be mounted, it retains the canonical paths whose control files
// are protected later in plan construction.
func validatedGitMetadata(project string) (gitMetadata, error) {
	dotGit := filepath.Join(project, ".git")
	info, err := os.Lstat(dotGit)
	if errors.Is(err, os.ErrNotExist) {
		return gitMetadata{}, nil
	}
	if err != nil {
		return gitMetadata{}, fmt.Errorf("inspect project Git metadata: %w", err)
	}
	if info.IsDir() {
		return gitMetadata{DotGit: dotGit, GitDir: dotGit, CommonDir: dotGit}, nil
	}
	if !info.Mode().IsRegular() {
		return gitMetadata{}, fmt.Errorf("unsafe project Git metadata: %s must be a regular file or directory", dotGit)
	}
	content, err := readSmallRegularFile(dotGit, maxGitPointerSize)
	if err != nil {
		return gitMetadata{}, fmt.Errorf("read project Git metadata: %w", err)
	}
	line, err := singleLine(content, "project .git pointer")
	if err != nil || !strings.HasPrefix(line, "gitdir: ") {
		return gitMetadata{}, fmt.Errorf("unsafe project Git metadata: invalid .git pointer")
	}
	gitDir, err := resolveGitPath(project, strings.TrimPrefix(line, "gitdir: "))
	if err != nil {
		return gitMetadata{}, fmt.Errorf("unsafe project Git metadata: resolve gitdir: %w", err)
	}

	commonText, err := readSmallRegularFile(filepath.Join(gitDir, "commondir"), maxGitPointerSize)
	if err == nil {
		commonValue, valueErr := singleLine(commonText, "Git commondir pointer")
		if valueErr != nil {
			return gitMetadata{}, fmt.Errorf("unsafe project Git metadata: %w", valueErr)
		}
		common, resolveErr := resolveGitPath(gitDir, commonValue)
		if resolveErr != nil {
			return gitMetadata{}, fmt.Errorf("unsafe project Git metadata: resolve commondir: %w", resolveErr)
		}
		if filepath.Dir(gitDir) != filepath.Join(common, "worktrees") {
			return gitMetadata{}, fmt.Errorf("unsafe project Git metadata: gitdir is not registered beneath the common directory")
		}
		backlinkText, readErr := readSmallRegularFile(filepath.Join(gitDir, "gitdir"), maxGitPointerSize)
		if readErr != nil {
			return gitMetadata{}, fmt.Errorf("unsafe project Git metadata: read worktree backlink: %w", readErr)
		}
		backlink, valueErr := singleLine(backlinkText, "Git worktree backlink")
		if valueErr != nil {
			return gitMetadata{}, fmt.Errorf("unsafe project Git metadata: %w", valueErr)
		}
		if !filepath.IsAbs(backlink) {
			backlink = filepath.Join(gitDir, backlink)
		}
		backlink, resolveErr = filepath.EvalSymlinks(filepath.Clean(backlink))
		if resolveErr != nil || backlink != dotGit {
			return gitMetadata{}, fmt.Errorf("unsafe project Git metadata: worktree backlink does not reference %s", dotGit)
		}
		return gitMetadata{DotGit: dotGit, DotGitFile: true, GitDir: gitDir, CommonDir: common, ExternalCommon: externalPath(common, project)}, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return gitMetadata{}, fmt.Errorf("unsafe project Git metadata: read commondir: %w", err)
	}

	config := filepath.Join(gitDir, "config")
	configInfo, err := os.Lstat(config)
	if err != nil || !configInfo.Mode().IsRegular() {
		return gitMetadata{}, fmt.Errorf("unsafe project Git metadata: external gitdir has no regular config")
	}
	command := exec.Command("git", "config", "--file", config, "--no-includes", "--path", "--get", "core.worktree")
	command.Env = []string{"LC_ALL=C"}
	command.Stderr = nil
	output, err := command.Output()
	if err != nil {
		return gitMetadata{}, fmt.Errorf("unsafe project Git metadata: external gitdir is not associated with this project")
	}
	worktreeValue, err := singleLine(string(output), "core.worktree")
	if err != nil {
		return gitMetadata{}, fmt.Errorf("unsafe project Git metadata: %w", err)
	}
	worktree, err := resolveGitPath(gitDir, worktreeValue)
	if err != nil || worktree != project {
		return gitMetadata{}, fmt.Errorf("unsafe project Git metadata: external gitdir core.worktree does not reference %s", project)
	}
	return gitMetadata{DotGit: dotGit, DotGitFile: true, GitDir: gitDir, CommonDir: gitDir, ExternalCommon: externalPath(gitDir, project)}, nil
}

func validatedExternalGitCommonDir(project string) (string, error) {
	metadata, err := validatedGitMetadata(project)
	return metadata.ExternalCommon, err
}
