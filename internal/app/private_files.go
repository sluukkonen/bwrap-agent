package app

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// The caller holds the instance lock until the sandbox exits. These files must
// remain linked: Podman bind-mounts its own resolver over /etc/resolv.conf, which
// Linux rejects when the destination is backed by an unlinked file.
func preparePrivateFiles(identity instanceIdentity) (string, error) {
	root, err := ensureStateDirectory(identity.Root, "generated", 0o700)
	if err != nil {
		return "", fmt.Errorf("prepare private files: %w", err)
	}
	if err := clearPrivateFiles(root); err != nil {
		return "", err
	}
	// Only remove launcher-generated legacy files, never user Maven settings
	// or other instance configuration. Unlinking must not follow state aliases.
	for _, relative := range []string{
		"config/etc/passwd", "config/etc/group", "config/etc/nsswitch.conf",
		"config/etc/hosts", "config/etc/resolv.conf", "config/maven/settings.xml",
	} {
		if err := unlinkStateFile(identity.State, relative); err != nil {
			return "", fmt.Errorf("remove legacy generated file %s: %w", relative, err)
		}
	}
	return root, nil
}

func clearPrivateFiles(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("read private files: %w", err)
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
			return fmt.Errorf("remove private file %s: %w", entry.Name(), err)
		}
	}
	return nil
}

func copyPrivateExecutable(root, name, source string) (string, error) {
	file, err := openInjectedFile(source)
	if err != nil {
		return "", fmt.Errorf("open executable snapshot source: %w", err)
	}
	defer file.Close()
	path, err := writeStateFileFrom(root, filepath.Join("executables", name), file, 0o555)
	if err != nil {
		return "", fmt.Errorf("copy executable snapshot: %w", err)
	}
	if err := unix.Access(path, unix.X_OK); err != nil {
		return "", fmt.Errorf("executable snapshot %s is not executable; the instance store must permit execution: %w", path, err)
	}
	return path, nil
}
