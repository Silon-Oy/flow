// Package prwatch ports the PR classification + merge-decision logic from
// lib/pr-watch-lib.sh (pr_decide / pr_ci_state).
//
// Pure decision rules over a `gh pr view` JSON payload, unit-testable with no
// GitHub access and no side effects.
package prwatch

import "encoding/json"

// Decision is one of the merge-decision tokens emitted by Decide.
type Decision string

const (
	// Merge: all three gates pass (label present AND CI green AND mergeable).
	Merge Decision = "MERGE"
	// SkipNoLabel: the merge label is absent (nothing to do).
	SkipNoLabel Decision = "SKIP_NO_LABEL"
	// SkipClosed: PR is not OPEN (merged/closed).
	SkipClosed Decision = "SKIP_CLOSED"
	// WaitCI: label present but checks are pending/failing.
	WaitCI Decision = "WAIT_CI"
	// WaitDirty: label present, BEHIND/DIRTY, conflict resolution OFF.
	WaitDirty Decision = "WAIT_DIRTY"
	// Rebase: label present, BEHIND/DIRTY, conflict resolution ON.
	Rebase Decision = "REBASE"
	// SkipBlocked: mergeStateStatus BLOCKED (e.g. required review missing).
	SkipBlocked Decision = "SKIP_BLOCKED"
)

// CIState is one of GREEN / RED / PENDING.
type CIState string

const (
	Green   CIState = "GREEN"
	Red     CIState = "RED"
	Pending CIState = "PENDING"
)

// prView mirrors the subset of `gh pr view --json
// state,mergeable,mergeStateStatus,labels,statusCheckRollup` the decision uses.
type prView struct {
	State            string         `json:"state"`
	Mergeable        string         `json:"mergeable"`
	MergeStateStatus string         `json:"mergeStateStatus"`
	Labels           []label        `json:"labels"`
	StatusCheckRollup []rollupEntry `json:"statusCheckRollup"`
}

type label struct {
	Name string `json:"name"`
}

// rollupEntry covers both shapes GitHub returns:
//
//	CheckRun:      {status, conclusion}   (GitHub Actions / checks API)
//	StatusContext: {state}                (legacy commit statuses)
//
// We distinguish them by presence of "conclusion" — mirroring jq's has().
type rollupEntry struct {
	Status     *string `json:"status"`
	Conclusion *string `json:"conclusion"`
	State      *string `json:"state"`
}

func (e rollupEntry) isCheckRun() bool { return e.Conclusion != nil || e.Status != nil }

// Decide reads a gh-pr-view JSON document and returns exactly one Decision.
//
// enableConflictResolution allows REBASE instead of WAIT_DIRTY.
// mergeLabel defaults to "auto-merge" when empty.
//
// INVARIANT: Decide NEVER returns Merge unless label AND CI-green AND mergeable
// are all true together.
func Decide(viewJSON []byte, enableConflictResolution bool, mergeLabel string) Decision {
	if mergeLabel == "" {
		mergeLabel = "auto-merge"
	}
	var pv prView
	// A malformed payload degrades to "wait" rather than panicking; the bash
	// equivalent would see empty fields from jq and fall through to WAIT_CI.
	_ = json.Unmarshal(viewJSON, &pv)

	if pv.State != "OPEN" {
		return SkipClosed
	}

	hasLabel := false
	for _, l := range pv.Labels {
		if l.Name == mergeLabel {
			hasLabel = true
			break
		}
	}
	if !hasLabel {
		return SkipNoLabel
	}

	if ciState(pv) != Green {
		return WaitCI
	}

	switch pv.MergeStateStatus {
	case "CLEAN", "HAS_HOOKS", "UNSTABLE":
		// UNSTABLE = mergeable but a non-required check failed; the CI gate
		// above already validated required checks.
		if pv.Mergeable == "MERGEABLE" {
			return Merge
		}
		return WaitCI
	case "BEHIND", "DIRTY":
		if enableConflictResolution {
			return Rebase
		}
		return WaitDirty
	case "BLOCKED":
		return SkipBlocked
	default:
		// UNKNOWN or anything GitHub has not finished computing — wait.
		return WaitCI
	}
}

// CIStateOf computes GREEN / RED / PENDING from a gh-pr-view JSON document.
// Exposed for callers/tests that want the CI gate in isolation.
func CIStateOf(viewJSON []byte) CIState {
	var pv prView
	_ = json.Unmarshal(viewJSON, &pv)
	return ciState(pv)
}

// ciState normalises both rollup shapes. An empty rollup => GREEN (nothing to
// gate on).
func ciState(pv prView) CIState {
	if len(pv.StatusCheckRollup) == 0 {
		return Green
	}
	sawPending := false
	for _, e := range pv.StatusCheckRollup {
		var s string
		if e.Conclusion != nil || e.Status != nil {
			// CheckRun: pending while status != COMPLETED.
			status := ""
			if e.Status != nil {
				status = *e.Status
			}
			if status != "COMPLETED" {
				s = "PENDING"
			} else {
				concl := ""
				if e.Conclusion != nil {
					concl = *e.Conclusion
				}
				switch concl {
				case "SUCCESS", "NEUTRAL", "SKIPPED":
					s = "OK"
				default:
					s = "BAD"
				}
			}
		} else {
			// StatusContext: state in SUCCESS/PENDING/FAILURE/ERROR.
			state := ""
			if e.State != nil {
				state = *e.State
			}
			switch state {
			case "SUCCESS":
				s = "OK"
			case "PENDING":
				s = "PENDING"
			default:
				s = "BAD"
			}
		}
		switch s {
		case "BAD":
			return Red
		case "PENDING":
			sawPending = true
		}
	}
	if sawPending {
		return Pending
	}
	return Green
}
