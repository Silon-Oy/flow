package orchestrator

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// GitOps is the side-effecting boundary the orchestrator delegates to:
// dependency install, git push and PR creation.
//
// Responsibility split (§11.3 model C, decision 19): Install runs wherever the
// S7b env-bootstrap step runs — inside the per-run container in container
// mode. Push and OpenPR are TRUSTED-side operations: they run on the runner
// host after the agent container has exited, authenticated with a short-lived
// broker-minted GitHub App installation token (internal/runnergit). The
// container never holds a GitHub credential (invariant 3) — it commits to the
// mounted worktree and hands off; the runner pushes and opens the PR.
//
// Defining this as an interface keeps the S1–S12 machine unit-testable with a
// fake that records calls.
type GitOps interface {
	// Install runs the dependency installer (npm/pnpm/yarn/composer) in dir.
	Install(ctx context.Context, dir, manager string) error
	// Commit stages all worktree changes and commits them. Returns committed=false
	// (no error) when the agent produced no change. The agent edits files in the
	// container but cannot run git there (the worktree's .git points at an
	// unmounted host path), so the trusted runner commits on handoff.
	Commit(ctx context.Context, dir, message string) (committed bool, err error)
	// Push pushes branch from dir to its remote (auto-run/* only; enforced by
	// the App-token scope + branch protection, §11.4 / decision 12).
	Push(ctx context.Context, dir, branch string) error
	// OpenPR opens a pull request and returns its URL.
	OpenPR(ctx context.Context, dir, branch, baseBranch string, issueNumber int) (string, error)
}

// ShellGitOps shells out to git / package managers / gh with ambient
// credentials (R6: gh-shell-out). It is the Install implementation everywhere
// (the in-container env bootstrap needs no token) and the dev/test push path;
// production push/PR go through runnergit.GitOps, which authenticates with a
// broker-minted App token on the trusted runner side (§11.3 model C).
type ShellGitOps struct {
	// Remote is the git remote to push to (default "origin").
	Remote string
}

// Commit stages all changes in the worktree and commits them with an explicit
// identity (the runner container has no git user configured). Returns
// committed=false when there is nothing staged — the agent produced no change.
func (s ShellGitOps) Commit(ctx context.Context, dir, message string) (bool, error) {
	if err := runIn(ctx, dir, "git", "add", "-A"); err != nil {
		return false, fmt.Errorf("git add: %w", err)
	}
	// `git diff --cached --quiet` exits 0 when nothing is staged.
	check := exec.CommandContext(ctx, "git", "diff", "--cached", "--quiet")
	check.Dir = dir
	if err := check.Run(); err == nil {
		return false, nil // no changes to commit
	}
	if err := runIn(ctx, dir, "git",
		"-c", "user.name=Flow auto-run",
		"-c", "user.email=flow-auto-run@silon.local",
		"commit", "-m", message); err != nil {
		return false, fmt.Errorf("git commit: %w", err)
	}
	return true, nil
}

func (s ShellGitOps) Install(ctx context.Context, dir, manager string) error {
	var args []string
	switch manager {
	case "npm":
		args = []string{"npm", "install", "--no-audit", "--no-fund"}
	case "pnpm":
		args = []string{"pnpm", "install", "--frozen-lockfile"}
	case "yarn":
		args = []string{"yarn", "install", "--frozen-lockfile"}
	case "composer":
		args = []string{"composer", "install", "--no-interaction", "--no-progress"}
	default:
		return fmt.Errorf("unknown package manager %q", manager)
	}
	return runIn(ctx, dir, args[0], args[1:]...)
}

func (s ShellGitOps) Push(ctx context.Context, dir, branch string) error {
	remote := s.Remote
	if remote == "" {
		remote = "origin"
	}
	return runIn(ctx, dir, "git", "push", "-u", remote, branch)
}

func (s ShellGitOps) OpenPR(ctx context.Context, dir, branch, baseBranch string, issueNumber int) (string, error) {
	base := baseBranch
	if base == "" {
		base = "main"
	}
	cmd := exec.CommandContext(ctx, "gh", "pr", "create",
		"--head", branch, "--base", base,
		"--title", AutoRunPRTitle(issueNumber), "--body", AutoRunPRBody(issueNumber))
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("gh pr create: %w: %s", err, strings.TrimSpace(string(out)))
	}
	// gh prints the PR URL on the last non-empty output line.
	return LastURL(string(out)), nil
}

// AutoRunPRTitle is the canonical auto-run PR title, shared by every GitOps
// implementation so runs look identical regardless of which side pushed.
func AutoRunPRTitle(issueNumber int) string {
	return fmt.Sprintf("Auto-run: issue #%d", issueNumber)
}

// AutoRunPRBody is the canonical auto-run PR body.
func AutoRunPRBody(issueNumber int) string {
	return fmt.Sprintf("Automated PR for issue #%d (Flow auto-run).\n\nCloses #%d.", issueNumber, issueNumber)
}

// AutoRunCommitMessage is the canonical commit message for the agent's changes.
func AutoRunCommitMessage(issueNumber int) string {
	return fmt.Sprintf("Auto-run: implement issue #%d", issueNumber)
}

func runIn(ctx context.Context, dir, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// LastURL extracts the last https:// line from command output — gh prints the
// created PR's URL as its final line. Falls back to the trimmed output.
func LastURL(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, "https://") {
			return line
		}
	}
	return strings.TrimSpace(s)
}
