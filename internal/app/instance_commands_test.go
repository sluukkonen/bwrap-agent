package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestInstanceListJSONFieldsAndOrdering(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	created := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	recent := created.Add(time.Hour)
	projects := map[string]string{}
	states := map[string]string{}
	for _, name := range []string{"older", "beta", "alpha"} {
		opts := managedTestOptions(t.TempDir())
		opts.Instance = name
		identity, err := resolveTestInstance(t, opts)
		if err != nil {
			t.Fatal(err)
		}
		lastUsed := recent
		if name == "older" {
			lastUsed = created
		}
		metadata := instanceMetadata{
			Version: instanceMetadataVersion, Name: name, Project: identity.Project,
			CreatedAt: created, LastUsedAt: lastUsed,
		}
		if err := writeInstanceMetadata(identity.Root, metadata); err != nil {
			t.Fatal(err)
		}
		projects[name], states[name] = identity.Project, identity.State
	}
	var output bytes.Buffer
	if err := listInstances(true, &output); err != nil {
		t.Fatal(err)
	}
	var records []map[string]any
	if err := json.Unmarshal(output.Bytes(), &records); err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 {
		t.Fatalf("records = %#v", records)
	}
	for i, name := range []string{"alpha", "beta", "older"} {
		record := records[i]
		usage, ok := record["disk_usage_bytes"].(float64)
		if !ok || usage <= 0 {
			t.Fatalf("missing or invalid disk usage: %#v", record)
		}
		lastUsed := recent
		if name == "older" {
			lastUsed = created
		}
		want := map[string]any{
			"name": name, "project": projects[name], "status": "stopped",
			"disk_usage_bytes": usage, "created_at": created.Format(time.RFC3339),
			"last_used_at": lastUsed.Format(time.RFC3339), "state_path": states[name],
		}
		if !reflect.DeepEqual(record, want) {
			t.Fatalf("record %d = %#v, want %#v", i, record, want)
		}
	}
}

type instanceFailWriter struct{ err error }

func (writer instanceFailWriter) Write([]byte) (int, error) { return 0, writer.err }

func TestInstanceListOutputErrors(t *testing.T) {
	failure := errors.New("output unavailable")
	for _, test := range []struct {
		name    string
		asJSON  bool
		records []instanceRecord
	}{
		{name: "empty text"},
		{name: "empty JSON", asJSON: true},
		{name: "table", records: []instanceRecord{{Name: "example"}}},
		{name: "JSON", asJSON: true, records: []instanceRecord{{Name: "example"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := writeInstanceList(test.records, test.asJSON, instanceFailWriter{failure}); !errors.Is(err, failure) {
				t.Fatalf("output error = %v", err)
			}
		})
	}
}

type instanceConfirmationReader struct{ beforeRead func() }

func (reader *instanceConfirmationReader) Read(buffer []byte) (int, error) {
	if reader.beforeRead == nil {
		return 0, io.EOF
	}
	reader.beforeRead()
	reader.beforeRead = nil
	return copy(buffer, "yes\n"), nil
}

func TestInstanceDeleteRevalidatesAfterConfirmation(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	identity, err := resolveTestInstance(t, managedTestOptions(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	input := &instanceConfirmationReader{beforeRead: func() {
		// Confirmation must hold neither the registry nor the instance lock.
		registry, err := acquireDirectoryLock(filepath.Dir(identity.Root), true)
		if err != nil {
			t.Fatalf("registry locked during confirmation: %v", err)
		}
		defer registry.Close()
		lock, err := acquireInstanceLock(identity)
		if err != nil {
			t.Fatalf("instance locked during confirmation: %v", err)
		}
		defer lock.Close()
		metadata, err := readInstanceMetadata(identity.Root)
		if err != nil {
			t.Fatal(err)
		}
		metadata.LastUsedAt = metadata.LastUsedAt.Add(time.Second)
		if err := writeInstanceMetadata(identity.Root, metadata); err != nil {
			t.Fatal(err)
		}
	}}
	var output, prompt bytes.Buffer
	err = deleteInstance(identity.Instance, false, true, input, &output, &prompt)
	if err == nil || !strings.Contains(err.Error(), "changed since confirmation") {
		t.Fatalf("deletion after metadata change = %v", err)
	}
	if output.Len() != 0 || !strings.Contains(prompt.String(), "[y/N]") {
		t.Fatalf("unexpected output %q or prompt %q", output.String(), prompt.String())
	}
	if _, err := os.Stat(identity.State); err != nil {
		t.Fatalf("changed instance was removed: %v", err)
	}
}
