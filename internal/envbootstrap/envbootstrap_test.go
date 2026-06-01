package envbootstrap

import (
	"os"
	"path/filepath"
	"testing"
)

// touch creates an empty file, creating parent dirs as needed.
func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Table-driven port of tests/test-env-bootstrap.sh section (a).
func TestDetectPackageManager(t *testing.T) {
	root := t.TempDir()

	mk := func(sub string, files ...string) string {
		dir := filepath.Join(root, sub)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			touch(t, filepath.Join(dir, f))
		}
		return dir
	}

	cases := []struct {
		name  string
		files []string
		want  string
	}{
		{"none", nil, ""},
		{"pnpm", []string{"package.json", "pnpm-lock.yaml"}, "pnpm"},
		{"yarn", []string{"package.json", "yarn.lock"}, "yarn"},
		{"npm", []string{"package.json", "package-lock.json"}, "npm"},
		{"nolock", []string{"package.json"}, "npm"},
		{"multi", []string{"package.json", "pnpm-lock.yaml", "yarn.lock", "package-lock.json"}, "pnpm"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := mk(tc.name, tc.files...)
			if got := DetectPackageManager(dir); got != tc.want {
				t.Errorf("DetectPackageManager(%s) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

// Port of tests/test-env-bootstrap.sh section (a): independent composer signal.
func TestDetectComposer(t *testing.T) {
	root := t.TempDir()

	withLock := filepath.Join(root, "with")
	touch(t, filepath.Join(withLock, "composer.lock"))
	if got := DetectComposer(withLock); got != "composer" {
		t.Errorf("DetectComposer(with composer.lock) = %q, want composer", got)
	}

	without := filepath.Join(root, "without")
	if err := os.MkdirAll(without, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := DetectComposer(without); got != "" {
		t.Errorf("DetectComposer(no composer.lock) = %q, want empty", got)
	}

	// Bedrock case: composer + JS lockfile fire independently.
	bedrock := filepath.Join(root, "bedrock")
	touch(t, filepath.Join(bedrock, "composer.lock"))
	touch(t, filepath.Join(bedrock, "package.json"))
	touch(t, filepath.Join(bedrock, "package-lock.json"))
	if got := DetectComposer(bedrock); got != "composer" {
		t.Errorf("DetectComposer(bedrock) = %q, want composer", got)
	}
	if got := DetectPackageManager(bedrock); got != "npm" {
		t.Errorf("DetectPackageManager(bedrock) = %q, want npm", got)
	}
}
