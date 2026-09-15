package app

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
	"unicode"

	"golang.org/x/sys/unix"
)

const instanceMetadataVersion = 1
const instanceMetadataName = "metadata.json"
const deletionTombstonePrefix = ".deleting-"

var errManagedInstanceNotFound = errors.New("managed instance not found")

type instanceMetadata struct {
	Version    int       `json:"version"`
	Name       string    `json:"name"`
	Project    string    `json:"project"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at"`
}

type instanceRecord struct {
	Name           string    `json:"name"`
	Project        string    `json:"project"`
	Status         string    `json:"status"`
	DiskUsageBytes uint64    `json:"disk_usage_bytes"`
	CreatedAt      time.Time `json:"created_at"`
	LastUsedAt     time.Time `json:"last_used_at"`
	StatePath      string    `json:"state_path"`

	root     string
	metadata instanceMetadata
}

func managedInstancesDirectory(create bool) (string, bool, error) {
	_, directory, err := managedInstancesPaths()
	if err != nil {
		return "", false, err
	}
	if create {
		if err := secureMkdir(directory, 0o700); err != nil {
			return "", false, err
		}
		return directory, true, nil
	}
	info, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		return directory, false, nil
	}
	if err != nil {
		return "", false, err
	}
	if !info.IsDir() {
		return "", false, fmt.Errorf("managed instance store is not a directory: %s", directory)
	}
	return directory, true, nil
}

func managedInstancesPaths() (string, string, error) {
	base, err := stateBase()
	if err != nil {
		return "", "", err
	}
	base, err = filepath.Abs(base)
	if err != nil {
		return "", "", err
	}
	lexical := filepath.Clean(filepath.Join(base, "instances"))
	resolved, err := resolveState(lexical)
	if err != nil {
		return "", "", err
	}
	return lexical, resolved, nil
}

func validateInstanceStorePlacement(lexical, resolved, project, gitCommon string, rwBind []string) error {
	type protectedPath struct {
		label string
		path  string
	}
	protected := []protectedPath{{"project", project}}
	if gitCommon != "" {
		protected = append(protected, protectedPath{"external Git metadata", gitCommon})
	}
	for _, path := range rwBind {
		protected = append(protected, protectedPath{"read-write bind", path})
	}
	stores, err := pathResolutionCandidates(lexical)
	if err != nil {
		return fmt.Errorf("resolve managed instance store: %w", err)
	}
	stores = append(stores, resolved)
	for _, store := range stores {
		for _, candidate := range protected {
			if pathsOverlap(store, candidate.path) {
				return fmt.Errorf("managed instance store %s overlaps %s %s", store, candidate.label, candidate.path)
			}
		}
	}
	return nil
}

func readInstanceMetadata(root string) (instanceMetadata, error) {
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return instanceMetadata{}, err
	}
	defer unix.Close(rootFD)
	fd, err := unix.Openat(rootFD, instanceMetadataName, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return instanceMetadata{}, err
	}
	file := os.NewFile(uintptr(fd), instanceMetadataName)
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return instanceMetadata{}, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return instanceMetadata{}, errors.New("metadata is not a regular file")
	}
	if stat.Size > 64*1024 {
		return instanceMetadata{}, errors.New("metadata exceeds 64 KiB")
	}
	decoder := json.NewDecoder(io.LimitReader(file, 64*1024))
	decoder.DisallowUnknownFields()
	var metadata instanceMetadata
	if err := decoder.Decode(&metadata); err != nil {
		return instanceMetadata{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("unexpected trailing JSON value")
		}
		return instanceMetadata{}, err
	}
	if err := validateInstanceMetadata(metadata, filepath.Base(root)); err != nil {
		return instanceMetadata{}, err
	}
	return metadata, nil
}

