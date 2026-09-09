package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func managedTestOptions(project string) Options {
	return Options{Project: project, Network: "host", Podman: "off", TTY: "never", Command: []string{"/bin/true"}}
}

func resolveTestInstance(t *testing.T, opts Options) (instanceIdentity, error) {
	t.Helper()
	identity, lock, err := resolveAndLockInstance(opts)
	if err != nil {
		return instanceIdentity{}, err
	}
	if err := lock.Close(); err != nil {
		return instanceIdentity{}, err
	}
	return identity, nil
}

func TestManagedInstanceResolutionAndCollisionNames(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	parentA := t.TempDir()
	parentB := t.TempDir()
	projectA := filepath.Join(parentA, "service")
	projectB := filepath.Join(parentB, "service")
	for _, project := range []string{projectA, projectB} {
		if err := os.Mkdir(project, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	first, err := resolveTestInstance(t, managedTestOptions(projectA))
	if err != nil {
		t.Fatal(err)
	}
	again, err := resolveTestInstance(t, managedTestOptions(projectA))
	if err != nil {
		t.Fatal(err)
	}
	if first.Instance != "service" || again.Root != first.Root {
		t.Fatalf("unexpected reused identity: first=%#v again=%#v", first, again)
	}
	if first.State != filepath.Join(first.Root, "state") {
		t.Fatalf("managed paths are not separated: %#v", first)
	}

	second, err := resolveTestInstance(t, managedTestOptions(projectB))
	if err != nil {
		t.Fatal(err)
	}
	if second.Instance != collisionName(second.Project) || second.Instance == first.Instance {
		t.Fatalf("unexpected collision identity: %#v", second)
	}

	explicit := managedTestOptions(projectB)
	explicit.Instance = first.Instance
	if _, err := resolveTestInstance(t, explicit); err == nil || !strings.Contains(err.Error(), "belongs to project") {
		t.Fatalf("explicit cross-project reuse = %v", err)
	}
}

func TestManagedInstanceMetadataAndLastUse(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("BWRAP_AGENT_STATE_HOME", stateHome)
	identity, err := resolveTestInstance(t, managedTestOptions(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(identity.Root, instanceMetadataName))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("metadata mode = %o", info.Mode().Perm())
	}
	before, err := readInstanceMetadata(identity.Root)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	lock, err := acquireInstanceLock(identity)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := markInstanceUsed(identity); err != nil {
		t.Fatal(err)
	}
	after, err := readInstanceMetadata(identity.Root)
	if err != nil {
		t.Fatal(err)
	}
	if !after.LastUsedAt.After(before.LastUsedAt) || after.CreatedAt != before.CreatedAt {
		t.Fatalf("unexpected metadata update: before=%#v after=%#v", before, after)
	}
}

func TestInstanceListHumanJSONStatusAndUsage(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	var output bytes.Buffer
	if err := listInstances(false, &output); err != nil || output.String() != "No instances.\n" {
		t.Fatalf("empty human list = %q, %v", output.String(), err)
	}
	output.Reset()
	if err := listInstances(true, &output); err != nil || strings.TrimSpace(output.String()) != "[]" {
		t.Fatalf("empty JSON list = %q, %v", output.String(), err)
	}

	identity, err := resolveTestInstance(t, managedTestOptions(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(identity.State, "allocated"), bytes.Repeat([]byte{'x'}, 8192), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := acquireInstanceLock(identity)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	output.Reset()
	if err := listInstances(true, &output); err != nil {
		t.Fatal(err)
	}
	var records []instanceRecord
	if err := json.Unmarshal(output.Bytes(), &records); err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Status != "running" || records[0].DiskUsageBytes == 0 || records[0].StatePath != identity.State {
		t.Fatalf("unexpected record: %#v", records)
	}
	output.Reset()
	if err := listInstances(false, &output); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"NAME", "STATUS", "DISK USAGE", "LAST USED", "PROJECT", identity.Instance, "running"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("human list %q does not contain %q", output.String(), expected)
		}
	}
}

func TestInstanceTableEscapesControlCharacters(t *testing.T) {
	if got := tableField("/tmp/normal project"); got != "/tmp/normal project" {
		t.Fatalf("normal field = %q", got)
	}
	if got := tableField("/tmp/project\n\x1b[2J"); got != `"/tmp/project\n\x1b[2J"` {
		t.Fatalf("unsafe field was not escaped: %q", got)
	}
}

func TestDeletionTombstonesAreInternalAndHidden(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	store, _, err := managedInstancesDirectory(true)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(store, deletionTombstonePrefix+"old-0123456789abcdef"), 0o700); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := listInstances(true, &output); err != nil || strings.TrimSpace(output.String()) != "[]" {
		t.Fatalf("tombstone listing = %q, %v", output.String(), err)
	}
	if _, err := safeName(deletionTombstonePrefix + "user"); err == nil {
		t.Fatal("reserved tombstone name unexpectedly accepted")
	}
}

func TestDeletionTombstoneMatchingIsStrict(t *testing.T) {
	for _, entry := range []string{
		".deleting-example-0123456789abcdef",
		".deleting-example-with-dash-0123456789abcdef",
	} {
		name := strings.TrimSuffix(strings.TrimPrefix(entry, deletionTombstonePrefix), "-0123456789abcdef")
		if !deletionTombstoneMatches(name, entry) {
			t.Errorf("valid tombstone %q did not match %q", entry, name)
		}
	}
	for _, entry := range []string{
		".deleting-example-0123456789abcde",
		".deleting-example-0123456789abcdef0",
		".deleting-example-0123456789abcdeg",
		".deleting-example-0123456789ABCDEf",
		".deleting-other-0123456789abcdef",
	} {
		if deletionTombstoneMatches("example", entry) {
			t.Errorf("invalid tombstone %q matched", entry)
		}
	}
	if name, valid := deletionTombstoneInstanceName(".deleting-example-with-dash-0123456789abcdef"); !valid || name != "example-with-dash" {
		t.Fatalf("tombstone instance name = %q, %t", name, valid)
	}
	if _, valid := deletionTombstoneInstanceName(".deleting-example-0123456789abcdeg"); valid {
		t.Fatal("invalid tombstone produced an instance name")
	}
}

func TestRemoveInstanceTombstoneUsesPodmanNamespace(t *testing.T) {
	tombstone := filepath.Join(t.TempDir(), ".deleting-example-0123456789abcdef")
	if err := os.MkdirAll(filepath.Join(tombstone, "state", "podman", "storage"), 0o700); err != nil {
		t.Fatal(err)
	}
	directCalled := false
	podmanCalled := false
	err := removeInstanceTombstoneWith(tombstone, func(path string) error {
		directCalled = true
		return os.RemoveAll(path)
	}, func(path string) error {
		podmanCalled = true
		return os.RemoveAll(filepath.Join(path, "state"))
	})
	if err != nil {
		t.Fatal(err)
	}
	if !podmanCalled || !directCalled {
		t.Fatalf("cleanup calls: podman=%t direct=%t", podmanCalled, directCalled)
	}
}

func TestRemoveInstanceTombstoneFallsBackToDirectRemoval(t *testing.T) {
	tombstone := filepath.Join(t.TempDir(), ".deleting-example-0123456789abcdef")
	if err := os.MkdirAll(filepath.Join(tombstone, "state", "podman", "storage"), 0o700); err != nil {
		t.Fatal(err)
	}
	podmanErr := errors.New("podman cleanup failed")
	err := removeInstanceTombstoneWith(tombstone, os.RemoveAll, func(string) error { return podmanErr })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(tombstone); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tombstone remains after fallback: %v", err)
	}
}

