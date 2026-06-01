// Package orchestrator runs the per-run S1–S12 state machine inside the runner
// (REWRITE of orchestrate.sh). The lock+claim of the bash version is gone — the
// runner already holds a central lease before this runs (§5), so the machine
// starts at worktree creation.
//
// Telemetry is pushed to the central service via the Reporter interface (PATCH
// run + batched events). Per decision 7 only decision lines are forwarded, never
// full prompts/diffs.
package orchestrator

import (
	"context"
	"fmt"

	"github.com/Silon-Oy/flow/internal/claude"
	"github.com/Silon-Oy/flow/internal/envbootstrap"
	"github.com/Silon-Oy/flow/internal/prompts"
	"github.com/Silon-Oy/flow/internal/worktree"
)

// Step identifies a phase of the S1–S12 blueprint. The list is the durable
// contract carried over from orchestrate.sh; the bash mechanics (mkdir lock,
// @me claim, tmux) are NOT carried over.
type Step string

const (
	// Phase A — pick/claim are now the central lease (S1/S2/S3 collapse into
	// the lease the runner already holds when the orchestrator starts).
	StepLeaseHeld     Step = "S1_S3_lease_held"  // lease acquired centrally
	StepWorktree      Step = "S4_worktree"       // create per-run worktree
	StepDBClone       Step = "S5_db_clone"       // optional test-DB clone (opt-in)
	StepCycleReview   Step = "S6_cycle_review"   // cycle-review agent
	StepReviewGate    Step = "S7_review_gate"    // auto/interactive gate
	// Phase B
	StepEnvBootstrap  Step = "S7b_env_bootstrap" // fail-fast dependency install
	StepProvisionEnv  Step = "S7c_provision_env" // opt-in test-env hook
	StepImplementer   Step = "S8_implementer"    // implementer agent
	StepEvolution     Step = "S9_evolution"      // evolution agent
	StepPush          Step = "S10_push"          // push branch (scoped, via proxy)
	StepPR            Step = "S11_pr"            // open PR
	StepFinalize      Step = "S12_finalize"      // finalize run state
)

// Blueprint is the ordered phase list, exported so the runner, dashboard and
// docs share one source of truth for the machine's shape.
var Blueprint = []Step{
	StepLeaseHeld, StepWorktree, StepDBClone, StepCycleReview, StepReviewGate,
	StepEnvBootstrap, StepProvisionEnv, StepImplementer, StepEvolution,
	StepPush, StepPR, StepFinalize,
}

// Reporter is how the orchestrator pushes telemetry to the central service.
// The runner provides an HTTP-backed implementation; tests provide a fake.
type Reporter interface {
	// SetState records the current S-step (PATCH run.current_state).
	SetState(ctx context.Context, step Step) error
	// Event records a single telemetry event (decision lines only, decision 7).
	Event(ctx context.Context, event string, data map[string]string) error
	// Finalize sets the terminal run status + optional reason.
	Finalize(ctx context.Context, status, reason string) error
}

// Config carries the per-run inputs resolved from the lease + project config.
type Config struct {
	RunID       string
	RepoRoot    string // the runner's clone (mounted at /work in the container)
	Remote      string
	Branch      string // auto-run/<remote_label>-<slug>
	BaseBranch  string // optional integration branch
	IssueNumber int
	IssuePrompt string // pre-rendered issue context for the agents

	// AutoMode skips the interactive review gate (RUN_ISSUES_AUTO=1 equivalent).
	AutoMode bool
}

// Orchestrator drives one run.
type Orchestrator struct {
	cfg    Config
	claude *claude.Runner
	report Reporter
}

// New builds an Orchestrator.
func New(cfg Config, c *claude.Runner, r Reporter) *Orchestrator {
	return &Orchestrator{cfg: cfg, claude: c, report: r}
}

// Outcome summarizes a finished run.
type Outcome struct {
	Status    string // mirrors runstate.Status values
	Reason    string
	Branch    string
	PRURL     string
	LastStep  Step
}

