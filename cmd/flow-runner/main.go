// Command flow-runner is the Flow runner daemon (§4). It registers with the
// central service, then pulls work via POST /v1/leases/acquire (no GitHub
// polling), runs the S1–S12 orchestration per-run, and pushes telemetry +
// heartbeats. In production each run executes in a hardened container
// (deploy/Dockerfile.orchestrator, §11); this binary is the long-lived host
// daemon that drives those runs.
//
// The same binary is also the per-run container entrypoint: when invoked as
// `flow-runner orchestrate <run-id>` (the docker run argv built by
// internal/runnerexec.Spec.DockerArgs) it runs the S1–S12 machine inside the
// hardened container against the worktree mounted at /work.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Silon-Oy/flow/internal/centralclient"
	"github.com/Silon-Oy/flow/internal/claude"
	"github.com/Silon-Oy/flow/internal/egresship"
	"github.com/Silon-Oy/flow/internal/ghclient"
	"github.com/Silon-Oy/flow/internal/gitremote"
	"github.com/Silon-Oy/flow/internal/issue"
	"github.com/Silon-Oy/flow/internal/lease"
	"github.com/Silon-Oy/flow/internal/orchestrator"
	"github.com/Silon-Oy/flow/internal/runnerexec"
	"github.com/Silon-Oy/flow/internal/worktree"
)

func main() {
	// Subcommand dispatch: the per-run container is launched with
	// `flow-orchestrator orchestrate <run-id>` (built by runnerexec.Spec). Same
	// binary, different entrypoint — keeps build/release surface tight.
	if len(os.Args) >= 2 && os.Args[1] == "orchestrate" {
		if len(os.Args) < 3 {
			log.Fatal("flow-orchestrator: usage: orchestrate <run-id>")
		}
		if err := runOrchestrate(os.Args[2]); err != nil {
			log.Fatalf("flow-orchestrator: %v", err)
		}
		return
	}
	runDaemon()
}

