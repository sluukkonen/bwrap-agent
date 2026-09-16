package app

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

type instanceSnapshot struct {
	root           string
	metadata       instanceMetadata
	status         string
	diskUsageBytes uint64
}

type fileIdentity struct {
	device uint64
	inode  uint64
}

func allocatedDiskUsage(root string) (uint64, error) {
	return allocatedDiskUsageWith(root, os.Lstat)
}

func ignorableDiskUsageError(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission)
}

func allocatedDiskUsageWith(root string, lstat func(string) (os.FileInfo, error)) (uint64, error) {
	seen := map[fileIdentity]bool{}
	var total uint64
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if ignorableDiskUsageError(walkErr) {
				return nil
			}
			return walkErr
		}
		info, err := lstat(path)
		if err != nil {
			if ignorableDiskUsageError(err) {
				return nil
			}
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("unsupported file metadata for %s", path)
		}
		identity := fileIdentity{device: uint64(stat.Dev), inode: stat.Ino}
		if seen[identity] {
			return nil
		}
		seen[identity] = true
		if stat.Blocks > 0 {
			total += uint64(stat.Blocks) * 512
		}
		return nil
	})
	return total, err
}

func instanceStatus(root string) (string, error) {
	lock, err := acquireDirectoryLock(root, true)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return "running", nil
	}
	if err != nil {
		return "", err
	}
	if err := lock.Close(); err != nil {
		return "", err
	}
	return "stopped", nil
}

// readInstanceSnapshot loads metadata and probes status without walking storage.
func readInstanceSnapshot(store, name string) (instanceSnapshot, error) {
	metadata, found, err := existingManagedInstance(store, name)
	if err != nil {
		return instanceSnapshot{}, err
	}
	if !found {
		return instanceSnapshot{}, fmt.Errorf("%w: %s", errManagedInstanceNotFound, name)
	}
	root := filepath.Join(store, name)
	status, err := instanceStatus(root)
	if err != nil {
		return instanceSnapshot{}, fmt.Errorf("inspect instance %s status: %w", name, err)
	}
	return instanceSnapshot{root: root, metadata: metadata, status: status}, nil
}

func (snapshot *instanceSnapshot) measureDiskUsage() error {
	usage, err := allocatedDiskUsage(snapshot.root)
	if err != nil {
		return fmt.Errorf("measure instance %s: %w", snapshot.metadata.Name, err)
	}
	snapshot.diskUsageBytes = usage
	return nil
}

func inspectManagedInstance(store, name string) (instanceSnapshot, error) {
	snapshot, err := readInstanceSnapshot(store, name)
	if err != nil {
		return instanceSnapshot{}, err
	}
	if err := snapshot.measureDiskUsage(); err != nil {
		return instanceSnapshot{}, err
	}
	return snapshot, nil
}

func managedInstanceSnapshots() ([]instanceSnapshot, error) {
	store, found, err := managedInstancesDirectory(false)
	if err != nil || !found {
		return nil, err
	}
	registryLock, err := acquireDirectoryLock(store, false)
	if err != nil {
		return nil, fmt.Errorf("lock managed instance store: %w", err)
	}
	defer func() {
		if registryLock != nil {
			_ = registryLock.Close()
		}
	}()
	entries, err := os.ReadDir(store)
	if err != nil {
		return nil, err
	}
	snapshots := make([]instanceSnapshot, 0, len(entries))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), deletionTombstonePrefix) {
			continue
		}
		if !entry.IsDir() {
			return nil, fmt.Errorf("invalid entry in managed instance store: %s", entry.Name())
		}
		if _, err := safeName(entry.Name()); err != nil {
			return nil, fmt.Errorf("invalid entry in managed instance store: %s", entry.Name())
		}
		snapshot, err := readInstanceSnapshot(store, entry.Name())
		if err != nil {
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, errManagedInstanceNotFound) {
				continue
			}
			return nil, err
		}
		snapshots = append(snapshots, snapshot)
	}
	if err := registryLock.Close(); err != nil {
		return nil, err
	}
	registryLock = nil
	// Disk traversal can be slow; keep it outside the registry lock.
	sized := snapshots[:0]
	for _, snapshot := range snapshots {
		if err := snapshot.measureDiskUsage(); err != nil {
			return nil, err
		}
		if _, err := os.Lstat(snapshot.root); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, err
		}
		sized = append(sized, snapshot)
	}
	snapshots = sized
	sort.Slice(snapshots, func(left, right int) bool {
		a, b := snapshots[left].metadata, snapshots[right].metadata
		if a.LastUsedAt.Equal(b.LastUsedAt) {
			return a.Name < b.Name
		}
		return a.LastUsedAt.After(b.LastUsedAt)
	})
	return snapshots, nil
}
