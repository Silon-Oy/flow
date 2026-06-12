package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolveToken pins the token resolution order shared by status/init:
// FLOW_TOKEN env wins, then the credentials file from `flowctl login`,
// otherwise a "not signed in" error.
func TestResolveToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials")
	t.Setenv("FLOW_CREDENTIALS_PATH", path)
	t.Setenv("FLOW_TOKEN", "")

	if _, err := resolveToken(); err == nil {
		t.Error("want error when neither FLOW_TOKEN nor credentials file exists")
	}

	if err := os.WriteFile(path, []byte("file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := resolveToken()
	if err != nil {
		t.Fatalf("resolveToken with credentials file: %v", err)
	}
	if got != "file-token" {
		t.Errorf("token = %q, want %q", got, "file-token")
	}

	t.Setenv("FLOW_TOKEN", "env-token")
	got, err = resolveToken()
	if err != nil {
		t.Fatalf("resolveToken with FLOW_TOKEN: %v", err)
	}
	if got != "env-token" {
		t.Errorf("token = %q, want %q (env must override file)", got, "env-token")
	}
}
