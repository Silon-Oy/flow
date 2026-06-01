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

	cmd := exec.Command("git", "-C", repoRoot, "worktree", "add", "-b", branch, wtPath, baseRef)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("worktree add: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return wtPath, nil
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

// Cleanup removes the worktree dir AND its tracking entry. Best-effort; only
// called in rollback paths.
func Cleanup(wtPath string) error {
	if _, err := os.Stat(wtPath); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	out, err := exec.Command("git", "-C", wtPath, "rev-parse", "--show-toplevel").Output()
	if err == nil {
		repoTop := strings.TrimSpace(string(out))
		if err := exec.Command("git", "-C", repoTop, "worktree", "remove", "--force", wtPath).Run(); err == nil {
			return nil
		}
	}
	return os.RemoveAll(wtPath)
}
