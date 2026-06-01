package clarify

import (
	"strings"
	"testing"
)

// Port of tests/test-truncate-marker.sh build_marker round-trip.
func TestBuildMarker(t *testing.T) {
	m := BuildMarker("20260521-1500-issue-99", 99, "2026-05-21T15:00:00Z", 0)
	for _, want := range []string{
		"run=20260521-1500-issue-99",
		"issue=99",
		"ts=2026-05-21T15:00:00Z",
		"round=0",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("marker missing %q: %s", want, m)
		}
	}
	if !strings.HasPrefix(m, "<!-- run-issues:awaiting-answer ") {
		t.Errorf("marker prefix wrong: %s", m)
	}
	if !strings.HasSuffix(m, " -->") {
		t.Errorf("marker suffix wrong: %s", m)
	}

	m3 := BuildMarker("rid", 7, "2026-01-01T00:00:00Z", 3)
	if !strings.Contains(m3, "round=3") {
		t.Errorf("explicit round not honoured: %s", m3)
	}
}

// Port of tests/test-answer-detection.sh.
func TestParseMarkerAndDetectAnswer(t *testing.T) {
	const markerTS = "2026-05-21T10:00:00Z"
	const runID = "20260521-095900-issue-7"
	marker := BuildMarker(runID, 7, markerTS, 1)

	fixture := []byte(`{
		"title": "t",
		"body": "b",
		"comments": [
			{ "createdAt": "2026-05-21T10:00:00Z", "body": ` + jsonStr(marker+"\n## /run-issues — tarkennus tarvitaan") + ` },
			{ "createdAt": "2026-05-21T10:00:01Z", "body": "<!-- run-issues:noise --> bottikommentti" },
			{ "createdAt": "2026-05-21T09:59:00Z", "body": "vanha kommentti ennen markeria" },
			{ "createdAt": "2026-05-21T10:05:00Z", "body": "Tarkennus C: käytä Postgresia, ei MySQL:ää." },
			{ "createdAt": "2026-05-21T10:10:00Z", "body": "Tarkennus D (uudempi): itse asiassa SQLite riittää." }
		]
	}`)

	pm, ok, err := ParseMarker(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("parse_marker found no marker")
	}
	if pm.TS != markerTS {
		t.Errorf("parse_marker ts = %q, want %q", pm.TS, markerTS)
	}
	if pm.Round != "1" {
		t.Errorf("parse_marker round = %q, want 1", pm.Round)
	}
	if pm.Run != runID {
		t.Errorf("parse_marker run = %q, want %q", pm.Run, runID)
	}

	ans, err := DetectAnswer(fixture, markerTS, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ans, "SQLite riittää") {
		t.Errorf("detect_answer did not pick newest reply D: %q", ans)
	}
	if strings.Contains(ans, "Postgresia") {
		t.Errorf("detect_answer returned older reply C instead of D")
	}
	if strings.Contains(ans, "run-issues:") {
		t.Errorf("detect_answer returned a bot marker comment")
	}
	if strings.Contains(ans, "ennen markeria") {
		t.Errorf("detect_answer returned a pre-marker comment")
	}

	// No reply yet (race) -> empty.
	noReply := []byte(`{
		"title": "t", "body": "b",
		"comments": [
			{ "createdAt": "2026-05-21T10:00:00Z", "body": ` + jsonStr(marker+"\n## tarkennus") + ` }
		]
	}`)
	ans2, err := DetectAnswer(noReply, markerTS, 0)
	if err != nil {
		t.Fatal(err)
	}
	if ans2 != "" {
		t.Errorf("detect_answer returned non-empty when no human reply: %q", ans2)
	}
}

// jsonStr quotes a string as a JSON string literal for fixture embedding.
func jsonStr(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