func validateInstanceMetadata(metadata instanceMetadata, directoryName string) error {
	if metadata.Version != instanceMetadataVersion {
		return fmt.Errorf("unsupported metadata version %d", metadata.Version)
	}
	if _, err := safeName(metadata.Name); err != nil || metadata.Name != directoryName {
		return errors.New("metadata name does not match its directory")
	}
	if !filepath.IsAbs(metadata.Project) || filepath.Clean(metadata.Project) != metadata.Project {
		return errors.New("metadata project must be a clean absolute path")
	}
	if metadata.CreatedAt.IsZero() || metadata.LastUsedAt.IsZero() || metadata.LastUsedAt.Before(metadata.CreatedAt) {
		return errors.New("metadata timestamps are invalid")
	}
	return nil
}

func writeInstanceMetadata(root string, metadata instanceMetadata) error {
	content, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	content = append(content, '\n')
	_, err = writeStateFile(root, instanceMetadataName, content, 0o600)
	return err
}

func validateInstanceState(root string) error {
	state := filepath.Join(root, "state")
	info, err := os.Lstat(state)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("instance state is not a directory: %s", state)
	}
	return nil
}

func createManagedInstance(store, name, project string) (instanceMetadata, error) {
	root := filepath.Join(store, name)
	if err := os.Mkdir(root, 0o700); err != nil {
		return instanceMetadata{}, err
	}
	created := true
	defer func() {
		if created {
			_ = os.RemoveAll(root)
		}
	}()
	if err := secureMkdir(filepath.Join(root, "state"), 0o700); err != nil {
		return instanceMetadata{}, err
	}
	now := time.Now().UTC()
	metadata := instanceMetadata{Version: instanceMetadataVersion, Name: name, Project: project, CreatedAt: now, LastUsedAt: now}
	if err := writeInstanceMetadata(root, metadata); err != nil {
		return instanceMetadata{}, err
	}
	created = false
	return metadata, nil
}

func existingManagedInstance(store, name string) (instanceMetadata, bool, error) {
	root := filepath.Join(store, name)
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return instanceMetadata{}, false, nil
	}
	if err != nil {
		return instanceMetadata{}, false, err
	}
	if !info.IsDir() {
		return instanceMetadata{}, false, fmt.Errorf("invalid managed instance path: %s", root)
	}
	metadata, err := readInstanceMetadata(root)
	if err != nil {
		return instanceMetadata{}, false, fmt.Errorf("invalid instance %s: %w", name, err)
	}
	if err := validateInstanceState(root); err != nil {
		return instanceMetadata{}, false, fmt.Errorf("invalid instance %s: %w", name, err)
	}
	return metadata, true, nil
}

func resolveBindPaths(paths []string) ([]string, error) {
	resolved := make([]string, 0, len(paths))
	for _, path := range paths {
		canonical, err := resolveExisting(path)
		if err != nil {
			return nil, err
		}
		resolved = append(resolved, canonical)
	}
	return resolved, nil
}

func resolveAndLockInstance(opts Options) (instanceIdentity, *instanceLock, error) {
	project, err := resolveProjectDirectory(opts.Project)
	if err != nil {
		return instanceIdentity{}, nil, err
	}
	git, err := validatedGitMetadata(project)
	if err != nil {
		return instanceIdentity{}, nil, err
	}
	roBind, err := resolveBindPaths(opts.ROBind)
	if err != nil {
		return instanceIdentity{}, nil, err
	}
	rwBind, err := resolveBindPaths(opts.RWBind)
	if err != nil {
		return instanceIdentity{}, nil, err
	}
	return resolveManagedInstanceLocked(project, opts.Instance, git, roBind, rwBind)
}