func TestInterruptedCleanupStateStillRequiresPodman(t *testing.T) {
	tombstone := filepath.Join(t.TempDir(), ".deleting-example-0123456789abcdef")
	if err := os.MkdirAll(filepath.Join(tombstone, ".podman-cleanup-interrupted", "storage"), 0o700); err != nil {
		t.Fatal(err)
	}
	needed, err := tombstoneNeedsPodmanCleanup(tombstone)
	if err != nil || !needed {
		t.Fatalf("interrupted cleanup detection = %t, %v", needed, err)
	}
}

func TestPodmanCleanupEnvironmentOwnsRuntimeDirectory(t *testing.T) {
	environment := podmanCleanupEnvironment([]string{
		"PATH=/usr/bin",
		"XDG_RUNTIME_DIR=/unusable",
		"CONTAINER_HOST=tcp://untrusted",
		"CONTAINERS_STORAGE_CONF=/untrusted/storage.conf",
		"STORAGE_DRIVER=overlay",
		"STORAGE_OPTS=overlay.mount_program=/untrusted",
		"PODMAN_NO_PAUSE_PROCESS=0",
	}, "/trusted/runtime", "/trusted/storage.conf", "/trusted/containers.conf")
	joined := strings.Join(environment, "\n")
	if strings.Count(joined, "XDG_RUNTIME_DIR=") != 1 || !strings.Contains(joined, "XDG_RUNTIME_DIR=/trusted/runtime") {
		t.Fatalf("cleanup runtime environment = %#v", environment)
	}
	if strings.Count(joined, "CONTAINERS_STORAGE_CONF=") != 1 || !strings.Contains(joined, "CONTAINERS_STORAGE_CONF=/trusted/storage.conf") {
		t.Fatalf("cleanup storage environment = %#v", environment)
	}
	if strings.Count(joined, "CONTAINERS_CONF=") != 1 || !strings.Contains(joined, "CONTAINERS_CONF=/trusted/containers.conf") {
		t.Fatalf("cleanup containers environment = %#v", environment)
	}
	if strings.Count(joined, "PODMAN_NO_PAUSE_PROCESS=") != 1 || !strings.Contains(joined, "PODMAN_NO_PAUSE_PROCESS=1") {
		t.Fatalf("cleanup pause-process environment = %#v", environment)
	}
	for _, forbidden := range []string{"XDG_RUNTIME_DIR=/unusable", "CONTAINER_HOST=", "CONTAINERS_STORAGE_CONF=/untrusted",
		"STORAGE_DRIVER=", "STORAGE_OPTS=", "PODMAN_NO_PAUSE_PROCESS=0"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("cleanup environment retained %q: %#v", forbidden, environment)
		}
	}
	if !strings.Contains(joined, "PATH=/usr/bin") {
		t.Fatalf("cleanup environment lost ordinary value: %#v", environment)
	}
}