// runDaemon is the long-lived runner-host daemon path (registers, polls for
// leases, drives each acquired run).
func runDaemon() {
	central := envOr("FLOW_CENTRAL_URL", "http://localhost:8080")
	hostname, _ := os.Hostname()
	repoRoot := os.Getenv("FLOW_REPO_ROOT") // the runner's clone of the target repo
	pollInterval := durationOr("FLOW_POLL_INTERVAL", 15*time.Second)
	capacity := 1

	if repoRoot == "" {
		log.Fatal("flow-runner: FLOW_REPO_ROOT is required (the target-repo clone)")
	}

	mode := envOr("FLOW_RUNNER_MODE", "inproc")
	if mode == "container" {
		// Fail fast on a missing dispatcher rather than silently fall back to
		// in-process (which would defeat the §11.1 isolation posture).
		if _, err := exec.LookPath("docker"); err != nil {
			log.Fatalf("flow-runner: FLOW_RUNNER_MODE=container but docker not in PATH: %v", err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-stop; log.Println("flow-runner: shutting down"); cancel() }()

	cli := centralclient.New(central, os.Getenv("FLOW_RUNNER_TOKEN"))

	runnerID, token, err := cli.RegisterRunner(ctx, hostname, capacity)
	if err != nil {
		log.Fatalf("flow-runner: register: %v", err)
	}
	if token != "" {
		cli.Token = token
	}
	log.Printf("flow-runner: registered as %s (host %s, mode %s)", runnerID, hostname, mode)

	go runnerHeartbeat(ctx, cli, runnerID)

	// §11.6: tail the egress-proxy's squid access.log and ship host-level
	// entries to flowd. The log path is opt-in (empty = disabled) so dev boxes
	// without the egress-proxy sidecar still boot.
	if logPath := os.Getenv("FLOW_EGRESS_LOG"); logPath != "" {
		go func() {
			if err := egresship.Run(ctx, egresship.Config{
				Path: logPath,
				Sink: egresship.CentralSink{Client: cli},
			}); err != nil {
				log.Printf("flow-runner: egress shipper exited: %v", err)
			}
		}()
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := pullAndRun(ctx, cli, runnerID, repoRoot, mode, central); err != nil {
				// A DB/transport error means fail-closed: log and back off, do
				// NOT fall back to a second arbiter.
				log.Printf("flow-runner: pull cycle error: %v", err)
			}
		}
	}
}

// pullAndRun acquires one unit of work (if any) and runs it to completion.
func pullAndRun(ctx context.Context, cli *centralclient.Client, runnerID, repoRoot, mode, central string) error {
	acq, err := cli.Acquire(ctx, runnerID, []string{"develop"})
	if err != nil {
		return err
	}
	if !acq.Acquired {
		return nil // empty queue — back off
	}
	work := acq.Work
	lz := acq.Lease
	log.Printf("flow-runner: acquired work %s (issue #%d) lease %s", work.WorkKey, work.IssueNumber, lz.ID)

	// Always release the lease when the run ends (completed or failed).
	defer func() {
		if err := cli.LeaseRelease(context.Background(), lz.ID); err != nil {
			log.Printf("flow-runner: lease release: %v", err)
		}
	}()

	runID, err := cli.CreateRun(ctx, work.ProjectID, work.Remote, work.IssueNumber)
	if err != nil {
		return err
	}
	branch := branchFor(work)
	// Persist runner/lease linkage + branch so the in-container orchestrator
	// (and the dashboard) can resolve them from the run record.
	if err := cli.PatchRun(ctx, runID, map[string]any{
		"runner_id": runnerID,
		"lease_id":  lz.ID,
		"branch":    branch,
	}); err != nil {
		log.Printf("flow-runner: patch run linkage: %v", err)
	}

	// Lease heartbeat for the duration of the run; if it ever fails (lease lost),
	// cancel the run context so the orchestrator aborts (split-brain guard, §5).
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	go leaseHeartbeat(runCtx, cli, lz.ID, runCancel)

	// Production isolation (§11.1): the runner creates the per-run worktree on
	// the host, then launches a hardened, ephemeral orchestrator container whose
	// ONLY host mount is that worktree (see internal/runnerexec.Spec for the
	// exact, test-asserted docker run flag set). The container's entrypoint runs
	// the same orchestration logic, but inside the capability-dropped, read-only,
	// egress-proxied sandbox.
	//
	// Vaihe 1 runs the orchestration in-process here so the central <-> runner
	// protocol (lease, heartbeat, telemetry, S1-S12 sequencing) is exercised
	// end-to-end without requiring Docker on the dev box. The container dispatch
	// is the deploy-time path (docker-compose mounts the Docker socket into the
	// trusted runner; the untrusted orchestrator container never gets it).
	// FLOW_RUNNER_MODE=container selects the sandboxed path; default is inproc.
	if mode == "container" {
		return runInContainer(runCtx, cli, work, runID, branch, repoRoot, central, acq.Env)
	}

	// Fetch issue body + comments + image URLs on the trusted host BEFORE
	// dispatching to the orchestrator. The orchestrator runs inside the
	// hardened container (§11.1) and must not hold a GitHub token; doing the
	// fetch here keeps the token out of the sandbox surface.
	issueDoc, imageURLs := fetchIssueContext(ctx, work, repoRoot)

	reporter := orchestrator.NewHTTPReporter(cli, runID)
	cfg := orchestrator.Config{
		RunID:          runID,
		RepoRoot:       repoRoot,
		Remote:         work.Remote,
		Branch:         branch,
		IssueNumber:    work.IssueNumber,
		IssueTitle:     issueDoc.title,
		IssueBody:      issueDoc.body,
		IssueComments:  issueDoc.comments,
		IssueImageURLs: imageURLs,
		AutoMode:       true,
	}
	o := orchestrator.New(cfg, claude.New(), reporter)
	gitOps := orchestrator.ShellGitOps{Remote: work.Remote}

	outcome, err := o.Run(runCtx, gitOps)
	if err != nil {
		log.Printf("flow-runner: run %s error: %v", runID, err)
		return nil
	}
	log.Printf("flow-runner: run %s finished: status=%s step=%s", runID, outcome.Status, outcome.LastStep)
	return nil
}

// runInContainer creates the per-run worktree on the host and dispatches the
// orchestration into a hardened ephemeral container (§11.1). The host never
// runs the orchestrator itself in this path — that is the trust boundary.
//
// Reporter/telemetry: the host does NOT report stage transitions here; the
// in-container orchestrator reports them itself via centralclient (FLOW_CENTRAL_URL
// + FLOW_RUNNER_TOKEN passed in env by runnerexec.Spec). On container exit we
// log the result; the run's terminal status is whatever the container PATCHed.
func runInContainer(ctx context.Context, cli *centralclient.Client, work *lease.Work, runID, branch, repoRoot, central string, leaseEnv map[string]string) error {
	// S4 (host-side): fetch the remote and create the per-run worktree. The
	// orchestrator inside the container will see this as /work and skip its own
	// worktree.Create (Config.WorktreePath is non-empty).
	worktree.Fetch(repoRoot, work.Remote)
	wt, err := worktree.Create(repoRoot, runID, branch, "", work.Remote)
	if err != nil {
		// Mark the run blocked so the dashboard reflects what happened — the
		// container never started, so the host owns this transition.
		_ = cli.PatchRun(ctx, runID, map[string]any{
			"status":         "blocked",
			"blocked_reason": "worktree_create_failed: " + err.Error(),
			"finished":       true,
		})
		return nil
	}

	spec := runnerexec.Spec{
		Image:              envOr("FLOW_ORCHESTRATOR_IMAGE", "flow-orchestrator:latest"),
		RunID:              runID,
		WorktreePath:       wt,
		RunnerName:         envOr("FLOW_RUNNER_NAME", ""),
		EgressProxy:        envOr("FLOW_EGRESS_PROXY", ""),
		CentralURL:         central,
		CentralToken:       cli.Token,
		ClaudeCredHostPath: os.Getenv("FLOW_CLAUDE_CRED_PATH"),
		// §9 delivery='env' secrets resolved by the central at lease-acquire
		// time. The runner never reads secret_value rows itself; it just
		// forwards the materialised map. §11.3 defense-in-depth filtering of
		// GITHUB_TOKEN/GH_TOKEN lives in runnerexec.DockerArgs().
		Env:         leaseEnv,
		MemoryLimit: envOr("FLOW_CONTAINER_MEMORY", ""),
		CPULimit:    envOr("FLOW_CONTAINER_CPUS", ""),
	}

	// Defensive guard: the §11.1 invariant test owns the same assertion, but a
	// last-line check before dispatch costs nothing and catches a refactor that
	// would otherwise mount the Docker socket into the untrusted container.
	args := spec.DockerArgs()
	if runnerexec.HasForbiddenMounts(args) {
		_ = cli.PatchRun(ctx, runID, map[string]any{
			"status":         "blocked",
			"blocked_reason": "refusing_dispatch: forbidden mount (§11.1)",
			"finished":       true,
		})
		return errors.New("refusing to dispatch: forbidden host mount")
	}

	log.Printf("flow-runner: dispatching run %s to container (image %s)", runID, spec.Image)
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		// Container exit ≠ 0 means the orchestrator inside either reported the
		// failure (terminal status already PATCHed) or died abnormally. Log; do
		// not retry — the lease release will let another runner pick this up.
		log.Printf("flow-runner: run %s container exited: %v", runID, err)
	}
	log.Printf("flow-runner: run %s container finished", runID)
	return nil
}

