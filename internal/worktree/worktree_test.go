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
	// Cleanup is a rollback: the run branch goes with the worktree, so the
	// issue's next run can `worktree add -b` the same branch again (issue #44).
	if err := exec.Command("git", "-C", repo, "rev-parse", "--verify", "--quiet",
		"refs/heads/"+branch).Run(); err == nil {
		t.Errorf("branch %s still present after cleanup", branch)
	}
}

// TestCreateIdempotentAfterCrash: a crashed run leaves its branch (and maybe
// worktree) behind; Create for the SAME issue branch under a new run ID must
// clear the residue and succeed instead of looping on "branch already exists"
// (issue #44).
func TestCreateIdempotentAfterCrash(t *testing.T) {
	branch := "auto-run/issue-44"
	cases := []struct {
		name    string
		residue func(t *testing.T, repo string)
	}{
		{"branch and worktree left behind", func(t *testing.T, repo string) {
			if _, err := Create(repo, "run-old", branch, "", "origin"); err != nil {
				t.Fatalf("residue Create: %v", err)
			}
		}},
		{"branch only", func(t *testing.T, repo string) {
			gitRun(t, repo, "branch", branch)
		}},
		{"worktree dir deleted manually, tracking entry stale", func(t *testing.T, repo string) {
			wt, err := Create(repo, "run-old", branch, "", "origin")
			if err != nil {
				t.Fatalf("residue Create: %v", err)
			}
			if err := os.RemoveAll(wt); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := makeRepo(t)
			tc.residue(t, repo)

			wt, err := Create(repo, "run-new", branch, "", "origin")
			if err != nil {
				t.Fatalf("Create after residue: %v", err)
			}
			out, err := exec.Command("git", "-C", wt, "rev-parse", "--abbrev-ref", "HEAD").Output()
			if err != nil {
				t.Fatal(err)
			}
			if got := string(out); got != branch+"\n" {
				t.Errorf("worktree branch = %q, want %q", got, branch)
			}
		})
	}
}

// TestCreateBranchCheckedOutInMainWorktree: when the run branch is checked out
// in the MAIN working tree, Create must fail with an error — never remove the
// main tree or its branch.
func TestCreateBranchCheckedOutInMainWorktree(t *testing.T) {
	repo := makeRepo(t)
	branch := "auto-run/issue-9"
	gitRun(t, repo, "checkout", "-qb", branch)

	if _, err := Create(repo, "rid", branch, "", "origin"); err == nil {
		t.Fatal("Create should fail when branch is checked out in the main worktree")
	}
	if _, err := os.Stat(filepath.Join(repo, "f.txt")); err != nil {
		t.Errorf("main worktree damaged: %v", err)
	}
	out, err := exec.Command("git", "-C", repo, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := string(out); got != branch+"\n" {
		t.Errorf("main worktree HEAD = %q, want %q", got, branch)
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