func TestPodmanCleanupStateIsPrivateIsolatedAndTemporary(t *testing.T) {
	for _, test := range []struct {
		name      string
		actionErr error
	}{
		{name: "success"},
		{name: "failure", actionErr: errors.New("cleanup failed")},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			store := filepath.Join(root, "instances")
			if err := os.Mkdir(store, 0o700); err != nil {
				t.Fatal(err)
			}
			tombstone := filepath.Join(store, ".deleting-example-0123456789abcdef")
			if err := os.Mkdir(tombstone, 0o700); err != nil {
				t.Fatal(err)
			}
			staleCleanup := filepath.Join(tombstone, ".podman-cleanup-interrupted")
			if err := os.Mkdir(staleCleanup, 0o700); err != nil {
				t.Fatal(err)
			}
			var cleanup string
			err := withPodmanCleanupState(tombstone, func(runtime, storageConfig, containersConfig string, targets []string) error {
				cleanup = filepath.Dir(runtime)
				info, err := os.Stat(runtime)
				if err != nil {
					return err
				}
				if !info.IsDir() || info.Mode().Perm() != 0o700 {
					return fmt.Errorf("runtime mode = %v", info.Mode())
				}
				content, err := os.ReadFile(storageConfig)
				if err != nil {
					return err
				}
				configuration := string(content)
				for _, expected := range []string{`driver = "vfs"`, filepath.Join(cleanup, "storage"), filepath.Join(cleanup, "runroot")} {
					if !strings.Contains(configuration, expected) {
						return fmt.Errorf("storage configuration %q does not contain %q", configuration, expected)
					}
				}
				if content, err := os.ReadFile(containersConfig); err != nil || !strings.Contains(string(content), `events_logger = "file"`) {
					return fmt.Errorf("containers configuration = %q, %v", content, err)
				}
				if len(targets) != 1 || targets[0] != staleCleanup {
					return fmt.Errorf("cleanup targets = %#v", targets)
				}
				if test.actionErr == nil {
					return os.RemoveAll(staleCleanup)
				}
				return test.actionErr
			})
			if !errors.Is(err, test.actionErr) {
				t.Fatalf("cleanup result = %v, want %v", err, test.actionErr)
			}
			if cleanup == "" {
				t.Fatal("cleanup action did not receive cleanup state")
			}
			if _, err := os.Lstat(cleanup); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("temporary cleanup state remains: %v", err)
			}
			if test.actionErr == nil {
				if _, err := os.Lstat(staleCleanup); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("interrupted cleanup state was not reclaimed: %v", err)
				}
			}
		})
	}
}

