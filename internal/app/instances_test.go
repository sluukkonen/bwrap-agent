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

	first, err := resolveInstance(managedTestOptions(projectA))
	if err != nil {
		t.Fatal(err)
	}
	again, err := resolveInstance(managedTestOptions(projectA))
	if err != nil {
		t.Fatal(err)
	}
	if first.Instance != "service" || again.Root != first.Root || !first.Managed {
		t.Fatalf("unexpected reused identity: first=%#v again=%#v", first, again)
	}
	if first.State != filepath.Join(first.Root, "state") || first.LockPath != first.Root {
		t.Fatalf("managed paths are not separated: %#v", first)
	}

	second, err := resolveInstance(managedTestOptions(projectB))
	if err != nil {
		t.Fatal(err)
	}
	if second.Instance != collisionName(second.Project) || second.Instance == first.Instance {
		t.Fatalf("unexpected collision identity: %#v", second)
	}

	explicit := managedTestOptions(projectB)
	explicit.Instance = first.Instance
	if _, err := resolveInstance(explicit); err == nil || !strings.Contains(err.Error(), "belongs to project") {
		t.Fatalf("explicit cross-project reuse = %v", err)
	}
}

func TestCustomStateIsUnmanaged(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("BWRAP_AGENT_STATE_HOME", stateHome)
	state := filepath.Join(t.TempDir(), "custom")
	opts := managedTestOptions(t.TempDir())
	opts.StateDir = state
	opts.Instance = "display-label"
	identity, err := resolveInstance(opts)
	if err != nil {
		t.Fatal(err)
	}
	if identity.Managed || identity.Instance != "display-label" || identity.State != state || identity.LockPath != state {
		t.Fatalf("unexpected custom-state identity: %#v", identity)
	}
	records, err := managedInstanceRecords()
	if err != nil || len(records) != 0 {
		t.Fatalf("custom state appeared in registry: %#v, %v", records, err)
	}
}

func TestManagedInstanceMetadataAndLastUse(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("BWRAP_AGENT_STATE_HOME", stateHome)
	identity, err := resolveInstance(managedTestOptions(t.TempDir()))
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

	identity, err := resolveInstance(managedTestOptions(t.TempDir()))
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

func TestInstanceDeleteConfirmationAndSafety(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	identity, err := resolveInstance(managedTestOptions(t.TempDir()))
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
	identity, err := resolveInstance(managedTestOptions(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	lock, err := acquireInstanceLock(identity)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := deleteInstance(identity.Instance, true, false, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "running") {
		t.Fatalf("running deletion = %v", err)
	}
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
}

func TestCorruptManagedMetadataFailsClearly(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	identity, err := resolveInstance(managedTestOptions(t.TempDir()))
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
