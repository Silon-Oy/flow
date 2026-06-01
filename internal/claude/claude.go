// Package claude invokes the claude CLI for a single orchestrated step
// (REWRITE of lib/claude-call.sh using os/exec + context.WithTimeout).
//
// Each call runs the CLI headless with a rendered prompt, captures combined
// output, and enforces a hard wall-clock budget via the context. A timeout is
// reported distinctly (ErrTimeout) so the orchestrator can finalize the run as
// timed_out and ramp the budget on restart.
package claude

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"
)

// DefaultTimeout matches lib/claude-call.sh (60 min) — accommodates implementer
// cycles in slower repos. A project can override per-run.
const DefaultTimeout = 60 * time.Minute

// ErrTimeout is returned when the call exceeds its budget.
var ErrTimeout = errors.New("claude: call timed out")

// Runner invokes the claude CLI. The command is configurable so tests can
// substitute a mock binary and production can route through the plan-billing
// npx wrapper.
type Runner struct {
	// Command is the argv prefix, e.g. ["npx","--no-install","@anthropic-ai/claude-code"]
	// or ["claude"] in the baked runner image (R4).
	Command []string
	Timeout time.Duration
}

// New returns a Runner with the default plan-billing command and timeout.
// In the baked runner image the binary is "claude" (see deploy/Dockerfile.orchestrator),
// so production overrides Command accordingly.
func New() *Runner {
	return &Runner{
		Command: []string{"npx", "--no-install", "@anthropic-ai/claude-code"},
		Timeout: DefaultTimeout,
	}
}

// Result captures one claude invocation.
type Result struct {
	Output   string
	ExitCode int
	TimedOut bool
}

// Call runs the claude CLI with the given prompt headlessly and returns the
// combined output. The context bounds the wall-clock; on timeout the process is
// killed and ErrTimeout is returned alongside whatever output was captured.
func (r *Runner) Call(ctx context.Context, prompt string) (Result, error) {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := append([]string{}, r.Command[1:]...)
	args = append(args, "--dangerously-skip-permissions", "-p", prompt, "--output-format", "text")
	cmd := exec.CommandContext(callCtx, r.Command[0], args...)

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	err := cmd.Run()
	out := buf.String()

	if callCtx.Err() == context.DeadlineExceeded {
		return Result{Output: out, ExitCode: 124, TimedOut: true}, ErrTimeout
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return Result{Output: out, ExitCode: exitErr.ExitCode()}, nil
		}
		return Result{Output: out, ExitCode: -1}, err
	}
	return Result{Output: out, ExitCode: 0}, nil
}

// ExtractDecision pulls the last line matching a decision prefix from claude
// output. The orchestrator agents emit a single decision line at the end
// (CYCLE_REVIEW_DECISION: / IMPLEMENTER_RESULT:); only that line is forwarded as
// telemetry (decision rows, not full prompts/diffs — decision 7).
func ExtractDecision(output, prefix string) string {
	lines := strings.Split(output, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	return ""
}