func resolveManagedInstanceLocked(project, requested string, git gitMetadata, roBind, rwBind []string) (instanceIdentity, *instanceLock, error) {
	if _, err := projectTrustStore(project, git.ExternalCommon, rwBind); err != nil {
		return instanceIdentity{}, nil, err
	}

	lexicalStore, store, err := managedInstancesPaths()
	if err != nil {
		return instanceIdentity{}, nil, err
	}
	if err := validateInstanceStorePlacement(lexicalStore, store, project, git.ExternalCommon, rwBind); err != nil {
		return instanceIdentity{}, nil, err
	}
	if err := secureMkdir(store, 0o700); err != nil {
		return instanceIdentity{}, nil, err
	}
	registryLock, err := acquireDirectoryLock(store, false)
	if err != nil {
		return instanceIdentity{}, nil, fmt.Errorf("lock managed instance store: %w", err)
	}
	defer func() {
		if registryLock != nil {
			_ = registryLock.Close()
		}
	}()

	name := requested
	if name == "" {
		name = defaultName(project)
	}
	if name, err = safeName(name); err != nil {
		return instanceIdentity{}, nil, err
	}
	metadata, found, err := existingManagedInstance(store, name)
	if err != nil {
		return instanceIdentity{}, nil, err
	}
	if found && metadata.Project != project {
		if requested != "" {
			return instanceIdentity{}, nil, fmt.Errorf("instance %s belongs to project %s", name, metadata.Project)
		}
		name = collisionName(project)
		metadata, found, err = existingManagedInstance(store, name)
		if err != nil {
			return instanceIdentity{}, nil, err
		}
		if found && metadata.Project != project {
			return instanceIdentity{}, nil, fmt.Errorf("instance name collision for %s; use --instance", name)
		}
	}
	if !found {
		if _, err := createManagedInstance(store, name, project); err != nil {
			return instanceIdentity{}, nil, fmt.Errorf("create instance %s: %w", name, err)
		}
	}
	root := filepath.Join(store, name)
	identity := instanceIdentity{
		Project: project, Instance: name, State: filepath.Join(root, "state"), Root: root,
		GitCommon: git.ExternalCommon, Git: git, ROBind: roBind, RWBind: rwBind,
	}
	lock, err := acquireInstanceLock(identity)
	if err != nil {
		return instanceIdentity{}, nil, err
	}
	current, found, err := existingManagedInstance(store, name)
	if err != nil || !found || current.Project != project {
		_ = lock.Close()
		if err != nil {
			return instanceIdentity{}, nil, err
		}
		return instanceIdentity{}, nil, fmt.Errorf("managed instance %s changed while acquiring its lock", name)
	}
	if err := registryLock.Close(); err != nil {
		_ = lock.Close()
		return instanceIdentity{}, nil, err
	}
	registryLock = nil
	return identity, lock, nil
}

func markInstanceUsed(identity instanceIdentity) error {
	metadata, err := readInstanceMetadata(identity.Root)
	if err != nil {
		return err
	}
	if metadata.Name != identity.Instance || metadata.Project != identity.Project {
		return errors.New("managed instance identity changed")
	}
	metadata.LastUsedAt = time.Now().UTC()
	return writeInstanceMetadata(identity.Root, metadata)
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

func inspectManagedInstance(store, name string) (instanceRecord, error) {
	metadata, found, err := existingManagedInstance(store, name)
	if err != nil {
		return instanceRecord{}, err
	}
	if !found {
		return instanceRecord{}, fmt.Errorf("%w: %s", errManagedInstanceNotFound, name)
	}
	root := filepath.Join(store, name)
	status, err := instanceStatus(root)
	if err != nil {
		return instanceRecord{}, fmt.Errorf("inspect instance %s status: %w", name, err)
	}
	usage, err := allocatedDiskUsage(root)
	if err != nil {
		return instanceRecord{}, fmt.Errorf("measure instance %s: %w", name, err)
	}
	return instanceRecord{
		Name: name, Project: metadata.Project, Status: status, DiskUsageBytes: usage,
		CreatedAt: metadata.CreatedAt, LastUsedAt: metadata.LastUsedAt,
		StatePath: filepath.Join(root, "state"), root: root, metadata: metadata,
	}, nil
}

func managedInstanceRecords() ([]instanceRecord, error) {
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
	records := make([]instanceRecord, 0, len(entries))
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
		metadata, found, err := existingManagedInstance(store, entry.Name())
		if err != nil {
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, errManagedInstanceNotFound) {
				continue
			}
			return nil, err
		}
		if !found {
			continue
		}
		root := filepath.Join(store, entry.Name())
		status, err := instanceStatus(root)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("inspect instance %s status: %w", entry.Name(), err)
		}
		records = append(records, instanceRecord{
			Name: entry.Name(), Project: metadata.Project, Status: status,
			CreatedAt: metadata.CreatedAt, LastUsedAt: metadata.LastUsedAt,
			StatePath: filepath.Join(root, "state"), root: root, metadata: metadata,
		})
	}
	if err := registryLock.Close(); err != nil {
		return nil, err
	}
	registryLock = nil
	sized := records[:0]
	for _, record := range records {
		usage, err := allocatedDiskUsage(record.root)
		if err != nil {
			return nil, fmt.Errorf("measure instance %s: %w", record.Name, err)
		}
		if _, err := os.Lstat(record.root); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, err
		}
		record.DiskUsageBytes = usage
		sized = append(sized, record)
	}
	records = sized
	sort.Slice(records, func(left, right int) bool {
		if records[left].LastUsedAt.Equal(records[right].LastUsedAt) {
			return records[left].Name < records[right].Name
		}
		return records[left].LastUsedAt.After(records[right].LastUsedAt)
	})
	return records, nil
}

