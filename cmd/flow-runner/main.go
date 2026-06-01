// Command flow-runner is the Flow runner daemon (§4). It registers with the
// central service, then pulls work via POST /v1/leases/acquire (no GitHub
// polling), runs the S1–S12 orchestration per-run, and pushes telemetry +
// heartbeats. In production each run executes in a hardened container
// (deploy/Dockerfile.orchestrator, §11); this binary is the long-lived host
// daemon that drives those runs.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Silon-Oy/flow/internal/centralclient"
	"github.com/Silon-Oy/flow/internal/claude"
	"github.com/Silon-Oy/flow/internal/lease"
	"github.com/Silon-Oy/flow/internal/orchestrator"
)

func main() {
	central := envOr("FLOW_CENTRAL_URL", "http://localhost:8080")
	hostname, _ := os.Hostname()
	repoRoot := os.Getenv("FLOW_REPO_ROOT") // the runner's clone of the target repo
	pollInterval := durationOr("FLOW_POLL_INTERVAL", 15*time.Second)
	capacity := 1

	if repoRoot == "" {
		log.Fatal("flow-runner: FLOW_REPO_ROOT is required (the target-repo clone)")
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
	log.Printf("flow-runner: registered as %s (host %s)", runnerID, hostname)

	go runnerHeartbeat(ctx, cli, runnerID)

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := pullAndRun(ctx, cli, runnerID, repoRoot); err != nil {
				// A DB/transport error means fail-closed: log and back off, do
				// NOT fall back to a second arbiter.
				log.Printf("flow-runner: pull cycle error: %v", err)
			}
		}
	}
}

// pullAndRun acquires one unit of work (if any) and runs it to completion.
func pullAndRun(ctx context.Context, cli *centralclient.Client, runnerID, repoRoot string) error {
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
	if err := cli.PatchRun(ctx, runID, map[string]any{"runner_id": runnerID, "lease_id": lz.ID}); err != nil {
		log.Printf("flow-runner: patch run linkage: %v", err)
	}

	// Lease heartbeat for the duration of the run; if it ever fails (lease lost),
	// cancel the run context so the orchestrator aborts (split-brain guard, §5).
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	go leaseHeartbeat(runCtx, cli, lz.ID, runCancel)

	reporter := orchestrator.NewHTTPReporter(cli, runID)
	cfg := orchestrator.Config{
		RunID:       runID,
		RepoRoot:    repoRoot,
		Remote:      work.Remote,
		Branch:      branchFor(work),
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
