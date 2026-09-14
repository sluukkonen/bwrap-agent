package app

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func trustTestConfig(t *testing.T, project string) {
	t.Helper()
	if err := setProjectConfigTrust(project, true, io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestProjectConfigTrustLifecycle(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	project := t.TempDir()
	parse := func(extra ...string) (Options, error) {
		args := append([]string{"run", "--project", project}, extra...)
		args = append(args, "/bin/true")
		opts, _, err := parseOptions(args, io.Discard, io.Discard)
		return opts, err
	}
	if _, err := parse(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(project, projectConfigName)
	writeTestFile(t, path, "network = \"host\"\n")
	for _, extra := range [][]string{nil, {"--dry-run"}, {"--allow-control-file-writes"}, {"--workspace-mode", "read-only"}, {"--workspace-mode", "copy-on-write"}} {
		if _, err := parse(extra...); err == nil || !strings.Contains(err.Error(), "config trust") {
			t.Fatalf("unapproved %v: %v", extra, err)
		}
	}
	for _, flag := range []string{"--no-config", "--no-project-config"} {
		if opts, err := parse(flag); err != nil || opts.Network == "host" {
			t.Fatalf("ignore %s: %v %v", flag, opts, err)
		}
	}
	trustTestConfig(t, project)
	if opts, err := parse(); err != nil || opts.Network != "host" {
		t.Fatalf("approved: %v %v", opts, err)
	}
	writeTestFile(t, path, "network = \"host\"\n# changed\n")
	if _, err := parse(); err == nil {
		t.Fatal("accepted changed comments")
	}
	trustTestConfig(t, project)
	if _, err := parse(); err != nil {
		t.Fatal(err)
	}
	if err := setProjectConfigTrust(project, false, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := parse(); err == nil {
		t.Fatal("accepted revoked configuration")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := parse(); err != nil {
		t.Fatal(err)
	}
	if err := setProjectConfigTrust(project, false, io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestProjectConfigTrustIsBoundToCanonicalProject(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	project := t.TempDir()
	writeTestFile(t, filepath.Join(project, projectConfigName), "network = \"host\"")
	trustTestConfig(t, project)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(project, alias); err != nil {
		t.Fatal(err)
	}
	trustTestConfig(t, alias)
	store, err := projectTrustStore(project, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(store)
	if err != nil || len(entries) != 1 {
		t.Fatalf("duplicate canonical approval: %v %v", entries, err)
	}
	moved := filepath.Join(t.TempDir(), "moved")
	if err := os.Rename(project, moved); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadTrustedProjectConfig(moved); err == nil {
		t.Fatal("approved moved project")
	}
}

func TestProjectConfigTrustRejectsUnsafeFiles(t *testing.T) {
	for _, kind := range []string{"symlink", "fifo", "directory", "oversized", "invalid"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
			project := t.TempDir()
			path := filepath.Join(project, projectConfigName)
			var err error
			switch kind {
			case "symlink":
				target := filepath.Join(t.TempDir(), "config")
				writeTestFile(t, target, "")
				err = os.Symlink(target, path)
			case "fifo":
				err = unix.Mkfifo(path, 0o600)
			case "directory":
				err = os.Mkdir(path, 0o700)
			case "oversized":
				err = os.WriteFile(path, bytes.Repeat([]byte{' '}, maxProjectConfigSize+1), 0o600)
			case "invalid":
				err = os.WriteFile(path, []byte("unknown = true"), 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := setProjectConfigTrust(project, true, io.Discard); err == nil {
				t.Fatal("approved unsafe file")
			}
			if _, _, err := loadTrustedProjectConfig(project); err == nil {
				t.Fatal("loaded unsafe file")
			}
		})
	}
}

func TestProjectTrustStoreIsolation(t *testing.T) {
	for _, kind := range []string{"project", "rw-bind", "ancestor", "external-git", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			base := t.TempDir()
			t.Setenv("BWRAP_AGENT_STATE_HOME", base)
			project := t.TempDir()
			store := filepath.Join(base, "trusted-projects")
			if err := os.Mkdir(store, 0o700); err != nil {
				t.Fatal(err)
			}
			var rw []string
			var git string
			switch kind {
			case "project":
				project = store
			case "rw-bind":
				rw = []string{store}
			case "ancestor":
				rw = []string{base}
			case "external-git":
				git = store
			case "symlink":
				if err := os.Remove(store); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(project, store); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := projectTrustStore(project, git, rw); err == nil {
				t.Fatal("accepted writable trust store")
			}
		})
	}
}

func TestLaunchRejectsTrustStoreBindWithoutProjectConfig(t *testing.T) {
	base := t.TempDir()
	t.Setenv("BWRAP_AGENT_STATE_HOME", base)
	store := filepath.Join(base, "trusted-projects")
	if err := os.Mkdir(store, 0o700); err != nil {
		t.Fatal(err)
	}
	opts := controlTestOptions(t.TempDir(), "trust-store-bind")
	opts.RWBind = []string{store}
	if _, err := BuildPlan(opts); err == nil || !strings.Contains(err.Error(), "trust store") {
		t.Fatalf("unsafe launch: %v", err)
	}
}

func TestProjectTrustStoreRejectsManagedInstanceStorage(t *testing.T) {
	for _, kind := range []string{"current", "other", "missing", "relocated-instances", "intermediate-alias", "ancestor"} {
		t.Run(kind, func(t *testing.T) {
			base := t.TempDir()
			t.Setenv("BWRAP_AGENT_STATE_HOME", base)
			project := t.TempDir()
			opts := controlTestOptions(project, "current")
			instances := filepath.Join(base, "instances")
			if kind == "relocated-instances" {
				relocated := t.TempDir()
				if err := os.Symlink(relocated, instances); err != nil {
					t.Fatal(err)
				}
				instances = relocated
			}
			name := "current"
			if kind == "other" {
				name = "other"
			}
			state := filepath.Join(instances, name, "state")
			if err := os.MkdirAll(state, 0o700); err != nil {
				t.Fatal(err)
			}
			target := state
			switch kind {
			case "missing":
				target = filepath.Join(state, "not-created", "approvals")
			case "intermediate-alias":
				target = filepath.Join(state, "alias")
				if err := os.Symlink(t.TempDir(), target); err != nil {
					t.Fatal(err)
				}
			case "ancestor":
				target = base
			}
			if err := os.Symlink(target, filepath.Join(base, "trusted-projects")); err != nil {
				t.Fatal(err)
			}
			check := func(err error) {
				t.Helper()
				if err == nil || !strings.Contains(err.Error(), "unsafe configuration trust store") {
					t.Fatalf("expected trust-store isolation error, got %v", err)
				}
			}
			_, err := projectTrustStore(project, "", nil)
			check(err)
			_, _, err = loadTrustedProjectConfig(project)
			check(err)
			check(setProjectConfigTrust(project, true, io.Discard))
			check(setProjectConfigTrust(project, false, io.Discard))
			// Launch validation must apply even without a project config file.
			_, err = BuildPlan(opts)
			check(err)
		})
	}
}

func TestProjectTrustStoreAllowsSharedStateBaseAlias(t *testing.T) {
	base := t.TempDir()
	alias := filepath.Join(t.TempDir(), "state-home")
	if err := os.Symlink(base, alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BWRAP_AGENT_STATE_HOME", alias)
	project := t.TempDir()
	writeTestFile(t, filepath.Join(project, projectConfigName), "")
	trustTestConfig(t, project)
	if _, found, err := loadTrustedProjectConfig(project); err != nil || !found {
		t.Fatalf("safe sibling stores rejected: found=%v err=%v", found, err)
	}
}

func TestProjectTrustRecordsFailClosed(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	project := t.TempDir()
	writeTestFile(t, filepath.Join(project, projectConfigName), "")
	trustTestConfig(t, project)
	store, err := projectTrustStore(project, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store, configDigest(project)+".json")
	wrongProject, _ := json.Marshal(projectConfigTrust{Project: "elsewhere", SHA256: configDigest("")})
	for _, content := range []string{"{", "{}", string(wrongProject)} {
		writeTestFile(t, path, content)
		if _, _, err := loadTrustedProjectConfig(project); err == nil {
			t.Fatalf("accepted record %q", content)
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "record")
	writeTestFile(t, target, "{}")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadTrustedProjectConfig(project); err == nil {
		t.Fatal("followed record symlink")
	}
	trustTestConfig(t, project) // Atomically replaces the symlink, never its target.
	if content, err := os.ReadFile(target); err != nil || string(content) != "{}" {
		t.Fatalf("changed symlink target: %q %v", content, err)
	}
	for _, entry := range []struct {
		path string
		mode os.FileMode
	}{{store, 0o700}, {path, 0o600}} {
		info, err := os.Stat(entry.path)
		if err != nil || info.Mode().Perm() != entry.mode {
			t.Fatalf("unsafe permissions: %v %v", info, err)
		}
	}
}

func TestConfigTrustCLI(t *testing.T) {
	for _, command := range []string{"trust", "untrust"} {
		parsed, code, err := parseCLI([]string{"config", command, "--project", "/tmp/project"}, io.Discard, io.Discard)
		if err != nil || code != 0 || parsed.ConfigTrust.Project != "/tmp/project" {
			t.Fatalf("parse %s: %+v %d %v", command, parsed, code, err)
		}
	}
}

func TestProjectConfigReadsOnlyApprovedBytesDuringReplacement(t *testing.T) {
	t.Setenv("BWRAP_AGENT_STATE_HOME", t.TempDir())
	project := t.TempDir()
	path := filepath.Join(project, projectConfigName)
	const approved = "network = \"none\"\n"
	const unapproved = "network = \"host\"\n"
	writeTestFile(t, path, approved)
	trustTestConfig(t, project)
	done := make(chan struct{})
	stopped := make(chan error, 1)
	go func() {
		for {
			for _, content := range []string{unapproved, approved} {
				select {
				case <-done:
					stopped <- nil
					return
				default:
				}
				temporary := filepath.Join(project, "replacement")
				if err := os.WriteFile(temporary, []byte(content), 0o600); err != nil {
					stopped <- err
					return
				}
				if err := os.Rename(temporary, path); err != nil {
					stopped <- err
					return
				}
			}
		}
	}()
	defer func() {
		close(done)
		if err := <-stopped; err != nil {
			t.Error(err)
		}
	}()
	for i := 0; i < 300; i++ {
		layer, found, err := loadTrustedProjectConfig(project)
		if err != nil {
			if !strings.Contains(err.Error(), "unapproved") {
				t.Fatal(err)
			}
			continue
		}
		if !found || layer.network == nil || *layer.network != "none" {
			t.Fatalf("loaded bytes that were not approved: %+v", layer)
		}
	}
}

func TestRegularFileReadersRejectFIFO(t *testing.T) {
	for _, reader := range []struct {
		name string
		read func(string, string) error
	}{
		{"bounded", func(root, name string) error {
			_, err := readSmallRegularFile(filepath.Join(root, name), 1024)
			return err
		}},
		{"user-config", func(root, name string) error { _, _, err := loadConfigFile(filepath.Join(root, name)); return err }},
		{"injected", func(root, name string) error {
			file, err := openInjectedFile(filepath.Join(root, name))
			if file != nil {
				file.Close()
			}
			return err
		}},
		{"instance-metadata", func(root, name string) error { _, err := readInstanceMetadata(root); return err }},
		{"agent-state", func(root, name string) error { _, err := stateRegularFileExists(root, name); return err }},
	} {
		t.Run(reader.name, func(t *testing.T) {
			root := t.TempDir()
			if err := unix.Mkfifo(filepath.Join(root, instanceMetadataName), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := reader.read(root, instanceMetadataName); err == nil {
				t.Fatal("accepted FIFO")
			}
		})
	}
}
