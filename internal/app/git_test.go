package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runGit(t *testing.T, arguments ...string) {
	t.Helper()
	if output, err := exec.Command("git", arguments...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", arguments, err, output)
	}
}

func initializeGitRepository(t *testing.T, path string) {
	t.Helper()
	runGit(t, "init", "-q", path)
	runGit(t, "-C", path, "config", "user.name", "Test User")
	runGit(t, "-C", path, "config", "user.email", "test@example.invalid")
	if err := os.WriteFile(filepath.Join(path, "tracked"), []byte("content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, "-C", path, "add", "tracked")
	runGit(t, "-C", path, "commit", "-qm", "initial")
}

func TestExternalGitDirectoryRejectsUnrelatedRepository(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	unrelated := filepath.Join(root, "private")
	if err := os.Mkdir(project, 0o700); err != nil {
		t.Fatal(err)
	}
	initializeGitRepository(t, unrelated)
	if err := os.WriteFile(filepath.Join(project, ".git"), []byte("gitdir: "+filepath.Join(unrelated, ".git")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := validatedExternalGitCommonDir(project); err == nil || !strings.Contains(err.Error(), "not associated") {
		t.Fatalf("unrelated Git directory = %v", err)
	}
	t.Setenv("BWRAP_AGENT_STATE_HOME", filepath.Join(root, "state-home"))
	if _, err := BuildPlan(managedTestOptions(project)); err == nil || !strings.Contains(err.Error(), "unsafe project Git metadata") {
		t.Fatalf("launch with unrelated Git directory = %v", err)
	}
}

func TestExternalGitDirectoryRejectsMismatchedWorktreeBacklink(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	worktree := filepath.Join(root, "worktree")
	initializeGitRepository(t, repository)
	runGit(t, "-C", repository, "worktree", "add", "-q", "--detach", worktree, "HEAD")
	gitPointer, err := os.ReadFile(filepath.Join(worktree, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	gitDir := strings.TrimSpace(strings.TrimPrefix(string(gitPointer), "gitdir: "))
	if err := os.WriteFile(filepath.Join(gitDir, "gitdir"), []byte(filepath.Join(root, "other", ".git")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := validatedExternalGitCommonDir(worktree); err == nil || !strings.Contains(err.Error(), "backlink") {
		t.Fatalf("mismatched worktree backlink = %v", err)
	}
}

func TestManagedStoreRejectsExternalGitOverlap(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	worktree := filepath.Join(root, "worktree")
	initializeGitRepository(t, repository)
	runGit(t, "-C", repository, "worktree", "add", "-q", "--detach", worktree, "HEAD")
	t.Setenv("BWRAP_AGENT_STATE_HOME", filepath.Join(repository, ".git"))
	if _, _, err := resolveAndLockInstance(managedTestOptions(worktree)); err == nil || !strings.Contains(err.Error(), "overlaps external Git metadata") {
		t.Fatalf("Git-overlapping store = %v", err)
	}
}

func TestExternalGitDirectoryAcceptsSubmodule(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	submoduleSource := filepath.Join(root, "submodule-source")
	initializeGitRepository(t, repository)
	initializeGitRepository(t, submoduleSource)
	runGit(t, "-C", repository, "-c", "protocol.file.allow=always", "submodule", "add", "-q", submoduleSource, "module")
	project := filepath.Join(repository, "module")
	want := filepath.Join(repository, ".git", "modules", "module")
	common, err := validatedExternalGitCommonDir(project)
	if err != nil || common != want {
		t.Fatalf("submodule common directory = %q, %v; want %q", common, err, want)
	}
	t.Setenv("BWRAP_AGENT_STATE_HOME", filepath.Join(root, "state-home"))
	plan, err := BuildPlan(managedTestOptions(project))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(plan.Bwrap, "\x00"), "--bind\x00"+want+"\x00"+want) {
		t.Fatalf("submodule Git directory was not mounted: %#v", plan.Bwrap)
	}
}

func TestExternalGitDirectoryAcceptsSeparateGitDir(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	gitDir := filepath.Join(root, "git-data")
	runGit(t, "init", "-q", "--separate-git-dir", gitDir, project)
	// A separate gitdir has no backlink by default. Requiring core.worktree
	// makes the association explicit and independently verifiable.
	runGit(t, "--git-dir", gitDir, "config", "core.worktree", project)
	common, err := validatedExternalGitCommonDir(project)
	if err != nil || common != gitDir {
		t.Fatalf("separate Git directory = %q, %v; want %q", common, err, gitDir)
	}
}
