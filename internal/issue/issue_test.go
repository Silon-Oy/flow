package issue

import (
	"strings"
	"testing"
)

// Port of tests/test-truncate-marker.sh truncate cases.
func TestTruncateForGitHub(t *testing.T) {
	// Short input passes through unchanged, no notice.
	short := "line1\nline2\nIMPLEMENTER_RESULT: SUCCESS"
	if got := TruncateForGitHub(short, 60000); got != short {
		t.Errorf("short input altered: %q", got)
	}
	if strings.Contains(TruncateForGitHub(short, 60000), "typistetty") {
		t.Errorf("short input got a truncation notice")
	}

	// Long input keeps tail + notice, stays under cap, decision line survives.
	big := strings.Repeat("x", 5000)
	long := big + "\n" + big + "\nCYCLE_REVIEW_DECISION: PROCEED"
	const max = 2000
	out := TruncateForGitHub(long, max)
	if len(out) > max {
		t.Errorf("truncated output %d > cap %d", len(out), max)
	}
	if !strings.Contains(out, "typistetty") {
		t.Errorf("long input missing truncation notice")
	}
	if !strings.Contains(out, "CYCLE_REVIEW_DECISION: PROCEED") {
		t.Errorf("decision line (tail) was dropped")
	}
	firstLine := strings.SplitN(out, "\n", 2)[0]
	if !strings.Contains(firstLine, "typistetty") {
		t.Errorf("notice not on first line: %q", firstLine)
	}
}

// Port of tests/test-image-extraction.sh.
func TestExtractImageURLs(t *testing.T) {
	const dup = "https://github.com/user-attachments/assets/dup-1111-2222"

	// Build the fixture inline, mirroring the bash jq -n construction.
	fixture := []byte(`{
		"title": "t",
		"body": "Korjaa tämä, ks. kuvakaappaus:\n![screenshot](https://github.com/user-attachments/assets/aaaa-bbbb)\nMockup HTML:nä: <img alt=\"m\" src=\"https://user-images.githubusercontent.com/1/cccc.png\">\nDiagrammi markdownina, jossa otsikko: ![d](https://example.com/diagram.png \"otsikko\")\nTavallinen linkki EI ole kuva: [docs](https://example.com/readme.md)\nSama kuva myös kommentissa: ` + dup + `\n",
		"comments": [
			{ "body": "Tässä lisäkuva: <img src='https://github.com/user-attachments/assets/eeee-ffff'/>\nja sama kuin bodyssä: ` + dup + `" },
			{ "body": "<!-- run-issues:awaiting-answer --> ![bot](https://github.com/user-attachments/assets/9999-bot)" }
		]
	}`)

	urls, err := ExtractImageURLs(fixture)
	if err != nil {
		t.Fatalf("ExtractImageURLs error: %v", err)
	}
	set := map[string]int{}
	for _, u := range urls {
		set[u]++
	}

	want := []string{
		"https://github.com/user-attachments/assets/aaaa-bbbb", // markdown image (body)
		"https://user-images.githubusercontent.com/1/cccc.png", // html img (body)
		"https://example.com/diagram.png",                      // markdown image with title
		"https://github.com/user-attachments/assets/eeee-ffff", // html img (comment, single-quoted)
		dup, // duplicate across body + comment
	}
	for _, w := range want {
		if set[w] == 0 {
			t.Errorf("missing expected URL: %s", w)
		}
	}

	reject := []string{
		"https://example.com/readme.md",                        // non-image markdown link
		"https://github.com/user-attachments/assets/9999-bot",  // bot-comment image skipped
		"otsikko",                                              // markdown title must not leak
	}
	for _, r := range reject {
		if set[r] != 0 {
			t.Errorf("should not contain: %s", r)
		}
	}

	if set[dup] != 1 {
		t.Errorf("duplicate url count=%d (expected 1)", set[dup])
	}

	// No images -> empty.
	none := []byte(`{ "title": "t", "body": "Pelkkää tekstiä, ei kuvia. [linkki](https://x.test/a.md)", "comments": [] }`)
	urls2, err := ExtractImageURLs(none)
	if err != nil {
		t.Fatal(err)
	}
	if len(urls2) != 0 {
		t.Errorf("expected empty output, got %v", urls2)
	}
}
