package runnergit

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Silon-Oy/flow/internal/centralclient"
)

type fakeMinter struct {
	calls     int
	gotTenant string
	gotOrg    string
	token     string
	expiresAt time.Time
	err       error
}

func (m *fakeMinter) MintGitHubAppToken(_ context.Context, tenant, org string) (*centralclient.GitHubAppToken, error) {
	m.calls++
	m.gotTenant, m.gotOrg = tenant, org
	if m.err != nil {
		return nil, m.err
	}
	return &centralclient.GitHubAppToken{Token: m.token, ExpiresAt: m.expiresAt}, nil
}

// recordedCmd captures one runCmd invocation.
type recordedCmd struct {
	dir  string
	env  []string
	argv []string
}

func recorder(calls *[]recordedCmd) func(context.Context, string, []string, string, ...string) (string, error) {
	return func(_ context.Context, dir string, env []string, name string, args ...string) (string, error) {
		*calls = append(*calls, recordedCmd{dir: dir, env: env, argv: append([]string{name}, args...)})
		return "https://github.com/Silon-Oy/demo/pull/7\n", nil
	}
}

// repoWithRemote creates a throwaway git repo whose `origin` (or named remote)
// points at url, so ResolveRemoteToOwnerRepo has something real to resolve.
func repoWithRemote(t *testing.T, remote, url string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("remote", "add", remote, url)
	_ = os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x"), 0o644)
	return dir
}

const testToken = "ghs_test_secret_token_value"

func newGitOps(m Minter, calls *[]recordedCmd) *GitOps {
	return &GitOps{Remote: "origin", Minter: m, runCmd: recorder(calls)}
}

func TestPushTokenInEnvNeverArgv(t *testing.T) {
	dir := repoWithRemote(t, "origin", "git@github.com:Silon-Oy/demo.git")
	m := &fakeMinter{token: testToken, expiresAt: time.Now().Add(time.Hour)}
	var calls []recordedCmd
	g := newGitOps(m, &calls)

	if err := g.Push(context.Background(), dir, "auto-run/issue-34"); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if m.calls != 1 || m.gotOrg != "Silon-Oy" {
		t.Errorf("mint calls=%d org=%q, want 1 / Silon-Oy", m.calls, m.gotOrg)
	}
	if len(calls) != 1 {
		t.Fatalf("expected 1 command, got %d", len(calls))
	}
	cmd := calls[0]
	wantArgv := []string{"git", "push", "https://github.com/Silon-Oy/demo.git", "auto-run/issue-34"}
	if strings.Join(cmd.argv, " ") != strings.Join(wantArgv, " ") {
		t.Errorf("argv = %v, want %v", cmd.argv, wantArgv)
	}
	// Invariant 3 / criterion 5: the raw token must never appear in argv.
	for _, a := range cmd.argv {
		if strings.Contains(a, testToken) {
			t.Errorf("token leaked into argv: %q", a)
		}
	}
	wantHeader := "GIT_CONFIG_VALUE_0=Authorization: basic " +
		base64.StdEncoding.EncodeToString([]byte("x-access-token:"+testToken))
	if !containsEnv(cmd.env, wantHeader) {
		t.Errorf("env missing auth extraheader; env = %v", cmd.env)
	}
}

func TestOpenPRTokenViaGHTokenEnv(t *testing.T) {
	dir := repoWithRemote(t, "origin", "https://github.com/Silon-Oy/demo.git")
	m := &fakeMinter{token: testToken, expiresAt: time.Now().Add(time.Hour)}
	var calls []recordedCmd
	g := newGitOps(m, &calls)

	url, err := g.OpenPR(context.Background(), dir, "auto-run/issue-34", "", 34)
	if err != nil {
		t.Fatalf("OpenPR: %v", err)
	}
	if url != "https://github.com/Silon-Oy/demo/pull/7" {
		t.Errorf("url = %q", url)
	}
	cmd := calls[0]
	joined := strings.Join(cmd.argv, " ")
	for _, want := range []string{"gh pr create", "--repo Silon-Oy/demo", "--head auto-run/issue-34", "--base main"} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv %q missing %q", joined, want)
		}
	}
	if strings.Contains(joined, testToken) {
		t.Errorf("token leaked into argv: %q", joined)
	}
	if !containsEnv(cmd.env, "GH_TOKEN="+testToken) {
		t.Errorf("env missing GH_TOKEN; env = %v", cmd.env)
	}
}

