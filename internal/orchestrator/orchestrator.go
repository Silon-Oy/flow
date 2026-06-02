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
	"log"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Silon-Oy/flow/internal/claude"
	"github.com/Silon-Oy/flow/internal/envbootstrap"
	"github.com/Silon-Oy/flow/internal/issue"
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

// IssueComment is one issue comment as it enters the agent prompt.
type IssueComment struct {
	Author string
	Body   string
}

// Config carries the per-run inputs resolved from the lease + project config.
//
// The issue context is split into granular fields (title/body/comments/images)
// so the orchestrator can fill the placeholders the prompt templates actually
// declare ({{ISSUE_TITLE}}, {{ISSUE_BODY}}, {{ISSUE_COMMENTS}}, {{ISSUE_IMAGES}},
// …) rather than dumping a single pre-rendered blob.
type Config struct {
	RunID       string
	RepoRoot    string // the runner's clone (host path used to create the worktree)
	Remote      string
	Branch      string // auto-run/<remote_label>-<slug>
	BaseBranch  string // optional integration branch
	IssueNumber int

	IssueTitle     string         // pre-rendered short title
	IssueBody      string         // raw issue body (markdown ok)
	IssueComments  []IssueComment // chronological, excluding run-issues:* bot markers
	IssueImageURLs []string       // image URLs extracted from body + comments; downloaded after S4

	// WorktreePath, when non-empty, is the already-populated worktree directory
	// the orchestrator should use as-is (skips S4 worktree.Create). This is the
	// container-mode path: the host creates the worktree at run time and mounts
	// it as /work; the in-container orchestrator gets /work here so it does not
	// try to git-worktree-add from inside a single mount. In in-process mode the
	// field stays empty and the orchestrator creates the worktree itself.
	WorktreePath string

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

	// S4: worktree. In container mode the host has already created and mounted
	// the worktree (so the orchestrator sees a populated dir under a single mount
	// — `git worktree add` from inside that mount cannot work). The host hands the
	// path through Config.WorktreePath; we record S4 and use it as-is.
	o.setState(ctx, StepWorktree)
	var wt string
	if o.cfg.WorktreePath != "" {
		wt = o.cfg.WorktreePath
	} else {
		created, err := worktree.Create(o.cfg.RepoRoot, o.cfg.RunID, o.cfg.Branch, o.cfg.BaseBranch, o.cfg.Remote)
		if err != nil {
			return o.fail(ctx, out, StepWorktree, "blocked", "worktree_create_failed: "+err.Error())
		}
		wt = created
	}
	out.LastStep = StepWorktree

	// Download issue images into the worktree (best-effort). The worktree path
	// is the only host mount handed to the per-run container (§11.1), so images
	// MUST live inside it or the agent can't read them. Per-URL failures don't
	// abort the run — agents can still process the issue body.
	imageReport := o.downloadIssueImages(ctx, wt)

	promptValues := o.basePromptValues(wt, imageReport)

	// S6: cycle review.
	o.setState(ctx, StepCycleReview)
	reviewPrompt := RenderPrompt(prompts.CycleReview, promptValues)
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
	implValues := cloneStringMap(promptValues)
	implValues["CYCLE_REVIEW_OUTPUT"] = reviewRes.Output
	implPrompt := RenderPrompt(prompts.Implementer, implValues)
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
	evoValues := cloneStringMap(promptValues)
	evoValues["IMPLEMENTER_OUTPUT_TAIL"] = implRes.Output
	evoPrompt := RenderPrompt(prompts.Evolution, evoValues)
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

// issueImagesSubdir is the worktree-relative directory where downloaded issue
// images land. Centralised so the prompt-rendering helper and the downloader
// agree on one path.
const issueImagesSubdir = ".flow/issue-images"

// downloadIssueImages downloads every IssueImageURL into <wt>/.flow/issue-images/.
// Errors are best-effort — a failed download is recorded in the result and the
// run continues. Returns the per-URL results so the prompt can list both
// successes (with disk paths) and failures (with the original URL).
func (o *Orchestrator) downloadIssueImages(ctx context.Context, wt string) []issue.DownloadResult {
	if len(o.cfg.IssueImageURLs) == 0 {
		return nil
	}
	destDir := filepath.Join(wt, issueImagesSubdir)
	results, err := issue.DownloadImages(ctx, destDir, o.cfg.IssueImageURLs)
	if err != nil {
		log.Printf("orchestrator: issue-image download dir setup: %v", err)
		o.event(ctx, "issue_images_dir_failed", map[string]string{"reason": err.Error()})
		return nil
	}
	ok, failed := 0, 0
	for _, r := range results {
		if r.Err == nil {
			ok++
		} else {
			failed++
			log.Printf("orchestrator: issue-image download failed: %s: %v", r.URL, r.Err)
		}
	}
	o.event(ctx, "issue_images_downloaded", map[string]string{
		"ok":     strconv.Itoa(ok),
		"failed": strconv.Itoa(failed),
	})
	return results
}

// basePromptValues builds the placeholder map shared across all S6/S8/S9
// prompts. Each step augments the returned map with step-specific keys
// (CYCLE_REVIEW_OUTPUT, IMPLEMENTER_OUTPUT_TAIL); placeholders the templates
// declare but this run does not provide (REPO_CLAUDE_MD, CLARIFICATION_CONTEXT,
// RESTART_CONTEXT, RUN_ISSUES_DB_CLONE) get empty defaults so the rendered
// prompt has no literal "{{KEY}}" leakage.
func (o *Orchestrator) basePromptValues(wt string, images []issue.DownloadResult) map[string]string {
	return map[string]string{
		"REPO_ROOT":     o.cfg.RepoRoot,
		"WORKTREE_PATH": wt,
		"BRANCH":        o.cfg.Branch,

		"ISSUE_NUMBER":   strconv.Itoa(o.cfg.IssueNumber),
		"ISSUE_TITLE":    o.cfg.IssueTitle,
		"ISSUE_BODY":     issueBodyText(o.cfg.IssueTitle, o.cfg.IssueBody),
		"ISSUE_COMMENTS": renderComments(o.cfg.IssueComments),
		"ISSUE_IMAGES":   renderImages(wt, images, o.cfg.IssueImageURLs),

		// Defaults for placeholders this step doesn't supply — keeps the rendered
		// prompt clean instead of leaving literal "{{KEY}}" strings.
		"RUN_ISSUES_DB_CLONE":     "",
		"REPO_CLAUDE_MD":          "",
		"CLARIFICATION_CONTEXT":   "",
		"CYCLE_REVIEW_OUTPUT":     "",
		"RESTART_CONTEXT":         "",
		"IMPLEMENTER_OUTPUT_TAIL": "",
	}
}

// issueBodyText is the value substituted into {{ISSUE_BODY}}: title on the
// first line then a blank line then the body, mirroring what a GitHub issue
// renders. If only the body is set it's used as-is.
func issueBodyText(title, body string) string {
	title = strings.TrimSpace(title)
	body = strings.TrimRight(body, "\n")
	switch {
	case title == "" && body == "":
		return ""
	case title == "":
		return body
	case body == "":
		return "# " + title
	}
	return "# " + title + "\n\n" + body
}

// renderComments produces the human-readable block for {{ISSUE_COMMENTS}}.
// Each non-bot comment becomes a labelled section; an empty list yields a
// short Finnish "no comments" notice so the rendered prompt section isn't
// blank.
func renderComments(cs []IssueComment) string {
	if len(cs) == 0 {
		return "_Ei kommentteja._"
	}
	var b strings.Builder
	for i, c := range cs {
		if i > 0 {
			b.WriteString("\n\n---\n\n")
		}
		author := strings.TrimSpace(c.Author)
		if author == "" {
			author = "(tuntematon)"
		}
		fmt.Fprintf(&b, "**@%s:**\n\n%s", author, strings.TrimRight(c.Body, "\n"))
	}
	return b.String()
}

// renderImages produces the {{ISSUE_IMAGES}} block. Successful downloads point
// at their worktree-relative path so the agent can Read them directly; failed
// downloads list the URL so the agent at least knows what was referenced.
func renderImages(wt string, results []issue.DownloadResult, fallbackURLs []string) string {
	if len(results) == 0 && len(fallbackURLs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Issue-kuvat\n\n")
	if len(results) == 0 {
		// Image-extraction listed URLs but none were downloaded (e.g. mkdir
		// failed); fall back to URL-only refs.
		b.WriteString("Linkit (lataus epäonnistui — käytä URL:ää suoraan):\n")
		for _, u := range fallbackURLs {
			fmt.Fprintf(&b, "- %s\n", u)
		}
		return strings.TrimRight(b.String(), "\n")
	}
	fmt.Fprintf(&b, "Ladattu hakemistoon `%s/`:\n", issueImagesSubdir)
	for _, r := range results {
		if r.Err == nil {
			rel := r.Path
			if r := strings.TrimPrefix(rel, wt+string(filepath.Separator)); r != rel {
				rel = r
			}
			fmt.Fprintf(&b, "- `%s` (alkuperä: %s)\n", rel, r.URL)
		} else {
			fmt.Fprintf(&b, "- **lataus epäonnistui:** %s (%v)\n", r.URL, r.Err)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func cloneStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
