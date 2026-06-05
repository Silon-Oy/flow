package main

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseRemoteLine locks the interactive remote-line grammar used by
// `flowctl init`. The grammar is documented in init.go and the test is the
// single readable spec of every accepted shape.
func TestParseRemoteLine(t *testing.T) {
	cases := []struct {
		name       string
		line       string
		wantRemote string
		wantOwner  string
		wantBranch string
		wantErr    bool
	}{
		{"plain", "origin=Silon-Oy/flow", "origin", "Silon-Oy/flow", "", false},
		{"with_branch", "upstream=Silon-Oy/flow@develop", "upstream", "Silon-Oy/flow", "develop", false},
		{"whitespace", "  origin = Silon-Oy/flow @ main ", "origin", "Silon-Oy/flow", "main", false},
		{"no_equals", "origin", "", "", "", true},
		{"no_owner", "origin=flow", "", "", "", true},
		{"trailing_equals", "origin=", "", "", "", true},
		{"leading_equals", "=Silon-Oy/flow", "", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseRemoteLine(tc.line)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Remote != tc.wantRemote {
				t.Errorf("remote = %q, want %q", got.Remote, tc.wantRemote)
			}
			if got.OwnerRepo != tc.wantOwner {
				t.Errorf("owner_repo = %q, want %q", got.OwnerRepo, tc.wantOwner)
			}
			if got.BaseBranch != tc.wantBranch {
				t.Errorf("base_branch = %q, want %q", got.BaseBranch, tc.wantBranch)
			}
		})
	}
}

// TestLoadProjectConfig proves --config flow.yaml deserialises to the wire
// shape with every §8 field populated. Includes the per-remote base_branch
// override (päätös 14) so a regression in YAML mapping fails this test.
func TestLoadProjectConfig(t *testing.T) {
	yaml := `name: flow
owner_repo: Silon-Oy/flow
base_branch: main
labels:
  - auto-run
  - urgent
remotes:
  - remote: origin
    owner_repo: Silon-Oy/flow
  - remote: upstream
    owner_repo: Acme/flow
    base_branch: develop
claude_timeout_seconds: 3600
merge_policy:
  label: ready-to-merge
  conflict_strategy: abort
secret_refs:
  GH_TOKEN: github-token-key
  DB_URL_TEST: db-test-key
`
	dir := t.TempDir()
	path := filepath.Join(dir, "flow.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	req, err := loadProjectConfig(path)
	if err != nil {
		t.Fatalf("loadProjectConfig: %v", err)
	}
	if req.Name != "flow" {
		t.Errorf("name = %q", req.Name)
	}
	if req.OwnerRepo != "Silon-Oy/flow" {
		t.Errorf("owner_repo = %q", req.OwnerRepo)
	}
	if req.BaseBranch != "main" {
		t.Errorf("base_branch = %q", req.BaseBranch)
	}
	if len(req.Labels) != 2 || req.Labels[0] != "auto-run" {
		t.Errorf("labels = %v", req.Labels)
	}
	if len(req.Remotes) != 2 {
		t.Fatalf("remotes = %v", req.Remotes)
	}
	if req.Remotes[1].Remote != "upstream" || req.Remotes[1].OwnerRepo != "Acme/flow" || req.Remotes[1].BaseBranch != "develop" {
		t.Errorf("remotes[1] = %+v", req.Remotes[1])
	}
	if req.ClaudeTimeoutSeconds != 3600 {
		t.Errorf("claude_timeout_seconds = %d", req.ClaudeTimeoutSeconds)
	}
	if req.MergePolicy["label"] != "ready-to-merge" {
		t.Errorf("merge_policy = %v", req.MergePolicy)
	}
	if req.SecretRefs["GH_TOKEN"] != "github-token-key" {
		t.Errorf("secret_refs = %v", req.SecretRefs)
	}
}

// TestLoadProjectConfig_UnknownField fails on a typo so the CI doesn't
// silently drop a misspelled field on the floor.
func TestLoadProjectConfig_UnknownField(t *testing.T) {
	yaml := `name: flow
owner_repo: Silon-Oy/flow
mispelled_field: oops
`
	dir := t.TempDir()
	path := filepath.Join(dir, "flow.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadProjectConfig(path); err == nil {
		t.Fatalf("expected unknown-field error, got nil")
	}
}

// TestParseOwnerRepoFromURL covers the URL-shape diversity `git remote
// get-url origin` may emit on a real repo. The prompt's default suggestion
// breaks if any of these regress.
func TestParseOwnerRepoFromURL(t *testing.T) {
	cases := map[string]string{
		"git@github.com:Silon-Oy/flow.git":      "Silon-Oy/flow",
		"git@github.com:Silon-Oy/flow":          "Silon-Oy/flow",
		"https://github.com/Silon-Oy/flow.git":  "Silon-Oy/flow",
		"https://github.com/Silon-Oy/flow":      "Silon-Oy/flow",
		"https://x-token-auth:abc@github.com/Silon-Oy/flow.git": "Silon-Oy/flow",
		"not a url":                             "",
	}
	for in, want := range cases {
		if got := parseOwnerRepoFromURL(in); got != want {
			t.Errorf("parseOwnerRepoFromURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestPromptSecretRefs_RejectsTokenShape proves the CLI fails fast on a
// plaintext token value — mirrors the central's hasSecretLikePrefix check.
// The architecture invariant ("secrets are references, not values") would
// otherwise survive only as a server-side rejection.
func TestPromptSecretRefs_RejectsTokenShape(t *testing.T) {
	// Three lines: a token-shaped value (refused), a legitimate key (kept),
	// and a blank line to terminate the loop.
	input := "GH_TOKEN=ghp_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\nGH_TOKEN=github-token-key\n\n"
	br := bufio.NewReader(strings.NewReader(input))
	var out bytes.Buffer
	refs, err := promptSecretRefs(br, &out)
	if err != nil {
		t.Fatalf("promptSecretRefs: %v", err)
	}
	if refs["GH_TOKEN"] != "github-token-key" {
		t.Errorf("expected the second (key) attempt to be kept, got %v", refs)
	}
	if !strings.Contains(out.String(), "refused") {
		t.Errorf("expected stdout to mention the refusal, got %q", out.String())
	}
}