func TestTokenCachedAcrossPushAndPR(t *testing.T) {
	dir := repoWithRemote(t, "origin", "git@github.com:Silon-Oy/demo.git")
	m := &fakeMinter{token: testToken, expiresAt: time.Now().Add(time.Hour)}
	var calls []recordedCmd
	g := newGitOps(m, &calls)

	if err := g.Push(context.Background(), dir, "b"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.OpenPR(context.Background(), dir, "b", "", 1); err != nil {
		t.Fatal(err)
	}
	if m.calls != 1 {
		t.Errorf("mint calls = %d, want 1 (cached within expiry margin)", m.calls)
	}
}

func TestTokenRemintedNearExpiry(t *testing.T) {
	dir := repoWithRemote(t, "origin", "git@github.com:Silon-Oy/demo.git")
	m := &fakeMinter{token: testToken, expiresAt: time.Now().Add(time.Minute)} // inside margin
	var calls []recordedCmd
	g := newGitOps(m, &calls)

	_ = g.Push(context.Background(), dir, "b")
	_ = g.Push(context.Background(), dir, "b")
	if m.calls != 2 {
		t.Errorf("mint calls = %d, want 2 (re-mint near expiry)", m.calls)
	}
}

func TestVerifyLeaseGatesSideEffects(t *testing.T) {
	dir := repoWithRemote(t, "origin", "git@github.com:Silon-Oy/demo.git")
	m := &fakeMinter{token: testToken, expiresAt: time.Now().Add(time.Hour)}
	var calls []recordedCmd
	g := newGitOps(m, &calls)
	g.VerifyLease = func(context.Context) error { return errors.New("lease lost") }

	if err := g.Push(context.Background(), dir, "b"); err == nil {
		t.Fatal("Push must fail when the lease is lost")
	}
	if _, err := g.OpenPR(context.Background(), dir, "b", "", 1); err == nil {
		t.Fatal("OpenPR must fail when the lease is lost")
	}
	if len(calls) != 0 {
		t.Errorf("no command may run without the lease; ran %v", calls)
	}
	if m.calls != 0 {
		t.Errorf("no token may be minted without the lease; minted %d", m.calls)
	}
}

func TestNonGitHubRemoteFailsClosed(t *testing.T) {
	dir := repoWithRemote(t, "origin", "https://gitlab.com/Silon-Oy/demo.git")
	m := &fakeMinter{token: testToken, expiresAt: time.Now().Add(time.Hour)}
	var calls []recordedCmd
	g := newGitOps(m, &calls)

	if err := g.Push(context.Background(), dir, "b"); err == nil {
		t.Fatal("Push must fail for a non-github remote (no personal-credential fallback)")
	}
	if len(calls) != 0 || m.calls != 0 {
		t.Errorf("nothing may run for an unresolvable remote (cmds=%d mints=%d)", len(calls), m.calls)
	}
}

func TestNonOriginRemoteMintsForItsOrg(t *testing.T) {
	dir := repoWithRemote(t, "acme", "git@github.com:acme-corp/widget.git")
	m := &fakeMinter{token: testToken, expiresAt: time.Now().Add(time.Hour)}
	var calls []recordedCmd
	g := &GitOps{Remote: "acme", Minter: m, runCmd: recorder(&calls)}

	if err := g.Push(context.Background(), dir, "auto-run/acme-issue-2"); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if m.gotOrg != "acme-corp" {
		t.Errorf("minted org = %q, want acme-corp (multi-org, #8 criterion)", m.gotOrg)
	}
	if got := calls[0].argv[2]; got != "https://github.com/acme-corp/widget.git" {
		t.Errorf("push URL = %q", got)
	}
}

func containsEnv(env []string, want string) bool {
	for _, e := range env {
		if e == want {
			return true
		}
	}
	return false
}
