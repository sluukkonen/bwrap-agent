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
	base, err := stateBase()
	if err != nil {
		return "", false, err
	}
	directory, err := resolveState(filepath.Join(base, "instances"))
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

func readInstanceMetadata(root string) (instanceMetadata, error) {
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return instanceMetadata{}, err
	}
	defer unix.Close(rootFD)
	fd, err := unix.Openat(rootFD, instanceMetadataName, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
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

func resolveManagedInstance(project, requested string) (instanceIdentity, error) {
	store, _, err := managedInstancesDirectory(true)
	if err != nil {
		return instanceIdentity{}, err
	}
	registryLock, err := acquireDirectoryLock(store, false)
	if err != nil {
		return instanceIdentity{}, fmt.Errorf("lock managed instance store: %w", err)
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
		return instanceIdentity{}, err
	}
	metadata, found, err := existingManagedInstance(store, name)
	if err != nil {
		return instanceIdentity{}, err
	}
	if found && metadata.Project != project {
		if requested != "" {
			return instanceIdentity{}, fmt.Errorf("instance %s belongs to project %s", name, metadata.Project)
		}
		name = collisionName(project)
		metadata, found, err = existingManagedInstance(store, name)
		if err != nil {
			return instanceIdentity{}, err
		}
		if found && metadata.Project != project {
			return instanceIdentity{}, fmt.Errorf("instance name collision for %s; use --instance", name)
		}
	}
	if !found {
		if _, err := createManagedInstance(store, name, project); err != nil {
			return instanceIdentity{}, fmt.Errorf("create instance %s: %w", name, err)
		}
	}
	root := filepath.Join(store, name)
	return instanceIdentity{Project: project, Instance: name, State: filepath.Join(root, "state"), LockPath: root, Root: root, Managed: true}, nil
}

func markInstanceUsed(identity instanceIdentity) error {
	if !identity.Managed {
		return nil
	}
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
	seen := map[fileIdentity]bool{}
	var total uint64
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := os.Lstat(path)
		if err != nil {
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
		return instanceRecord{}, fmt.Errorf("instance not found: %s", name)
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
	entries, err := os.ReadDir(store)
	closeErr := registryLock.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	records := make([]instanceRecord, 0, len(entries))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".deleting-") {
			continue
		}
		if !entry.IsDir() {
			return nil, fmt.Errorf("invalid entry in managed instance store: %s", entry.Name())
		}
		if _, err := safeName(entry.Name()); err != nil {
			return nil, fmt.Errorf("invalid entry in managed instance store: %s", entry.Name())
		}
		record, err := inspectManagedInstance(store, entry.Name())
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
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
	tombstone := filepath.Join(store, ".deleting-"+name+"-"+hex.EncodeToString(random))
	if err := os.Rename(root, tombstone); err != nil {
		return err
	}
	if err := registryLock.Close(); err != nil {
		return err
	}
	registryLock = nil
	if err := os.RemoveAll(tombstone); err != nil {
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
