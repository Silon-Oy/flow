package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestWriteCredentials_Mode0600 pins the acceptance invariant: the credentials
// file must end up at mode 0600 even if it already existed with a wider mode.
func TestWriteCredentials_Mode0600(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials")
	t.Setenv("FLOW_CREDENTIALS_PATH", path)

	// Pre-create the file world-readable so we exercise the explicit chmod
	// path (os.OpenFile with O_TRUNC ignores `perm` on an existing file).
	if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := writeCredentials("super-secret-token")
	if err != nil {
		t.Fatalf("writeCredentials: %v", err)
	}
	if got != path {
		t.Errorf("path = %q, want %q", got, path)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("perm = %v, want 0600", mode)
		}
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "super-secret-token") {
		t.Errorf("file content = %q, want it to contain the token", string(body))
	}
}

func TestCredentialsPath_OverrideOrder(t *testing.T) {
	t.Setenv("FLOW_CREDENTIALS_PATH", "/x/y/z")
	t.Setenv("XDG_CONFIG_HOME", "/should/lose")
	p, err := credentialsPath()
	if err != nil {
		t.Fatal(err)
	}
	if p != "/x/y/z" {
		t.Errorf("explicit override lost: %q", p)
	}

	t.Setenv("FLOW_CREDENTIALS_PATH", "")
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg")
	p, err = credentialsPath()
	if err != nil {
		t.Fatal(err)
	}
	if p != "/tmp/xdg/flow/credentials" {
		t.Errorf("XDG path wrong: %q", p)
	}
}
