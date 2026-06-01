package orchestrator

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// GitOps is the side-effecting boundary the orchestrator delegates to: git
// push, PR creation, and dependency install. These run inside the per-run
// container; the egress proxy injects the GitHub credential on the way out
// (§11.3), so commands here never carry a token.
//
// Defining this as an interface keeps the S1–S12 machine unit-testable with a
// fake that records calls, while production uses ShellGitOps.
type GitOps interface {
	// Install runs the dependency installer (npm/pnpm/yarn/composer) in dir.
	Install(ctx context.Context, dir, manager string) error
	// Push pushes branch from dir to its remote (scoped to auto-run/* by the proxy).
	Push(ctx context.Context, dir, branch string) error
	// OpenPR opens a pull request and returns its URL.
	OpenPR(ctx context.Context, dir, branch, baseBranch string, issueNumber int) (string, error)
}

// ShellGitOps is the production GitOps: it shells out to git / package managers
// / gh (R6: gh-shell-out for the runner's git operations). All commands run
// with dir as the working directory.
type ShellGitOps struct {
	// Remote is the git remote to push to (default "origin").
	Remote string
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
	title := fmt.Sprintf("Auto-run: issue #%d", issueNumber)
	body := fmt.Sprintf("Automated PR for issue #%d (Flow auto-run).\n\nCloses #%d.", issueNumber, issueNumber)
	cmd := exec.CommandContext(ctx, "gh", "pr", "create",
		"--head", branch, "--base", base, "--title", title, "--body", body)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("gh pr create: %w: %s", err, strings.TrimSpace(string(out)))
	}
	// gh prints the PR URL on the last non-empty output line.
	return lastURL(string(out)), nil
}

func runIn(ctx context.Context, dir, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func lastURL(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, "https://") {
			return line
		}
	}
	return strings.TrimSpace(s)
}