// Run executes the S1–S12 machine. It pushes state transitions and decision
// events through the Reporter and returns the terminal Outcome. The side-effect
// steps that need GitHub (S10 push, S11 PR) are wired through gh-shell-out by
// the runner via the GitOps hook; here they are sequenced and gated on the
// lease being held.
func (o *Orchestrator) Run(ctx context.Context, git GitOps) (Outcome, error) {
	out := Outcome{Branch: o.cfg.Branch, LastStep: StepLeaseHeld}

	// S4: worktree.
	o.setState(ctx, StepWorktree)
	wt, err := worktree.Create(o.cfg.RepoRoot, o.cfg.RunID, o.cfg.Branch, o.cfg.BaseBranch, o.cfg.Remote)
	if err != nil {
		return o.fail(ctx, out, StepWorktree, "blocked", "worktree_create_failed: "+err.Error())
	}
	out.LastStep = StepWorktree

	// S6: cycle review.
	o.setState(ctx, StepCycleReview)
	reviewPrompt := RenderPrompt(prompts.CycleReview, map[string]string{
		"ISSUE":  o.cfg.IssuePrompt,
		"BRANCH": o.cfg.Branch,
	})
	reviewRes, err := o.claude.Call(ctx, reviewPrompt)
	if err == claude.ErrTimeout {
		return o.fail(ctx, out, StepCycleReview, "timed_out", "cycle_review_timeout")
	}
	if err != nil {
		return o.fail(ctx, out, StepCycleReview, "blocked", "cycle_review_failed: "+err.Error())
	}
	decision := claude.ExtractDecision(reviewRes.Output, "CYCLE_REVIEW_DECISION:")
	o.event(ctx, "cycle_review_decision", map[string]string{"decision": decision})
	out.LastStep = StepCycleReview

	// S7: review gate.
	switch decision {
	case "NEEDS_CLARIFICATION":
		return o.fail(ctx, out, StepReviewGate, "awaiting_clarification", "needs_clarification")
	case "BLOCK", "BLOCKER":
		return o.fail(ctx, out, StepReviewGate, "blocked", "cycle_review_blocked")
	case "PROCEED":
		// continue
	default:
		if !o.cfg.AutoMode {
			// Interactive mode would surface the decision to a human; in Vaihe 1
			// the runner is auto-mode, so an unknown decision blocks rather than
			// guessing.
			return o.fail(ctx, out, StepReviewGate, "blocked", "cycle_review_unrecognized: "+decision)
		}
	}

	// S7b: env bootstrap (fail-fast dependency install).
	o.setState(ctx, StepEnvBootstrap)
	if err := o.runEnvBootstrap(ctx, wt, git); err != nil {
		return o.fail(ctx, out, StepEnvBootstrap, "blocked", "env_bootstrap_failed: "+err.Error())
	}
	out.LastStep = StepEnvBootstrap

	// S8: implementer.
	o.setState(ctx, StepImplementer)
	implPrompt := RenderPrompt(prompts.Implementer, map[string]string{
		"ISSUE":  o.cfg.IssuePrompt,
		"BRANCH": o.cfg.Branch,
	})
	implRes, err := o.claude.Call(ctx, implPrompt)
	if err == claude.ErrTimeout {
		return o.fail(ctx, out, StepImplementer, "timed_out", "implementer_timeout")
	}
	if err != nil {
		return o.fail(ctx, out, StepImplementer, "blocked", "implementer_failed: "+err.Error())
	}
	implResult := claude.ExtractDecision(implRes.Output, "IMPLEMENTER_RESULT:")
	o.event(ctx, "implementer_result", map[string]string{"result": implResult})
	if implResult == "BLOCKED" {
		return o.fail(ctx, out, StepImplementer, "blocked", "implementer_blocked")
	}
	out.LastStep = StepImplementer

	// S9: evolution (advisory; failure does not block).
	o.setState(ctx, StepEvolution)
	evoPrompt := RenderPrompt(prompts.Evolution, map[string]string{"BRANCH": o.cfg.Branch})
	if _, err := o.claude.Call(ctx, evoPrompt); err != nil && err != claude.ErrTimeout {
		o.event(ctx, "evolution_skipped", map[string]string{"reason": err.Error()})
	}
	out.LastStep = StepEvolution

	// S10: push (scoped git-write through the egress proxy; the proxy injects
	// the credential — the container never holds the token, §11.3).
	o.setState(ctx, StepPush)
	if err := git.Push(ctx, wt, o.cfg.Branch); err != nil {
		return o.fail(ctx, out, StepPush, "blocked", "push_failed: "+err.Error())
	}
	out.LastStep = StepPush

	// S11: open PR.
	o.setState(ctx, StepPR)
	prURL, err := git.OpenPR(ctx, wt, o.cfg.Branch, o.cfg.BaseBranch, o.cfg.IssueNumber)
	if err != nil {
		return o.fail(ctx, out, StepPR, "blocked", "pr_open_failed: "+err.Error())
	}
	out.PRURL = prURL
	out.LastStep = StepPR

	// S12: finalize.
	o.setState(ctx, StepFinalize)
	out.Status = "completed"
	out.LastStep = StepFinalize
	_ = o.report.Finalize(ctx, "completed", "")
	return out, nil
}

// runEnvBootstrap detects the package manager(s) and runs install via the
// GitOps hook (the runner shells out inside the container). Detection is the
// ported pure logic; the install is a side effect.
func (o *Orchestrator) runEnvBootstrap(ctx context.Context, wt string, git GitOps) error {
	pm := envbootstrap.DetectPackageManager(wt)
	composer := envbootstrap.DetectComposer(wt)
	if pm == "" && composer == "" {
		o.event(ctx, "env_bootstrap_skipped", nil)
		return nil
	}
	if composer != "" {
		if err := git.Install(ctx, wt, "composer"); err != nil {
			return fmt.Errorf("composer install: %w", err)
		}
	}
	if pm != "" {
		if err := git.Install(ctx, wt, pm); err != nil {
			return fmt.Errorf("%s install: %w", pm, err)
		}
	}
	return nil
}

func (o *Orchestrator) setState(ctx context.Context, step Step) {
	_ = o.report.SetState(ctx, step)
}

func (o *Orchestrator) event(ctx context.Context, event string, data map[string]string) {
	_ = o.report.Event(ctx, event, data)
}

func (o *Orchestrator) fail(ctx context.Context, out Outcome, step Step, status, reason string) (Outcome, error) {
	out.LastStep = step
	out.Status = status
	out.Reason = reason
	_ = o.report.Finalize(ctx, status, reason)
	return out, nil
}