// runOrchestrate is the in-container entrypoint (§11.1). It runs the S1–S12
// machine against /work (the only host mount) and reports telemetry back to the
// central service via centralclient — the run's host context (the runner's
// clone, the GitHub credential) is deliberately NOT available here.
func runOrchestrate(runID string) error {
	central := os.Getenv("FLOW_CENTRAL_URL")
	if central == "" {
		return errors.New("FLOW_CENTRAL_URL is required in the orchestrator container")
	}
	token := os.Getenv("FLOW_RUNNER_TOKEN")
	if token == "" {
		return errors.New("FLOW_RUNNER_TOKEN is required in the orchestrator container")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-stop; cancel() }()

	cli := centralclient.New(central, token)

	run, err := cli.GetRun(ctx, runID)
	if err != nil {
		return fmt.Errorf("fetch run %s: %w", runID, err)
	}
	branch := ""
	if run.Branch != nil {
		branch = *run.Branch
	}
	if branch == "" {
		return fmt.Errorf("run %s has no branch set (host did not PATCH it)", runID)
	}

	reporter := orchestrator.NewHTTPReporter(cli, runID)
	cfg := orchestrator.Config{
		RunID:        runID,
		WorktreePath: "/work", // single host mount; orchestrator skips S4 create
		Remote:       run.Remote,
		Branch:       branch,
		IssueNumber:  run.IssueNumber,
		// Issue body/comments/images are fetched on the trusted host (it holds the
		// GitHub token); the container path will receive them via the run record in
		// a later pass. Left empty here so the sandbox never needs a token.
		AutoMode: true,
	}
	o := orchestrator.New(cfg, claude.New(), reporter)
	gitOps := orchestrator.ShellGitOps{Remote: run.Remote}

	outcome, err := o.Run(ctx, gitOps)
	if err != nil {
		return fmt.Errorf("orchestrator: %w", err)
	}
	log.Printf("flow-orchestrator: run %s finished: status=%s step=%s",
		runID, outcome.Status, outcome.LastStep)
	return nil
}

