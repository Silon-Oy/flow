package orchestrator

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Silon-Oy/flow/internal/issue"
)

func TestIssueBodyText(t *testing.T) {
	tests := []struct {
		title, body, want string
	}{
		{"", "", ""},
		{"Only title", "", "# Only title"},
		{"", "Only body\n", "Only body"},
		{"  Trimmed  ", "body line", "# Trimmed\n\nbody line"},
	}
	for _, tc := range tests {
		if got := issueBodyText(tc.title, tc.body); got != tc.want {
			t.Errorf("issueBodyText(%q,%q) = %q, want %q", tc.title, tc.body, got, tc.want)
		}
	}
}

func TestRenderComments(t *testing.T) {
	if got := renderComments(nil); !strings.Contains(got, "Ei kommentteja") {
		t.Errorf("empty comments produced %q", got)
	}
	got := renderComments([]IssueComment{
		{Author: "alice", Body: "first\n"},
		{Author: "", Body: "anon"},
	})
	for _, want := range []string{"**@alice:**", "first", "**@(tuntematon):**", "anon", "---"} {
		if !strings.Contains(got, want) {
			t.Errorf("renderComments missing %q in:\n%s", want, got)
		}
	}
}

func TestRenderImages(t *testing.T) {
	const wt = "/tmp/wt"
	// No URLs at all -> empty section so the prompt doesn't sprout a stray
	// heading.
	if got := renderImages(wt, nil, nil); got != "" {
		t.Errorf("empty input must produce empty string, got %q", got)
	}

	// URLs known but download dir setup failed (results==nil) -> fallback list.
	fallback := renderImages(wt, nil, []string{"https://example.com/a.png"})
	if !strings.Contains(fallback, "https://example.com/a.png") {
		t.Errorf("fallback block missing URL:\n%s", fallback)
	}
	if !strings.Contains(fallback, "lataus epäonnistui") {
		t.Errorf("fallback block missing failure notice:\n%s", fallback)
	}

	// Mixed success + failure: success path becomes a worktree-relative ref,
	// failure surfaces the original URL.
	results := []issue.DownloadResult{
		{URL: "https://example.com/a.png", Path: filepath.Join(wt, ".flow/issue-images/00.png")},
		{URL: "https://example.com/b.png", Err: errLike("http 404")},
	}
	got := renderImages(wt, results, []string{"https://example.com/a.png", "https://example.com/b.png"})
	if !strings.Contains(got, "`.flow/issue-images/00.png`") {
		t.Errorf("missing relative path: %s", got)
	}
	if strings.Contains(got, wt+"/") {
		t.Errorf("absolute worktree path leaked: %s", got)
	}
	if !strings.Contains(got, "lataus epäonnistui:** https://example.com/b.png") {
		t.Errorf("failed URL not surfaced: %s", got)
	}
}

type stubErr string

func (s stubErr) Error() string { return string(s) }

func errLike(s string) error { return stubErr(s) }