func humanBytes(bytes uint64) string {
	const unit = uint64(1024)
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	value := float64(bytes)
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	for _, suffix := range units {
		value /= 1024
		if value < 1024 || suffix == units[len(units)-1] {
			if value >= 10 {
				return fmt.Sprintf("%.0f %s", value, suffix)
			}
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%d B", bytes)
}

func tableField(value string) string {
	for _, char := range value {
		if unicode.IsControl(char) {
			return strconv.QuoteToGraphic(value)
		}
	}
	return value
}

func listInstances(asJSON bool, output io.Writer) error {
	records, err := managedInstanceRecords()
	if err != nil {
		return err
	}
	if asJSON {
		if records == nil {
			records = []instanceRecord{}
		}
		encoder := json.NewEncoder(output)
		encoder.SetIndent("", "  ")
		return encoder.Encode(records)
	}
	if len(records) == 0 {
		_, err := fmt.Fprintln(output, "No instances.")
		return err
	}
	writer := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(writer, "NAME\tSTATUS\tDISK USAGE\tLAST USED\tPROJECT"); err != nil {
		return err
	}
	for _, record := range records {
		if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n", record.Name, record.Status,
			humanBytes(record.DiskUsageBytes), record.LastUsedAt.Local().Format("2006-01-02 15:04"), tableField(record.Project)); err != nil {
			return err
		}
	}
	return writer.Flush()
}

func confirmedDeletion(name string, record instanceRecord, yes, interactive bool, input io.Reader, prompt io.Writer) (bool, error) {
	if yes {
		return true, nil
	}
	if !interactive {
		return false, errors.New("deletion requires an interactive terminal or --yes")
	}
	if _, err := fmt.Fprintf(prompt, "Delete instance %q for project %q (%s)? [y/N] ", name, record.Project, humanBytes(record.DiskUsageBytes)); err != nil {
		return false, err
	}
	answer, err := bufio.NewReader(input).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes", nil
}

func deletionTombstoneMatches(name, entry string) bool {
	prefix := deletionTombstonePrefix + name + "-"
	if !strings.HasPrefix(entry, prefix) {
		return false
	}
	suffix := strings.TrimPrefix(entry, prefix)
	if len(suffix) != 16 || suffix != strings.ToLower(suffix) {
		return false
	}
	decoded, err := hex.DecodeString(suffix)
	return err == nil && len(decoded) == 8
}

func tombstoneNeedsPodmanCleanup(tombstone string) (bool, error) {
	path := filepath.Join(tombstone, "state", "podman", "storage")
	info, err := os.Lstat(path)
	if err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
		return true, nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	entries, err := os.ReadDir(tombstone)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), ".podman-cleanup-") {
			return true, nil
		}
	}
	return false, nil
}