func branchFor(w *lease.Work) string {
	// auto-run/<remote-label>-<issue> — slug is omitted in Vaihe 1; the issue
	// number keeps branches unique per remote (gitremote.RemoteLabel handles the
	// origin/non-origin split when the slug is added later).
	if w.Remote == "origin" || w.Remote == "" {
		return "auto-run/issue-" + itoa(w.IssueNumber)
	}
	return "auto-run/" + w.Remote + "-issue-" + itoa(w.IssueNumber)
}

func runnerHeartbeat(ctx context.Context, cli *centralclient.Client, runnerID string) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := cli.RunnerHeartbeat(ctx, runnerID); err != nil {
				log.Printf("flow-runner: runner heartbeat: %v", err)
			}
		}
	}
}

// leaseHeartbeat keeps the lease alive every 60s (§5). A failed heartbeat means
// the lease was lost/reaped — cancel the run so the orchestrator stops before
// any further side effects (split-brain guard).
func leaseHeartbeat(ctx context.Context, cli *centralclient.Client, leaseID string, cancel context.CancelFunc) {
	ticker := time.NewTicker(lease.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := cli.LeaseHeartbeat(ctx, leaseID); err != nil {
				log.Printf("flow-runner: lease heartbeat lost (%v) — aborting run", err)
				cancel()
				return
			}
		}
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func durationOr(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

// issueContext is the runner's view of a fetched issue before it becomes
// orchestrator.Config.Issue* fields. A bare value type (no error) — fetch
// failures are logged and degrade to an empty-context run rather than
// blocking the lease that has already been acquired.
type issueContext struct {
	title    string
	body     string
	comments []orchestrator.IssueComment
}

// fetchIssueContext resolves the remote → owner/repo, calls FetchIssue, and
// extracts image URLs. Any failure (no remote, missing token, network) is
// reported as an empty context — the orchestrator still runs with whatever it
// has so a transient GitHub outage doesn't strand the lease.
func fetchIssueContext(ctx context.Context, work *lease.Work, repoRoot string) (issueContext, []string) {
	remote := work.Remote
	if remote == "" {
		remote = "origin"
	}
	ownerRepo, ok := gitremote.ResolveRemoteToOwnerRepo(repoRoot, remote)
	if !ok {
		log.Printf("flow-runner: fetchIssueContext: cannot resolve remote %q in %s", remote, repoRoot)
		return issueContext{}, nil
	}
	parts := strings.SplitN(ownerRepo, "/", 2)
	if len(parts) != 2 {
		log.Printf("flow-runner: fetchIssueContext: bad owner/repo %q", ownerRepo)
		return issueContext{}, nil
	}
	owner, repo := parts[0], parts[1]

	gh := ghclient.New(os.Getenv("FLOW_GITHUB_TOKEN"))
	is, err := gh.FetchIssue(ctx, owner, repo, work.IssueNumber)
	if err != nil {
		log.Printf("flow-runner: FetchIssue %s/%s#%d: %v", owner, repo, work.IssueNumber, err)
		return issueContext{}, nil
	}
	urls, err := issue.ExtractImageURLs(is.RawJSON)
	if err != nil {
		log.Printf("flow-runner: ExtractImageURLs: %v", err)
		urls = nil
	}
	cs := make([]orchestrator.IssueComment, 0, len(is.Comments))
	for _, c := range is.Comments {
		// Strip run-issues bot comments here so the agent prompt sees only
		// human/issue-author text. ExtractImageURLs already skips them; this
		// keeps the prompt and the image list consistent.
		if strings.Contains(c.Body, "run-issues:") {
			continue
		}
		cs = append(cs, orchestrator.IssueComment{Author: c.Author, Body: c.Body})
	}
	return issueContext{title: is.Title, body: is.Body, comments: cs}, urls
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
