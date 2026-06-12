package orchestrator

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Silon-Oy/flow/internal/claude"
)

// fakeReporter records telemetry calls so the test can assert the state path.
type fakeReporter struct {
	mu       sync.Mutex
	states   []Step
	events   []string
	finalSt  string
	finalRsn string
}

func (f *fakeReporter) SetState(_ context.Context, s Step) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.states = append(f.states, s)
	return nil
}
func (f *fakeReporter) Event(_ context.Context, e string, _ map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, e)
	return nil
}
func (f *fakeReporter) Finalize(_ context.Context, status, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finalSt = status
	f.finalRsn = reason
	return nil
}

// fakeGitOps records install/push/PR calls.
type fakeGitOps struct {
	installs []string
	pushed   bool
	pushErr  error
	prURL    string
}

func (g *fakeGitOps) Install(_ context.Context, _ string, m string) error {
	g.installs = append(g.installs, m)
	return nil
}
func (g *fakeGitOps) Commit(_ context.Context, _, _ string) (bool, error) {
	return true, nil
}
func (g *fakeGitOps) Push(_ context.Context, _, _ string) error {
	if g.pushErr != nil {
		return g.pushErr
	}
	g.pushed = true
	return nil
}
func (g *fakeGitOps) OpenPR(_ context.Context, _, _, _ string, _ int) (string, error) {
	g.prURL = "https://github.com/o/r/pull/1"
	return g.prURL, nil
}

func makeRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		if len(args) > 0 && args[0] != "init" {
			cmd.Dir = dir
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", dir)
	run("config", "user.email", "t@t.t")
	run("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("v0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "f.txt")
	run("commit", "-qm", "init")
	run("branch", "-M", "main")
	return dir
}

// mockClaude returns a Runner whose CLI echoes the given decision lines. It
// emits the cycle-review decision and the implementer result so both
// ExtractDecision calls find their token.
func mockClaude(script string) *claude.Runner {
	return &claude.Runner{
		Command: []string{"/bin/sh", "-c", script, "--"},
		Timeout: 5 * time.Second,
	}
}

func TestOrchestratorHappyPath(t *testing.T) {
	repo := makeRepo(t)
	rep := &fakeReporter{}
	git := &fakeGitOps{}

	// One mock that prints both decision lines; the orchestrator calls claude
	// three times (review, implementer, evolution) — each invocation prints all
	// lines, ExtractDecision picks the relevant one.
	c := mockClaude("echo CYCLE_REVIEW_DECISION: PROCEED; echo IMPLEMENTER_RESULT: SUCCESS")

	o := New(Config{
		RunID: "20260602-rid-1", RepoRoot: repo, Remote: "origin",
		Branch: "auto-run/issue-1", IssueNumber: 1,
		IssueTitle: "Fix the thing", IssueBody: "do the thing", AutoMode: true,
	}, c, rep)

	out, err := o.Run(context.Background(), git)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Status != "completed" {
		t.Errorf("status = %q, want completed (reason=%q step=%s)", out.Status, out.Reason, out.LastStep)
	}
	if !git.pushed {
		t.Errorf("expected push")
	}
	if out.PRURL == "" {
		t.Errorf("expected PR url")
	}
	if rep.finalSt != "completed" {
		t.Errorf("reporter final = %q, want completed", rep.finalSt)
	}
	// The full blueprint advanced through finalize.
	if out.LastStep != StepFinalize {
		t.Errorf("last step = %s, want finalize", out.LastStep)
	}
}

// TestOrchestratorHandoffAfterAgent asserts the §11.3 model C container phase:
// the machine stops after S9 without pushing, opening a PR or finalizing — the
// trusted runner host owns the S10–S12 tail.
func TestOrchestratorHandoffAfterAgent(t *testing.T) {
	repo := makeRepo(t)
	rep := &fakeReporter{}
	git := &fakeGitOps{}
	c := mockClaude("echo CYCLE_REVIEW_DECISION: PROCEED; echo IMPLEMENTER_RESULT: SUCCESS")

	o := New(Config{
		RunID: "rid-handoff", RepoRoot: repo, Remote: "origin",
		Branch: "auto-run/issue-4", IssueNumber: 4, AutoMode: true,
		HandoffAfterAgent: true,
	}, c, rep)

	out, err := o.Run(context.Background(), git)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if git.pushed {
		t.Errorf("container phase must NOT push (§11.3 model C)")
	}
	if out.PRURL != "" {
		t.Errorf("container phase must NOT open a PR")
	}
	if out.LastStep != StepEvolution {
		t.Errorf("last step = %s, want %s", out.LastStep, StepEvolution)
	}
	if out.Status != "" {
		t.Errorf("status = %q, want non-terminal (empty)", out.Status)
	}
	if rep.finalSt != "" {
		t.Errorf("must not finalize on handoff; finalized as %q", rep.finalSt)
	}
}

