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
	"syscall"
	"time"

	"github.com/Silon-Oy/flow/internal/centralclient"
	"github.com/Silon-Oy/flow/internal/claude"
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
	// FLOW_RUNNER_MODE=container selects the sandboxed path; the default (inproc)
	// runs the orchestration in this process so the central <-> runner protocol
	// (lease, heartbeat, telemetry, S1-S12 sequencing) is exercised end-to-end
	// without requiring Docker on the dev box.
	if mode == "container" {
		return runInContainer(runCtx, cli, work, runID, branch, repoRoot, central)
	}

	reporter := orchestrator.NewHTTPReporter(cli, runID)
	cfg := orchestrator.Config{
		RunID:       runID,
		RepoRoot:    repoRoot,
		Remote:      work.Remote,
		Branch:      branch,
		IssueNumber: work.IssueNumber,
		IssuePrompt: "", // resolved from the issue body in a later pass
		AutoMode:    true,
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
func runInContainer(ctx context.Context, cli *centralclient.Client, work *lease.Work, runID, branch, repoRoot, central string) error {
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
		MemoryLimit:        envOr("FLOW_CONTAINER_MEMORY", ""),
		CPULimit:           envOr("FLOW_CONTAINER_CPUS", ""),
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
		IssuePrompt:  "", // resolved from the issue body in a later pass
		AutoMode:     true,
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
