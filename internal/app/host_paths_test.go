package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func testHostContext(t *testing.T) (string, hostContext) {
	t.Helper()
	root := t.TempDir()
	project := filepath.Join(root, "project")
	state := filepath.Join(root, "state")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := secureMkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	return root, hostContext{state: state, hostEnv: os.Environ(), sources: newHostSourcePolicy(instanceIdentity{Project: project, State: state})}
}

func TestHostSourcePolicySourceTypesAndMissingPaths(t *testing.T) {
	root, host := testHostContext(t)
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, "directory")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	directoryLink := filepath.Join(root, "directory-link")
	if err := os.Symlink(directory, directoryLink); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(root, "missing")
	dangling := filepath.Join(root, "dangling")
	if err := os.Symlink(missing, dangling); err != nil {
		t.Fatal(err)
	}
	loop := filepath.Join(root, "loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(root, "fifo")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", root)
	t.Chdir(root)
	for _, tc := range []struct {
		name, path, want               string
		directory, required, wantError bool
	}{
		{name: "file", path: file, want: file},
		{name: "directory", path: directory, want: directory, directory: true},
		{name: "file symlink", path: link, want: file},
		{name: "directory symlink", path: directoryLink, want: directory, directory: true},
		{name: "relative", path: "file", want: file},
		{name: "home", path: "~/file", want: file},
		{name: "optional missing", path: missing},
		{name: "optional missing ancestor", path: filepath.Join(missing, "file")},
		{name: "required missing", path: missing, required: true, wantError: true},
		{name: "optional dangling", path: dangling, wantError: true},
		{name: "required dangling", path: dangling, required: true, wantError: true},
		{name: "symlink loop", path: loop, wantError: true},
		{name: "directory as file", path: directory, wantError: true},
		{name: "file as directory", path: file, directory: true, wantError: true},
		{name: "fifo", path: fifo, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, found, err := host.sources.resolveSource(tc.path, tc.directory, tc.required)
			if (err != nil) != tc.wantError {
				t.Fatalf("resolveSource = %q, %v, %v", got, found, err)
			}
			if tc.wantError {
				return
			}
			if got != tc.want || found != (tc.want != "") {
				t.Fatalf("resolveSource = %q, %v; want %q", got, found, tc.want)
			}
		})
	}
	if _, err := host.sources.resolvePath("file"); err == nil {
		t.Fatal("accepted relative path in absolute-path resolver")
	}
	if _, err := host.sources.resolvePath(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing path error = %v", err)
	}
}

func TestHostSourcePolicyProtectedRoots(t *testing.T) {
	for _, category := range []string{"project", "state", "git", "bind"} {
		for _, relation := range []string{"inside", "equal", "ancestor", "lexical symlink", "resolved target", "intermediate hop", "hop before dotdot", "sibling"} {
			t.Run(category+"/"+relation, func(t *testing.T) {
				root := t.TempDir()
				identity := instanceIdentity{
					Project:   filepath.Join(root, "project", "protected"),
					State:     filepath.Join(root, "state", "protected"),
					GitCommon: filepath.Join(root, "git", "protected"),
					RWBind:    []string{filepath.Join(root, "bind", "protected")},
				}
				protected := filepath.Join(root, category, "protected")
				if err := os.MkdirAll(protected, 0700); err != nil {
					t.Fatal(err)
				}
				outside := filepath.Join(root, "outside")
				if err := os.MkdirAll(filepath.Join(outside, "discarded"), 0700); err != nil {
					t.Fatal(err)
				}
				file := filepath.Join(outside, "file")
				if err := os.WriteFile(file, nil, 0600); err != nil {
					t.Fatal(err)
				}
				link := func(target, path string) {
					t.Helper()
					if err := os.Symlink(target, path); err != nil {
						t.Fatal(err)
					}
				}
				path := filepath.Join(protected, "file")
				directory := false
				switch relation {
				case "inside":
					if err := os.WriteFile(path, nil, 0600); err != nil {
						t.Fatal(err)
					}
				case "equal":
					path, directory = protected, true
				case "ancestor":
					path, directory = filepath.Dir(protected), true
				case "lexical symlink":
					link(file, path)
				case "resolved target":
					if err := os.WriteFile(path, nil, 0600); err != nil {
						t.Fatal(err)
					}
					link(path, filepath.Join(root, "source"))
					path = filepath.Join(root, "source")
				case "intermediate hop":
					hop := filepath.Join(protected, "hop")
					link(file, hop)
					path = filepath.Join(root, "source")
					link(hop, path)
				case "hop before dotdot":
					hop := filepath.Join(protected, "hop")
					link(filepath.Join(outside, "discarded"), hop)
					path = filepath.Join(root, "source")
					link(hop+"/../file", path)
				case "sibling":
					path, directory = protected+"-sibling", true
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
				}
				policy := newHostSourcePolicy(identity)
				// Both typed resources and clipboard sockets use these overlap checks.
				_, pathErr := policy.resolvePath(path)
				_, _, sourceErr := policy.resolveSource(path, directory, true)
				for _, err := range []error{pathErr, sourceErr} {
					if relation == "sibling" {
						if err != nil {
							t.Fatalf("rejected sibling: %v", err)
						}
					} else if err == nil || !strings.Contains(err.Error(), "overlaps") || !strings.Contains(err.Error(), protected) {
						t.Fatalf("expected overlap with %s, got %v", protected, err)
					}
				}
			})
		}
	}
}
