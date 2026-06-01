package prwatch

import (
	"encoding/json"
	"fmt"
	"testing"
)

// mk mirrors the bash test helper mk():
//
//	empty label => no labels; empty ci => empty rollup (= green, no checks).
func mk(state, mergeable, mergeState, label, ci string) []byte {
	m := map[string]any{
		"state":            state,
		"mergeable":        mergeable,
		"mergeStateStatus": mergeState,
	}
	if label == "" {
		m["labels"] = []any{}
	} else {
		m["labels"] = []any{map[string]string{"name": label}}
	}
	if ci == "" {
		m["statusCheckRollup"] = []any{}
	} else {
		m["statusCheckRollup"] = []any{map[string]any{"status": "COMPLETED", "conclusion": ci}}
	}
	b, _ := json.Marshal(m)
	return b
}

// Table-driven port of tests/test-pr-watch-decision.sh.
func TestDecide(t *testing.T) {
	const L = "auto-merge"
	cases := []struct {
		name   string
		json   []byte
		res    bool
		expect Decision
	}{
		{"label+green+clean=MERGE", mk("OPEN", "MERGEABLE", "CLEAN", L, "SUCCESS"), false, Merge},
		{"no-label=SKIP_NO_LABEL", mk("OPEN", "MERGEABLE", "CLEAN", "", "SUCCESS"), false, SkipNoLabel},
		{"label+red=WAIT_CI", mk("OPEN", "MERGEABLE", "CLEAN", L, "FAILURE"), false, WaitCI},
		{"label+dirty+resOFF=WAIT", mk("OPEN", "CONFLICTING", "DIRTY", L, "SUCCESS"), false, WaitDirty},
		{"label+behind+resOFF=WAIT", mk("OPEN", "MERGEABLE", "BEHIND", L, "SUCCESS"), false, WaitDirty},
		{"closed=SKIP_CLOSED", mk("MERGED", "MERGEABLE", "CLEAN", L, "SUCCESS"), false, SkipClosed},
		{"blocked=SKIP_BLOCKED", mk("OPEN", "MERGEABLE", "BLOCKED", L, "SUCCESS"), false, SkipBlocked},
		{"label+behind+resON=REBASE", mk("OPEN", "MERGEABLE", "BEHIND", L, "SUCCESS"), true, Rebase},
		{"label+dirty+resON=REBASE", mk("OPEN", "CONFLICTING", "DIRTY", L, "SUCCESS"), true, Rebase},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Decide(tc.json, tc.res, L); got != tc.expect {
				t.Errorf("Decide(%s) = %q, want %q", tc.name, got, tc.expect)
			}
		})
	}

	// Real pending CI check (not empty rollup) -> WAIT_CI.
	pending := []byte(`{"state":"OPEN","mergeable":"MERGEABLE","mergeStateStatus":"CLEAN",
	  "labels":[{"name":"auto-merge"}],"statusCheckRollup":[{"status":"IN_PROGRESS","conclusion":null}]}`)
	if got := Decide(pending, false, L); got != WaitCI {
		t.Errorf("pending CI -> %q, want WAIT_CI", got)
	}
}

// Port of the invariant sweep: MERGE iff (label AND green AND mergeable).
func TestDecideMergeInvariant(t *testing.T) {
	const L = "auto-merge"
	for _, hasLabel := range []bool{false, true} {
		for _, ci := range []string{"SUCCESS", "FAILURE"} {
			for _, ms := range []string{"CLEAN", "DIRTY"} {
				lbl := ""
				if hasLabel {
					lbl = L
				}
				mrg := "MERGEABLE"
				if ms == "DIRTY" {
					mrg = "CONFLICTING"
				}
				j := mk("OPEN", mrg, ms, lbl, ci)
				d := Decide(j, false, L)
				shouldMerge := hasLabel && ci == "SUCCESS" && ms == "CLEAN"
				if shouldMerge && d != Merge {
					t.Errorf("%v should merge but got %q", fmt.Sprintf("label=%v ci=%s ms=%s", hasLabel, ci, ms), d)
				}
				if !shouldMerge && d == Merge {
					t.Errorf("%v should NOT merge but got MERGE", fmt.Sprintf("label=%v ci=%s ms=%s", hasLabel, ci, ms))
				}
			}
		}
	}
}

func TestCIStateOf(t *testing.T) {
	// Empty rollup => GREEN.
	if got := CIStateOf([]byte(`{"statusCheckRollup":[]}`)); got != Green {
		t.Errorf("empty rollup -> %q, want GREEN", got)
	}
	// Legacy StatusContext SUCCESS => GREEN.
	if got := CIStateOf([]byte(`{"statusCheckRollup":[{"state":"SUCCESS"}]}`)); got != Green {
		t.Errorf("StatusContext SUCCESS -> %q, want GREEN", got)
	}
	// Mixed: one failing CheckRun => RED.
	if got := CIStateOf([]byte(`{"statusCheckRollup":[{"status":"COMPLETED","conclusion":"SUCCESS"},{"status":"COMPLETED","conclusion":"FAILURE"}]}`)); got != Red {
		t.Errorf("mixed with failure -> %q, want RED", got)
	}
}
