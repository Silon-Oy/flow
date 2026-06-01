// Package clarify ports the clarification-loop helpers from lib/issue.sh:
// build_marker / parse_marker / detect_answer.
//
// The clarification loop posts an awaiting-answer marker (an invisible HTML
// comment) on the issue, then later detects a human reply by TIMESTAMP — the
// bot and the human may share the same GitHub account, so author is not a
// usable discriminator. The marker ts and GitHub's createdAt are both in the
// same `%FT%TZ` (Zulu, no offset, no fractional seconds) format, so a
// lexicographic string comparison is a chronological comparison.
package clarify

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// BuildMarker emits an HTML comment marker that uniquely identifies an
// awaiting-answer situation comment. Invisible in rendered GitHub markdown.
func BuildMarker(runID string, issueNum int, ts string, round int) string {
	return fmt.Sprintf("<!-- run-issues:awaiting-answer run=%s issue=%d ts=%s round=%d -->",
		runID, issueNum, ts, round)
}

// Marker is the parsed result of the newest awaiting-answer marker.
type Marker struct {
	Run   string
	TS    string
	Round string
}

var reMarker = regexp.MustCompile(
	`<!-- run-issues:awaiting-answer run=([^ ]+) issue=[^ ]+ ts=([^ ]+) round=([^ ]+) -->`)

type clarifyDoc struct {
	Comments []clarifyComment `json:"comments"`
}

type clarifyComment struct {
	Body      string `json:"body"`
	CreatedAt string `json:"createdAt"`
}

// ParseMarker scans the issue comments for the NEWEST
// run-issues:awaiting-answer marker (newest decided by the marker's own ts=
// field) and returns it. ok is false when no marker is present.
func ParseMarker(issueJSON []byte) (Marker, bool, error) {
	var doc clarifyDoc
	if err := json.Unmarshal(issueJSON, &doc); err != nil {
		return Marker{}, false, err
	}
	var markers []Marker
	for _, c := range doc.Comments {
		// A comment may carry the marker anywhere in its body; find all.
		for _, m := range reMarker.FindAllStringSubmatch(c.Body, -1) {
			markers = append(markers, Marker{Run: m[1], TS: m[2], Round: m[3]})
		}
	}
	if len(markers) == 0 {
		return Marker{}, false, nil
	}
	sort.SliceStable(markers, func(i, j int) bool { return markers[i].TS < markers[j].TS })
	return markers[len(markers)-1], true, nil
}

// DetectAnswer returns the body of the NEWEST comment created strictly after
// markerTS that is NOT itself a run-issues bot comment (body does not contain
// "run-issues:"). Empty output means "no human reply yet". The reply is capped
// to maxChars to keep the downstream prompt bounded (0 => default 8000).
func DetectAnswer(issueJSON []byte, markerTS string, maxChars int) (string, error) {
	if maxChars <= 0 {
		maxChars = 8000
	}
	var doc clarifyDoc
	if err := json.Unmarshal(issueJSON, &doc); err != nil {
		return "", err
	}
	var candidates []clarifyComment
	for _, c := range doc.Comments {
		if c.CreatedAt <= markerTS {
			continue
		}
		if containsRunIssues(c.Body) {
			continue
		}
		candidates = append(candidates, c)
	}
	if len(candidates) == 0 {
		return "", nil
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].CreatedAt < candidates[j].CreatedAt
	})
	newest := candidates[len(candidates)-1].Body
	if len(newest) > maxChars {
		// Cap by rune count to mirror jq's character (not byte) slice.
		r := []rune(newest)
		if len(r) > maxChars {
			newest = string(r[:maxChars])
		}
	}
	return newest, nil
}

func containsRunIssues(s string) bool {
	return strings.Contains(s, "run-issues:")
}
