// Package worktree manages per-run git worktrees in the target repo (REWRITE of
// lib/worktree.sh). Worktrees live under <repo>/.claude/worktrees/<run-id>/ and
// are the single host mount handed to the per-run container (§11.1).
//
// Multi-remote: every function takes a remote (default "origin"). It resolves
// the base ref and chooses which remote to fetch, so one clone can branch off
// origin/main and wisol/main alike.
package worktree

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// FetchOutcome reports the result of Fetch.
type FetchOutcome int

const (
	FetchOK        FetchOutcome = iota // fetch succeeded
	FetchFailed                        // fetch ran but failed (network/auth); refs may be stale
	FetchNoRemote                      // the named remote does not exist; benign no-op
)

// Fetch updates the named remote's local refs before a worktree is branched off
// them (REWRITE of refresh_origin). It works for any remote despite the legacy
// name.
func Fetch(repoRoot, remote string) FetchOutcome {
	if remote == "" {
		remote = "origin"
	}
	if err := exec.Command("git", "-C", repoRoot, "remote", "get-url", remote).Run(); err != nil {
		return FetchNoRemote
	}
	if err := exec.Command("git", "-C", repoRoot, "fetch", remote, "--quiet").Run(); err != nil {
		return FetchFailed
	}
	return FetchOK
}

// Create makes a worktree at <repo>/.claude/worktrees/<runID>/ on a NEW branch.
// The base ref resolves in order:
//  1. <remote>/<baseBranch> when baseBranch is given
//  2. the named remote's default HEAD (<remote>/main, via symbolic-ref)
//  3. local HEAD (brand-new repo)
//
// The base ref is assumed already fresh — the caller runs Fetch first. Returns
// the worktree path.
func Create(repoRoot, runID, branch, baseBranch, remote string) (string, error) {
	if remote == "" {
		remote = "origin"
	}
	basePath := filepath.Join(repoRoot, ".claude", "worktrees")
	if err := os.MkdirAll(basePath, 0o755); err != nil {
		return "", err
	}
	wtPath := filepath.Join(basePath, runID)

	baseRef := resolveBaseRef(repoRoot, baseBranch, remote)

	// Validate an explicit base branch resolves — a clear error beats a cryptic
	// `git worktree add` failure downstream.
	if baseBranch != "" {
		ref := "refs/remotes/" + remote + "/" + baseBranch
		if err := exec.Command("git", "-C", repoRoot, "rev-parse", "--verify", "--quiet", ref).Run(); err != nil {
			return "", fmt.Errorf("base branch %q not found (remote fresh)", remote+"/"+baseBranch)
		}
	}

	// `git worktree add -b` is not idempotent: a crashed earlier run for the
	// same issue leaves its branch (and worktree) behind, and every retry would
	// fail here forever (issue #44). Clear the residue first.
	removeStaleBranch(repoRoot, branch)

	cmd := exec.Command("git", "-C", repoRoot, "worktree", "add", "-b", branch, wtPath, baseRef)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("worktree add: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return wtPath, nil
}

// removeStaleBranch clears what a crashed run left behind: the linked worktree
// still holding branch (whatever run ID it was created under) and the local
// branch itself. Best-effort by design — git refuses to remove the MAIN
// working tree and to delete its checked-out branch, so a branch in use there
// surfaces as a normal Create error instead of data loss.
func removeStaleBranch(repoRoot, branch string) {
	// Drop tracking entries whose dirs were already deleted manually; otherwise
	// the branch still counts as checked out and cannot be deleted.
	_ = exec.Command("git", "-C", repoRoot, "worktree", "prune").Run()
	if wt := worktreePathForBranch(repoRoot, branch); wt != "" {
		_ = exec.Command("git", "-C", repoRoot, "worktree", "remove", "--force", wt).Run()
	}
	if exec.Command("git", "-C", repoRoot, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch).Run() == nil {
		_ = exec.Command("git", "-C", repoRoot, "branch", "-D", branch).Run()
	}
}

// worktreePathForBranch returns the path of the worktree that has branch
// checked out, or "" if none.
func worktreePathForBranch(repoRoot, branch string) string {
	out, err := exec.Command("git", "-C", repoRoot, "worktree", "list", "--porcelain").Output()
	if err != nil {
		return ""
	}
	var path string
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			path = strings.TrimPrefix(line, "worktree ")
		case line == "branch refs/heads/"+branch:
			return path
		}
	}
	return ""
}

func resolveBaseRef(repoRoot, baseBranch, remote string) string {
	if baseBranch != "" {
		return remote + "/" + baseBranch
	}
	out, err := exec.Command("git", "-C", repoRoot, "symbolic-ref", "--short",
		"refs/remotes/"+remote+"/HEAD").Output()
	if err == nil {
		if ref := strings.TrimSpace(string(out)); ref != "" {
			return ref
		}
	}
	return "HEAD"
}

// Cleanup removes the worktree dir, its tracking entry AND the run branch it
// had checked out. Best-effort; only called in rollback paths, where the
// branch holds nothing worth keeping — leaving it would wedge the next run
// for the same issue (issue #44).
func Cleanup(wtPath string) error {
	if _, err := os.Stat(wtPath); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	// Resolve the run branch and the main repo BEFORE removing the worktree —
	// neither can be read from a deleted dir.
	var branch, mainRepo string
	if out, err := exec.Command("git", "-C", wtPath, "rev-parse", "--abbrev-ref", "HEAD").Output(); err == nil {
		if b := strings.TrimSpace(string(out)); b != "HEAD" {
			branch = b
		}
	}
	if out, err := exec.Command("git", "-C", wtPath, "rev-parse", "--path-format=absolute", "--git-common-dir").Output(); err == nil {
		mainRepo = filepath.Dir(strings.TrimSpace(string(out)))
	}

	removed := false
	if err := exec.Command("git", "-C", wtPath, "worktree", "remove", "--force", wtPath).Run(); err == nil {
		removed = true
	}
	var rmErr error
	if !removed {
		rmErr = os.RemoveAll(wtPath)
	}
	if branch != "" && mainRepo != "" && mainRepo != wtPath {
		_ = exec.Command("git", "-C", mainRepo, "branch", "-D", branch).Run()
	}
	return rmErr
}
