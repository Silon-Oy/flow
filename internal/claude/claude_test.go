package claude

import (
	"context"
	"runtime"
	"testing"
	"time"
)

// TestExtractDecision: the last matching decision line wins.
func TestExtractDecision(t *testing.T) {
	out := "some log\nCYCLE_REVIEW_DECISION: NEEDS_CLARIFICATION\nmore\nCYCLE_REVIEW_DECISION: PROCEED\n"
	if got := ExtractDecision(out, "CYCLE_REVIEW_DECISION:"); got != "PROCEED" {
		t.Errorf("ExtractDecision = %q, want PROCEED", got)
	}
	if got := ExtractDecision("IMPLEMENTER_RESULT: SUCCESS", "IMPLEMENTER_RESULT:"); got != "SUCCESS" {
		t.Errorf("ExtractDecision = %q, want SUCCESS", got)
	}
	if got := ExtractDecision("no decision here", "IMPLEMENTER_RESULT:"); got != "" {
		t.Errorf("ExtractDecision = %q, want empty", got)
	}
}

// TestCallSuccess: a fast mock command returns its output and exit 0.
func TestCallSuccess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	// Mock "claude": echo a fixed decision line, ignore all flags.
	r := &Runner{
		Command: []string{"/bin/sh", "-c", "echo IMPLEMENTER_RESULT: SUCCESS; exit 0", "--"},
		Timeout: 5 * time.Second,
	}
	res, err := r.Call(context.Background(), "ignored prompt")
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit = %d, want 0", res.ExitCode)
	}
	if got := ExtractDecision(res.Output, "IMPLEMENTER_RESULT:"); got != "SUCCESS" {
		t.Errorf("decision = %q, want SUCCESS", got)
	}
}

// TestCallTimeout: a command that outlives the budget is killed and reported as
// timed out — the load-bearing invariant for the timed_out run status.
func TestCallTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	r := &Runner{
		Command: []string{"/bin/sh", "-c", "sleep 5", "--"},
		Timeout: 100 * time.Millisecond,
	}
	start := time.Now()
	res, err := r.Call(context.Background(), "prompt")
	elapsed := time.Since(start)
	if err != ErrTimeout {
		t.Errorf("err = %v, want ErrTimeout", err)
	}
	if !res.TimedOut {
		t.Errorf("res.TimedOut = false, want true")
	}
	if elapsed > 2*time.Second {
		t.Errorf("timeout not enforced promptly: %v", elapsed)
	}
}
