package orchestrator

import "testing"

// Port of lib/render-prompt.test.sh intent: single-pass substitution, injection
// safety (a substituted value containing {{OTHER}} is NOT re-scanned), unknown
// placeholders left verbatim.
func TestRenderPrompt(t *testing.T) {
	tmpl := "Issue: {{ISSUE}} on branch {{BRANCH}}. Unknown: {{NOPE}}."
	got := RenderPrompt(tmpl, map[string]string{
		"ISSUE":  "fix {{BRANCH}} bug", // contains a placeholder-looking literal
		"BRANCH": "auto-run/issue-1",
	})
	want := "Issue: fix {{BRANCH}} bug on branch auto-run/issue-1. Unknown: {{NOPE}}."
	if got != want {
		t.Errorf("RenderPrompt:\n got=%q\nwant=%q", got, want)
	}
}

func TestRenderPromptEmptyValues(t *testing.T) {
	if got := RenderPrompt("{{A}}{{B}}", map[string]string{"A": "", "B": "x"}); got != "x" {
		t.Errorf("got %q, want x", got)
	}
}