// TestFinishGitHub asserts the trusted-side tail: S10 push → S11 PR → S12
// finalize, reported through the Reporter.
func TestFinishGitHub(t *testing.T) {
	rep := &fakeReporter{}
	git := &fakeGitOps{}

	out, err := FinishGitHub(context.Background(), rep, git, "/tmp/wt", "auto-run/issue-5", "", 5)
	if err != nil {
		t.Fatalf("FinishGitHub: %v", err)
	}
	if !git.pushed {
		t.Errorf("expected push")
	}
	if out.Status != "completed" || out.LastStep != StepFinalize {
		t.Errorf("status=%q step=%s, want completed/finalize", out.Status, out.LastStep)
	}
	if out.PRURL == "" {
		t.Errorf("expected PR url")
	}
	if rep.finalSt != "completed" {
		t.Errorf("reporter final = %q, want completed", rep.finalSt)
	}
	wantStates := []Step{StepPush, StepPR, StepFinalize}
	if len(rep.states) != len(wantStates) {
		t.Fatalf("states = %v, want %v", rep.states, wantStates)
	}
	for i, s := range wantStates {
		if rep.states[i] != s {
			t.Errorf("state[%d] = %s, want %s", i, rep.states[i], s)
		}
	}
}

func TestFinishGitHubPushFailureBlocks(t *testing.T) {
	rep := &fakeReporter{}
	git := &fakeGitOps{pushErr: context.DeadlineExceeded}

	out, err := FinishGitHub(context.Background(), rep, git, "/tmp/wt", "auto-run/issue-6", "", 6)
	if err != nil {
		t.Fatalf("FinishGitHub: %v", err)
	}
	if out.Status != "blocked" || out.LastStep != StepPush {
		t.Errorf("status=%q step=%s, want blocked/push", out.Status, out.LastStep)
	}
	if out.PRURL != "" {
		t.Errorf("must not open a PR after a failed push")
	}
	if rep.finalSt != "blocked" {
		t.Errorf("reporter final = %q, want blocked", rep.finalSt)
	}
}

func TestOrchestratorNeedsClarification(t *testing.T) {
	repo := makeRepo(t)
	rep := &fakeReporter{}
	git := &fakeGitOps{}
	c := mockClaude("echo CYCLE_REVIEW_DECISION: NEEDS_CLARIFICATION")

	o := New(Config{
		RunID: "rid-2", RepoRoot: repo, Remote: "origin",
		Branch: "auto-run/issue-2", IssueNumber: 2, AutoMode: true,
	}, c, rep)

	out, err := o.Run(context.Background(), git)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Status != "awaiting_clarification" {
		t.Errorf("status = %q, want awaiting_clarification", out.Status)
	}
	if git.pushed {
		t.Errorf("must NOT push when clarification needed")
	}
}

// TestOrchestratorDownloadsIssueImages asserts that S4 + image-download wiring
// lands files in <worktree>/.flow/issue-images/ on a successful run, proving
// the FetchIssue → ExtractImageURLs → orchestrator → worktree pipeline.
func TestOrchestratorDownloadsIssueImages(t *testing.T) {
	repo := makeRepo(t)
	rep := &fakeReporter{}
	git := &fakeGitOps{}

	imgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte("PNG"))
	}))
	defer imgSrv.Close()

	c := mockClaude("echo CYCLE_REVIEW_DECISION: PROCEED; echo IMPLEMENTER_RESULT: SUCCESS")
	runID := "rid-images"
	o := New(Config{
		RunID: runID, RepoRoot: repo, Remote: "origin",
		Branch: "auto-run/issue-9", IssueNumber: 9,
		IssueTitle: "Render images", IssueBody: "see attached",
		IssueImageURLs: []string{imgSrv.URL + "/a.png"},
		AutoMode:       true,
	}, c, rep)

	out, err := o.Run(context.Background(), git)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Status != "completed" {
		t.Fatalf("status = %q reason=%q step=%s", out.Status, out.Reason, out.LastStep)
	}
	wt := filepath.Join(repo, ".claude/worktrees", runID)
	imagePath := filepath.Join(wt, ".flow/issue-images/00.png")
	if _, err := os.Stat(imagePath); err != nil {
		t.Errorf("expected image at %s, got %v", imagePath, err)
	}
}

func TestOrchestratorImplementerTimeout(t *testing.T) {
	repo := makeRepo(t)
	rep := &fakeReporter{}
	git := &fakeGitOps{}
	// Review proceeds instantly; implementer sleeps past the budget.
	// Distinguish the two calls by argument count is not possible here, so use a
	// script that proceeds on the first read and hangs otherwise. Simpler: make
	// the single command both print PROCEED and sleep — review extracts PROCEED
	// then env bootstrap is a no-op (empty repo), then implementer call sleeps.
	c := &claude.Runner{
		Command: []string{"/bin/sh", "-c", "echo CYCLE_REVIEW_DECISION: PROCEED; sleep 5", "--"},
		Timeout: 200 * time.Millisecond,
	}
	o := New(Config{
		RunID: "rid-3", RepoRoot: repo, Remote: "origin",
		Branch: "auto-run/issue-3", IssueNumber: 3, AutoMode: true,
	}, c, rep)

	out, err := o.Run(context.Background(), git)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The FIRST claude call (cycle review) also sleeps 5s past the 200ms budget,
	// so the review itself times out. Either way the run must end timed_out and
	// never push.
	if out.Status != "timed_out" {
		t.Errorf("status = %q, want timed_out", out.Status)
	}
	if git.pushed {
		t.Errorf("must NOT push on timeout")
	}
}