func TestStopPodmanPauseProcessVerifiesAndTerminatesKeeper(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "instances")
	tombstone := filepath.Join(store, ".deleting-example-0123456789abcdef")
	pidDirectory := filepath.Join(tombstone, "state", "run", "libpod", "tmp")
	if err := os.MkdirAll(pidDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	oldRoot := filepath.Join(store, "example")
	command := exec.Command("/bin/sleep", "30")
	command.Env = append(os.Environ(),
		"_PODMAN_PAUSE=1",
		"XDG_RUNTIME_DIR="+filepath.Join(oldRoot, "state", "run"),
		"CONTAINERS_STORAGE_CONF="+filepath.Join(oldRoot, "state", "config", "containers", "storage.conf"))
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		if !waited {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()
	if err := os.WriteFile(filepath.Join(pidDirectory, "pause.pid"), []byte(strconv.Itoa(command.Process.Pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := stopPodmanPauseProcess(tombstone); err != nil {
		t.Fatal(err)
	}
	err := command.Wait()
	waited = true
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) {
		t.Fatalf("pause process wait = %v, want signal exit", err)
	}
}

func TestInstanceDeleteRetriesPendingTombstone(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	identity, err := resolveTestInstance(t, managedTestOptions(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	tombstone := filepath.Join(filepath.Dir(identity.Root), deletionTombstonePrefix+identity.Instance+"-0123456789abcdef")
	if err := os.Rename(identity.Root, tombstone); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := deleteInstance(identity.Instance, true, false, strings.NewReader(""), &output, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if output.String() != "Deleted pending instance data for \""+identity.Instance+"\".\n" {
		t.Fatalf("retry output = %q", output.String())
	}
	if _, err := os.Lstat(tombstone); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pending tombstone remains: %v", err)
	}
}

func TestInstanceDeleteConfirmationAndSafety(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	identity, err := resolveTestInstance(t, managedTestOptions(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	var output, prompt bytes.Buffer
	if err := deleteInstance(identity.Instance, false, false, strings.NewReader(""), &output, &prompt); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("noninteractive deletion = %v", err)
	}
	if err := deleteInstance(identity.Instance, false, true, strings.NewReader("no\n"), &output, &prompt); err != nil || output.String() != "Not deleted.\n" {
		t.Fatalf("cancel deletion: output=%q err=%v", output.String(), err)
	}
	if _, err := os.Stat(identity.Root); err != nil {
		t.Fatalf("cancelled instance disappeared: %v", err)
	}

	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(identity.State, "outside")); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := deleteInstance(identity.Instance, true, false, strings.NewReader(""), &output, &prompt); err != nil {
		t.Fatal(err)
	}
	if output.String() != "Deleted instance \""+identity.Instance+"\".\n" {
		t.Fatalf("delete output = %q", output.String())
	}
	if content, err := os.ReadFile(victim); err != nil || string(content) != "safe" {
		t.Fatalf("delete followed state symlink: %q, %v", content, err)
	}
	if _, err := os.Stat(identity.Root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("instance root remains: %v", err)
	}
}

func TestInstanceDeleteRejectsRunningInstance(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	identity, lock, err := resolveAndLockInstance(managedTestOptions(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := deleteInstance(identity.Instance, true, false, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "running") {
		t.Fatalf("running deletion = %v", err)
	}
}

func TestInstanceLockDoesNotRecreateMissingRoot(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	identity, err := resolveTestInstance(t, managedTestOptions(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(identity.Root); err != nil {
		t.Fatal(err)
	}
	if _, err := acquireInstanceLock(identity); err == nil {
		t.Fatal("locking a deleted instance unexpectedly succeeded")
	}
	if _, err := os.Stat(identity.Root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lock attempt recreated deleted instance: %v", err)
	}
}

func TestAllocatedDiskUsageIgnoresDisappearingFiles(t *testing.T) {
	root := t.TempDir()
	disappearing := filepath.Join(root, "disappearing")
	if err := os.WriteFile(disappearing, bytes.Repeat([]byte{'x'}, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "stable"), bytes.Repeat([]byte{'y'}, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	usage, err := allocatedDiskUsageWith(root, func(path string) (os.FileInfo, error) {
		if path == disappearing {
			return nil, os.ErrNotExist
		}
		return os.Lstat(path)
	})
	if err != nil || usage == 0 {
		t.Fatalf("usage with disappearing file = %d, %v", usage, err)
	}
}

func TestIgnorableDiskUsageErrors(t *testing.T) {
	for _, err := range []error{
		os.ErrNotExist,
		os.ErrPermission,
		&fs.PathError{Op: "readdir", Path: "/state/podman/work", Err: syscall.EACCES},
		&fs.PathError{Op: "open", Path: "/state/podman/work", Err: syscall.EPERM},
	} {
		if !ignorableDiskUsageError(err) {
			t.Errorf("error should be ignored while sizing: %v", err)
		}
	}
	if ignorableDiskUsageError(&fs.PathError{Op: "read", Path: "/state/file", Err: syscall.EIO}) {
		t.Fatal("unexpected I/O error was ignored")
	}
}

func TestAllocatedDiskUsageSkipsUnreadableDirectory(t *testing.T) {
	root := t.TempDir()
	restricted := filepath.Join(root, "restricted")
	if err := os.Mkdir(restricted, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(restricted, 0o700)
	if _, err := os.ReadDir(restricted); err == nil {
		t.Skip("current user can bypass mode-000 directory permissions")
	}
	if _, err := allocatedDiskUsage(root); err != nil {
		t.Fatalf("sizing failed on unreadable directory: %v", err)
	}
}

func TestManagedStoreRejectsProtectedPathOverlap(t *testing.T) {
	t.Run("inside project", func(t *testing.T) {
		project := t.TempDir()
		stateHome := filepath.Join(project, "state-home")
		t.Setenv("BWRAP_AGENT_STATE_HOME", stateHome)
		if _, _, err := resolveAndLockInstance(managedTestOptions(project)); err == nil || !strings.Contains(err.Error(), "overlaps project") {
			t.Fatalf("overlapping store = %v", err)
		}
		if _, err := os.Stat(stateHome); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unsafe state home was created: %v", err)
		}
	})

	t.Run("contains project", func(t *testing.T) {
		root := t.TempDir()
		project := filepath.Join(root, "instances", "checkout")
		if err := os.MkdirAll(project, 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("BWRAP_AGENT_STATE_HOME", root)
		if _, _, err := resolveAndLockInstance(managedTestOptions(project)); err == nil || !strings.Contains(err.Error(), "overlaps project") {
			t.Fatalf("containing store = %v", err)
		}
	})

	t.Run("canonical symlink target", func(t *testing.T) {
		root := t.TempDir()
		project := filepath.Join(root, "project")
		if err := os.MkdirAll(filepath.Join(project, "state-home"), 0o700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(root, "state-link")
		if err := os.Symlink(filepath.Join(project, "state-home"), link); err != nil {
			t.Fatal(err)
		}
		t.Setenv("BWRAP_AGENT_STATE_HOME", link)
		if _, _, err := resolveAndLockInstance(managedTestOptions(project)); err == nil || !strings.Contains(err.Error(), "overlaps project") {
			t.Fatalf("symlinked store = %v", err)
		}
	})

	t.Run("project-owned lexical symlink", func(t *testing.T) {
		root := t.TempDir()
		project := filepath.Join(root, "project")
		outside := filepath.Join(root, "outside-state")
		if err := os.MkdirAll(project, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(outside, 0o700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(project, "state-home")
		if err := os.Symlink(outside, link); err != nil {
			t.Fatal(err)
		}
		t.Setenv("BWRAP_AGENT_STATE_HOME", link)
		if _, _, err := resolveAndLockInstance(managedTestOptions(project)); err == nil || !strings.Contains(err.Error(), "overlaps project") {
			t.Fatalf("project-owned state symlink = %v", err)
		}
	})

	t.Run("intermediate symlink expansion", func(t *testing.T) {
		root := t.TempDir()
		project := filepath.Join(root, "project")
		outside := filepath.Join(root, "outside-state")
		if err := os.MkdirAll(project, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(outside, 0o700); err != nil {
			t.Fatal(err)
		}
		alias := filepath.Join(root, "project-alias")
		if err := os.Symlink(project, alias); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(project, "state-home")); err != nil {
			t.Fatal(err)
		}
		t.Setenv("BWRAP_AGENT_STATE_HOME", filepath.Join(alias, "state-home"))
		if _, _, err := resolveAndLockInstance(managedTestOptions(project)); err == nil || !strings.Contains(err.Error(), "overlaps project") {
			t.Fatalf("intermediate state symlink = %v", err)
		}
	})

	t.Run("read-write bind", func(t *testing.T) {
		root := t.TempDir()
		project := filepath.Join(root, "project")
		if err := os.Mkdir(project, 0o700); err != nil {
			t.Fatal(err)
		}
		stateHome := filepath.Join(root, "state-home")
		t.Setenv("BWRAP_AGENT_STATE_HOME", stateHome)
		opts := managedTestOptions(project)
		opts.RWBind = []string{root}
		if _, _, err := resolveAndLockInstance(opts); err == nil || !strings.Contains(err.Error(), "overlaps read-write bind") {
			t.Fatalf("writable-bind overlap = %v", err)
		}
	})
}

func TestInstanceCommandsParse(t *testing.T) {
	var stdout, stderr bytes.Buffer
	parsed, code, err := parseCLI([]string{"instance", "list", "--json"}, &stdout, &stderr)
	if err != nil || code != 0 || parsed.Command != commandInstanceList || !parsed.InstanceList.JSON {
		t.Fatalf("list parse: %#v code=%d err=%v stderr=%q", parsed, code, err, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	parsed, code, err = parseCLI([]string{"instance", "delete", "example", "--yes"}, &stdout, &stderr)
	if err != nil || code != 0 || parsed.Command != commandInstanceDelete || parsed.InstanceDelete.Name != "example" || !parsed.InstanceDelete.Yes {
		t.Fatalf("delete parse: %#v code=%d err=%v stderr=%q", parsed, code, err, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if _, code, err = parseOptions([]string{"run", "--name", "old", "/bin/true"}, &stdout, &stderr); err == nil || code != 2 {
		t.Fatalf("old --name unexpectedly parsed: code=%d err=%v", code, err)
	}
	stdout.Reset()
	stderr.Reset()
	if _, code, err = parseOptions([]string{"run", "--state-dir", "/tmp/state", "/bin/true"}, &stdout, &stderr); err == nil || code != 2 {
		t.Fatalf("removed --state-dir unexpectedly parsed: code=%d err=%v", code, err)
	}
}

func TestCorruptManagedMetadataFailsClearly(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	identity, err := resolveTestInstance(t, managedTestOptions(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(identity.Root, instanceMetadataName), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := managedInstanceRecords(); err == nil || !strings.Contains(err.Error(), "invalid instance") {
		t.Fatalf("corrupt metadata list = %v", err)
	}
}
