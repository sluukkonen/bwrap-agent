package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
