// Package runnergit implements the trusted-runner side of the GitHub write
// path (§11.3 model C, decision 19): git push and PR creation run on the
// runner host — after the agent container has exited in container mode —
// authenticated with a short-lived GitHub App installation token minted by
// the central token broker (§7.3, GET /v1/github-app/token).
//
// Invariant 3 (§11.3): the token never crosses the trust boundary into the
// run container. It lives only in this process's memory and is handed to
// git/gh exclusively via the child-process environment — never argv (visible
// in `ps`/logs), never disk. Error messages wrap command output, which never
// contains the credential (git/gh do not echo auth headers or GH_TOKEN).
//
// Multi-org: the org is resolved from the run's git remote (owner/repo), so
// non-origin remotes mint against their own App installation instead of
// falling back to a personal credential (#8 acceptance criterion).
package runnergit

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/Silon-Oy/flow/internal/centralclient"
	"github.com/Silon-Oy/flow/internal/gitremote"
	"github.com/Silon-Oy/flow/internal/orchestrator"
)

// tokenExpiryMargin is how close to expiry a cached installation token may be
// before it is re-minted (GitHub App tokens last ~1h; pushes near the end of
// a long run must not race the expiry).
const tokenExpiryMargin = 5 * time.Minute

// Minter mints GitHub App installation tokens; *centralclient.Client is the
// production implementation (§7.3 broker endpoint).
type Minter interface {
	MintGitHubAppToken(ctx context.Context, tenant, org string) (*centralclient.GitHubAppToken, error)
}

// GitOps implements orchestrator.GitOps with broker-minted credentials on the
// trusted runner side. Install delegates to the ambient ShellGitOps (no
// credential needed); Push and OpenPR mint (and cache) an installation token
// for the remote's org and pass it to git/gh via the environment only.
type GitOps struct {
	// Remote is the git remote name the run targets (default "origin"). The
	// push/PR target owner/repo — and the org the token is minted for — are
	// resolved from this remote's URL in the worktree.
	Remote string
	// Tenant may be empty in Vaihe 1 — the central resolves the bootstrap
	// tenant (Vaihe 2 disambiguates via the runner token).
	Tenant string
	Minter Minter
	// VerifyLease, when set, runs before every side-effecting GitHub operation
	// (push, PR create) as the §10/R5 split-brain guard: an error means the
	// lease is no longer held and the operation is aborted.
	VerifyLease func(ctx context.Context) error

	// runCmd is the exec seam for tests; nil = real exec with the token-bearing
	// env appended to the parent environment.
	runCmd func(ctx context.Context, dir string, extraEnv []string, name string, args ...string) (string, error)

	// tok caches the minted token without an org key: safe only because one
	// GitOps instance serves exactly one run and hence one Remote/org. Do not
	// share an instance across runs — a stale cross-org token would be reused.
	mu  sync.Mutex
	tok *centralclient.GitHubAppToken
}

// Install runs the dependency installer with ambient tooling — no credential
// involved, identical to the in-container env-bootstrap path.
func (g *GitOps) Install(ctx context.Context, dir, manager string) error {
	return orchestrator.ShellGitOps{Remote: g.Remote}.Install(ctx, dir, manager)
}

// Commit delegates to ShellGitOps — committing the worktree needs no credential,
// only the trusted runner's local git (which, unlike the container, can resolve
// the worktree's .git pointer).
func (g *GitOps) Commit(ctx context.Context, dir, message string) (bool, error) {
	return orchestrator.ShellGitOps{Remote: g.Remote}.Commit(ctx, dir, message)
}

// Push pushes branch to the remote's https URL with the broker token in an
// http.extraheader supplied via GIT_CONFIG_* env vars. Pushing the explicit
// https URL (not the remote name) guarantees the App token is the credential
// used even when the clone's remote is an ssh URL — no fallback to personal
// keys/keyring (#8 / multi-org criterion).
func (g *GitOps) Push(ctx context.Context, dir, branch string) error {
	ownerRepo, token, err := g.prepare(ctx, dir)
	if err != nil {
		return fmt.Errorf("push %s: %w", branch, err)
	}
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	env := []string{
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.https://github.com/.extraheader",
		"GIT_CONFIG_VALUE_0=Authorization: basic " + basic,
	}
	out, err := g.exec(ctx, dir, env, "git", "push", "https://github.com/"+ownerRepo+".git", branch)
	if err != nil {
		return fmt.Errorf("git push %s: %w: %s", branch, err, strings.TrimSpace(out))
	}
	return nil
}

// OpenPR opens the pull request via gh with the broker token as GH_TOKEN (env
// only). --repo pins the target so gh never infers a different remote.
func (g *GitOps) OpenPR(ctx context.Context, dir, branch, baseBranch string, issueNumber int) (string, error) {
	ownerRepo, token, err := g.prepare(ctx, dir)
	if err != nil {
		return "", fmt.Errorf("open pr: %w", err)
	}
	base := baseBranch
	if base == "" {
		base = "main"
	}
	out, err := g.exec(ctx, dir, []string{"GH_TOKEN=" + token}, "gh", "pr", "create",
		"--repo", ownerRepo, "--head", branch, "--base", base,
		"--title", orchestrator.AutoRunPRTitle(issueNumber),
		"--body", orchestrator.AutoRunPRBody(issueNumber))
	if err != nil {
		return "", fmt.Errorf("gh pr create: %w: %s", err, strings.TrimSpace(out))
	}
	return orchestrator.LastURL(out), nil
}

// prepare gates on the lease, resolves the remote to owner/repo and returns a
// valid (cached or freshly minted) installation token for the org.
func (g *GitOps) prepare(ctx context.Context, dir string) (ownerRepo, token string, err error) {
	if g.VerifyLease != nil {
		if err := g.VerifyLease(ctx); err != nil {
			return "", "", fmt.Errorf("lease not held (§10/R5): %w", err)
		}
	}
	remote := g.Remote
	if remote == "" {
		remote = "origin"
	}
	ownerRepo, ok := gitremote.ResolveRemoteToOwnerRepo(dir, remote)
	if !ok {
		return "", "", fmt.Errorf("cannot resolve remote %q in %s to a github.com owner/repo", remote, dir)
	}
	org := ownerRepo[:strings.IndexByte(ownerRepo, '/')]
	token, err = g.token(ctx, org)
	if err != nil {
		return "", "", err
	}
	return ownerRepo, token, nil
}

func (g *GitOps) token(ctx context.Context, org string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.tok != nil && time.Until(g.tok.ExpiresAt) > tokenExpiryMargin {
		return g.tok.Token, nil
	}
	t, err := g.Minter.MintGitHubAppToken(ctx, g.Tenant, org)
	if err != nil {
		return "", fmt.Errorf("mint github app token for %s: %w", org, err)
	}
	g.tok = t
	return t.Token, nil
}

func (g *GitOps) exec(ctx context.Context, dir string, extraEnv []string, name string, args ...string) (string, error) {
	if g.runCmd != nil {
		return g.runCmd(ctx, dir, extraEnv, name, args...)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), extraEnv...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