func podmanCleanupEnvironment(environment []string, accountName, root string) []string {
	values := make(map[string]string)
	for _, assignment := range environment {
		if name, value, found := strings.Cut(assignment, "="); found {
			values[name] = value
		}
	}
	values = podmanBootstrapEnvironment(values, root)
	values["USER"], values["LOGNAME"] = accountName, accountName
	return environmentList(values)
}

func withPodmanCleanupState(tombstone string, action func(root string, targets []string) error) (result error) {
	store := filepath.Dir(tombstone)
	if filepath.Base(store) != "instances" {
		return fmt.Errorf("invalid instance tombstone path: %s", tombstone)
	}
	entries, err := os.ReadDir(tombstone)
	if err != nil {
		return fmt.Errorf("read instance tombstone for Podman cleanup: %w", err)
	}
	targets := make([]string, 0, len(entries))
	for _, entry := range entries {
		targets = append(targets, filepath.Join(tombstone, entry.Name()))
	}
	cleanup, err := os.MkdirTemp(tombstone, ".podman-cleanup-")
	if err != nil {
		return fmt.Errorf("create Podman cleanup state: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(cleanup); err != nil {
			result = errors.Join(result, fmt.Errorf("remove Podman cleanup state: %w", err))
		}
	}()
	if err := preparePodmanBootstrap(cleanup); err != nil {
		return err
	}
	return action(cleanup, targets)
}

func podmanUnshareRemove(path string) error {
	environment := os.Environ()
	accountName, err := hostAccountName(environment)
	if err != nil {
		return fmt.Errorf("resolve Podman cleanup identity: %w", err)
	}
	podman, err := exec.LookPath("podman")
	if err != nil {
		return fmt.Errorf("Podman is required to remove rootless container storage: %w", err)
	}
	remove, err := exec.LookPath("rm")
	if err != nil {
		return fmt.Errorf("rm is required to remove rootless container storage: %w", err)
	}
	return withPodmanCleanupState(path, func(root string, targets []string) error {
		arguments := append([]string{"unshare", remove, "-rf", "--"}, targets...)
		command := exec.Command(podman, arguments...)
		command.Env = podmanCleanupEnvironment(environment, accountName, root)
		output, err := command.CombinedOutput()
		if err != nil {
			message := strings.TrimSpace(string(output))
			if message == "" {
				return fmt.Errorf("podman unshare cleanup: %w", err)
			}
			return fmt.Errorf("podman unshare cleanup: %w: %s", err, message)
		}
		for _, target := range targets {
			if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
				if err == nil {
					return fmt.Errorf("podman unshare cleanup left %s in place", target)
				}
				return fmt.Errorf("verify podman unshare cleanup of %s: %w", target, err)
			}
		}
		return nil
	})
}

func removeInstanceTombstoneWith(path string, removeAll, removeWithPodman func(string) error) error {
	needsPodmanCleanup, err := tombstoneNeedsPodmanCleanup(path)
	if err != nil {
		return fmt.Errorf("inspect instance tombstone: %w", err)
	}
	var podmanErr error
	if needsPodmanCleanup {
		podmanErr = removeWithPodman(path)
	}
	if err := removeAll(path); err != nil {
		if podmanErr != nil {
			return fmt.Errorf("Podman namespace removal failed (%v); direct removal also failed: %w", podmanErr, err)
		}
		return err
	}
	return nil
}

func removeInstanceTombstone(path string) error {
	return removeInstanceTombstoneWith(path, os.RemoveAll, podmanUnshareRemove)
}

