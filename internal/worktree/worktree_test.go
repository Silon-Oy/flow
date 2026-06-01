package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// makeRepo builds a tiny repo with one commit on main and returns its path.
func makeRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	gitRun(t, "", "init", "-q", dir)
	gitRun(t, dir, "config", "user.email", "t@t.t")
	gitRun(t, dir, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("v0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "f.txt")
	gitRun(t, dir, "commit", "-qm", "init")
	gitRun(t, dir, "branch", "-M", "main")
	return dir
}

// TestCreateAndCleanup: a worktree is created on a new branch off local HEAD
// (no remote), then cleaned up — both the dir and the tracking entry.
func TestCreateAndCleanup(t *testing.T) {
	repo := makeRepo(t)
	runID := "20260602-test-issue-1"
	branch := "auto-run/issue-1"

	wt, err := Create(repo, runID, branch, "", "origin")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt, "f.txt")); err != nil {
		t.Errorf("worktree missing checked-out file: %v", err)
	}

	// The new branch exists and is the worktree's HEAD.
	out, err := exec.Command("git", "-C", wt, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := string(out); got != branch+"\n" {
		t.Errorf("worktree branch = %q, want %q", got, branch)
	}

	// The worktree appears in `git worktree list`.
	wl, _ := exec.Command("git", "-C", repo, "worktree", "list").Output()
	if !contains(string(wl), runID) {
		t.Errorf("worktree not listed: %s", wl)
	}

	if err := Cleanup(wt); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Errorf("worktree dir still present after cleanup")
	}
}

// TestFetchNoRemote: fetching a missing remote is a benign no-op.
func TestFetchNoRemote(t *testing.T) {
	repo := makeRepo(t)
	if got := Fetch(repo, "nonexistent"); got != FetchNoRemote {
		t.Errorf("Fetch(missing remote) = %v, want FetchNoRemote", got)
	}
}

// TestCreateMissingBaseBranch: an explicit base branch that does not resolve is
// a clear error, not a cryptic git failure.
func TestCreateMissingBaseBranch(t *testing.T) {
	repo := makeRepo(t)
	if _, err := Create(repo, "rid", "br", "no-such-base", "origin"); err == nil {
		t.Errorf("Create with missing base branch should error")
	}
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
