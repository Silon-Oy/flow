package gitremote

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Table-driven port of tests/test-multi-remote.sh section (a).
func TestParseOwnerRepoFromRemoteURL(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want string
	}{
		{"ssh scp+.git", "git@github.com:Silon-Oy/wisol-map-api.git", "Silon-Oy/wisol-map-api"},
		{"ssh scp no .git", "git@github.com:Silon-Oy/wisol-map-api", "Silon-Oy/wisol-map-api"},
		{"https+.git", "https://github.com/Silon-Oy/wisol-map-api.git", "Silon-Oy/wisol-map-api"},
		{"https no .git", "https://github.com/Silon-Oy/wisol-map-api", "Silon-Oy/wisol-map-api"},
		{"ssh://", "ssh://git@github.com/wisol-oy/wisol-map-api.git", "wisol-oy/wisol-map-api"},
		{"trailing slash", "https://github.com/wisol-oy/wisol-map-api/", "wisol-oy/wisol-map-api"},
		{"non-github (gitlab)", "https://gitlab.com/Silon-Oy/wisol-map-api.git", ""},
		{"empty input", "", ""},
		{"extra path segments rejected", "https://github.com/Silon-Oy/wisol-map-api/extra/path", ""},
		{"missing repo half", "git@github.com:owner-only", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseOwnerRepoFromRemoteURL(tc.url); got != tc.want {
				t.Errorf("ParseOwnerRepoFromRemoteURL(%q) = %q, want %q", tc.url, got, tc.want)
			}
		})
	}
}

// Port of tests/test-multi-remote.sh section (b): resolve against a real clone.
func TestResolveRemoteToOwnerRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	work := t.TempDir()
	clone := filepath.Join(work, "clone")
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		if dir != "" {
			cmd.Dir = dir
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("", "init", "-q", clone)
	run(clone, "config", "user.email", "t@t.t")
	run(clone, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(clone, "f.txt"), []byte("v0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(clone, "add", "f.txt")
	run(clone, "commit", "-qm", "init")
	run(clone, "remote", "add", "origin", "git@github.com:Silon-Oy/wisol-map-api.git")
	run(clone, "remote", "add", "wisol", "https://github.com/wisol-oy/wisol-map-api.git")

	if got, ok := ResolveRemoteToOwnerRepo(clone, "origin"); !ok || got != "Silon-Oy/wisol-map-api" {
		t.Errorf("origin -> %q,%v want Silon-Oy/wisol-map-api,true", got, ok)
	}
	if got, ok := ResolveRemoteToOwnerRepo(clone, "wisol"); !ok || got != "wisol-oy/wisol-map-api" {
		t.Errorf("wisol -> %q,%v want wisol-oy/wisol-map-api,true", got, ok)
	}
	if got, ok := ResolveRemoteToOwnerRepo(clone, "does-not-exist"); ok || got != "" {
		t.Errorf("missing remote -> %q,%v want \"\",false", got, ok)
	}
}

// Port of tests/test-multi-remote.sh section (c): backward-compat shapes.
func TestRemoteLabelAndSessionSuffix(t *testing.T) {
	cases := []struct {
		remote      string
		n           int
		wantLabel   string
		wantSuffix  string
	}{
		{"origin", 5, "issue-5", "5"},
		{"", 5, "issue-5", "5"},
		{"wisol", 5, "wisol-issue-5", "wisol-issue-5"},
	}
	for _, tc := range cases {
		if got := RemoteLabel(tc.remote, tc.n); got != tc.wantLabel {
			t.Errorf("RemoteLabel(%q,%d) = %q, want %q", tc.remote, tc.n, got, tc.wantLabel)
		}
		if got := SessionSuffix(tc.remote, tc.n); got != tc.wantSuffix {
			t.Errorf("SessionSuffix(%q,%d) = %q, want %q", tc.remote, tc.n, got, tc.wantSuffix)
		}
	}
}
