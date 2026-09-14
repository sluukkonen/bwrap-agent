package app

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

var errInstanceBusy = errors.New("sandbox instance is already running")

type instanceLock struct {
	fd int
}

func acquireInstanceLock(identity instanceIdentity) (*instanceLock, error) {
	fd, err := unix.Open(identity.Root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("could not open instance lock: %w", err)
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		unix.Close(fd)
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, fmt.Errorf("%w: %s (state %s); use a separate worktree or different instance", errInstanceBusy, identity.Instance, identity.State)
		}
		return nil, fmt.Errorf("could not lock instance %s: %w", identity.Instance, err)
	}
	return &instanceLock{fd: fd}, nil
}

func acquireDirectoryLock(path string, nonblocking bool) (*instanceLock, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	operation := unix.LOCK_EX
	if nonblocking {
		operation |= unix.LOCK_NB
	}
	if err := unix.Flock(fd, operation); err != nil {
		unix.Close(fd)
		return nil, err
	}
	return &instanceLock{fd: fd}, nil
}

func (lock *instanceLock) Close() error {
	if lock == nil || lock.fd < 0 {
		return nil
	}
	fd := lock.fd
	lock.fd = -1
	unlockErr := unix.Flock(fd, unix.LOCK_UN)
	closeErr := unix.Close(fd)
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

func secureMkdir(path string, mode uint32) error {
	if err := os.MkdirAll(path, fs.FileMode(mode)); err != nil {
		return err
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("unsafe directory path %s: %w", path, err)
	}
	defer unix.Close(fd)
	return unix.Fchmod(fd, mode)
}

func cleanRelative(relative string) ([]string, error) {
	if relative == "." {
		return nil, nil
	}
	if relative == "" || filepath.IsAbs(relative) || filepath.Clean(relative) != relative {
		return nil, fmt.Errorf("state path must be a clean relative path: %s", relative)
	}
	parts := strings.Split(filepath.ToSlash(relative), "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, fmt.Errorf("state path must be a clean relative path: %s", relative)
		}
	}
	return parts, nil
}

func openStateDirectory(state, relative string, mode uint32) (int, error) {
	parts, err := cleanRelative(relative)
	if err != nil {
		return -1, err
	}
	current, err := unix.Open(state, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("unsafe path beneath sandbox state %s: %w", filepath.Join(state, relative), err)
	}
	for _, part := range parts {
		if err := unix.Mkdirat(current, part, mode); err != nil && !errors.Is(err, unix.EEXIST) {
			unix.Close(current)
			return -1, fmt.Errorf("unsafe path beneath sandbox state %s: %w", filepath.Join(state, relative), err)
		}
		child, err := unix.Openat(current, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(current)
		if err != nil {
			return -1, fmt.Errorf("unsafe path beneath sandbox state %s: %w", filepath.Join(state, relative), err)
		}
		current = child
		if err := unix.Fchmod(current, mode); err != nil {
			unix.Close(current)
			return -1, err
		}
	}
	return current, nil
}

func ensureStateDirectory(state, relative string, mode uint32) (string, error) {
	fd, err := openStateDirectory(state, relative, mode)
	if err != nil {
		return "", err
	}
	unix.Close(fd)
	return filepath.Join(state, relative), nil
}

func unlinkStateFile(state, relative string) error {
	parts, err := cleanRelative(relative)
	if err != nil {
		return err
	}
	if len(parts) == 0 {
		return errors.New("state file path must name a file")
	}
	current, err := unix.Open(state, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer func() {
		_ = unix.Close(current)
	}()
	for _, part := range parts[:len(parts)-1] {
		child, err := unix.Openat(current, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			if errors.Is(err, unix.ENOENT) {
				return nil
			}
			return err
		}
		unix.Close(current)
		current = child
	}
	if err := unix.Unlinkat(current, parts[len(parts)-1], 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	return nil
}

func writeStateFile(state, relative string, content []byte, mode uint32) (string, error) {
	return writeStateFileFrom(state, relative, bytes.NewReader(content), mode)
}

func writeStateFileFrom(state, relative string, content io.Reader, mode uint32) (string, error) {
	if filepath.IsAbs(relative) || relative == "" {
		return "", fmt.Errorf("state file must be a clean relative path: %s", relative)
	}
	parent := filepath.Dir(relative)
	name := filepath.Base(relative)
	if name == "." || name == ".." || filepath.Clean(relative) != relative {
		return "", fmt.Errorf("state file must be a clean relative path: %s", relative)
	}
	parentFD, err := openStateDirectory(state, parent, 0o700)
	if err != nil {
		return "", err
	}
	defer unix.Close(parentFD)
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	temporary := fmt.Sprintf(".%s.tmp-%d-%s", name, os.Getpid(), hex.EncodeToString(random))
	fd, err := unix.Openat(parentFD, temporary, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, mode)
	if err != nil {
		return "", fmt.Errorf("could not safely write sandbox state file %s: %w", filepath.Join(state, relative), err)
	}
	created := true
	file := os.NewFile(uintptr(fd), temporary)
	defer func() {
		file.Close()
		if created {
			_ = unix.Unlinkat(parentFD, temporary, 0)
		}
	}()
	if _, err := io.Copy(file, content); err != nil {
		return "", err
	}
	if err := unix.Fchmod(fd, mode); err != nil {
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	if err := unix.Renameat(parentFD, temporary, parentFD, name); err != nil {
		return "", fmt.Errorf("could not safely write sandbox state file %s: %w", filepath.Join(state, relative), err)
	}
	created = false
	return filepath.Join(state, relative), nil
}