func retryPendingInstanceDeletion(store, name string) (int, error) {
	registryLock, err := acquireDirectoryLock(store, false)
	if err != nil {
		return 0, fmt.Errorf("lock managed instance store: %w", err)
	}
	defer func() {
		if registryLock != nil {
			_ = registryLock.Close()
		}
	}()
	if _, found, err := existingManagedInstance(store, name); err != nil {
		return 0, err
	} else if found {
		return 0, fmt.Errorf("instance %s appeared while retrying deletion; retry the command", name)
	}
	entries, err := os.ReadDir(store)
	if err != nil {
		return 0, err
	}
	type lockedTombstone struct {
		path string
		lock *instanceLock
	}
	var tombstones []lockedTombstone
	closeLocks := func() {
		for _, tombstone := range tombstones {
			_ = tombstone.lock.Close()
		}
	}
	defer closeLocks()
	for _, entry := range entries {
		if !entry.IsDir() || !deletionTombstoneMatches(name, entry.Name()) {
			continue
		}
		path := filepath.Join(store, entry.Name())
		lock, err := acquireDirectoryLock(path, true)
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return 0, fmt.Errorf("instance %s deletion cleanup is already running", name)
		}
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("lock instance tombstone %s: %w", path, err)
		}
		tombstones = append(tombstones, lockedTombstone{path: path, lock: lock})
	}
	if err := registryLock.Close(); err != nil {
		return 0, err
	}
	registryLock = nil
	for _, tombstone := range tombstones {
		if err := removeInstanceTombstone(tombstone.path); err != nil {
			return 0, fmt.Errorf("remove instance tombstone %s: %w", tombstone.path, err)
		}
	}
	return len(tombstones), nil
}

func deleteManagedInstance(name string, expected instanceMetadata) error {
	if _, err := safeName(name); err != nil {
		return err
	}
	store, found, err := managedInstancesDirectory(false)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("instance not found: %s", name)
	}
	registryLock, err := acquireDirectoryLock(store, false)
	if err != nil {
		return fmt.Errorf("lock managed instance store: %w", err)
	}
	defer func() {
		if registryLock != nil {
			_ = registryLock.Close()
		}
	}()
	root := filepath.Join(store, name)
	lock, err := acquireDirectoryLock(root, true)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return fmt.Errorf("instance %s is running", name)
	}
	if err != nil {
		return err
	}
	defer lock.Close()
	current, err := readInstanceMetadata(root)
	if err != nil {
		return err
	}
	if current != expected {
		return fmt.Errorf("instance %s changed since confirmation; retry deletion", name)
	}
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return err
	}
	tombstone := filepath.Join(store, deletionTombstonePrefix+name+"-"+hex.EncodeToString(random))
	if err := os.Rename(root, tombstone); err != nil {
		return err
	}
	if err := registryLock.Close(); err != nil {
		return err
	}
	registryLock = nil
	if err := removeInstanceTombstone(tombstone); err != nil {
		return fmt.Errorf("remove instance tombstone %s: %w", tombstone, err)
	}
	return nil
}

func deleteInstance(name string, yes, interactive bool, input io.Reader, output, prompt io.Writer) error {
	if _, err := safeName(name); err != nil {
		return err
	}
	store, found, err := managedInstancesDirectory(false)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("instance not found: %s", name)
	}
	record, err := inspectManagedInstance(store, name)
	if errors.Is(err, errManagedInstanceNotFound) {
		removed, cleanupErr := retryPendingInstanceDeletion(store, name)
		if cleanupErr != nil {
			return cleanupErr
		}
		if removed > 0 {
			_, writeErr := fmt.Fprintf(output, "Deleted pending instance data for %q.\n", name)
			return writeErr
		}
	}
	if err != nil {
		return err
	}
	if record.Status == "running" {
		return fmt.Errorf("instance %s is running", name)
	}
	confirmed, err := confirmedDeletion(name, record, yes, interactive, input, prompt)
	if err != nil {
		return err
	}
	if !confirmed {
		_, err := fmt.Fprintln(output, "Not deleted.")
		return err
	}
	if err := deleteManagedInstance(name, record.metadata); err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "Deleted instance %q.\n", name)
	return err
}
