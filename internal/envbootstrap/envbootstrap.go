// Package envbootstrap provides package-manager detection for the fail-fast
// env bootstrap gate (ported from lib/env-bootstrap.sh, orchestrate.sh S7b).
//
// Pure, side-effect-free detection resolved from lockfiles at the directory
// root. The orchestrator wraps these in the actual install + finalization.
package envbootstrap

import (
	"os"
	"path/filepath"
)

// DetectPackageManager returns the package manager to use for dir, resolved
// from the lockfile at the directory root:
//
//	"pnpm" — pnpm-lock.yaml present
//	"yarn" — yarn.lock present
//	"npm"  — package-lock.json present, OR a package.json with no recognized
//	         lockfile (npm install tolerates a missing lockfile)
//	""     — no package.json: nothing to bootstrap (the no-op case)
//
// Lockfile precedence (pnpm > yarn > npm) is deliberate: in a monorepo the
// root lockfile decides the manager.
func DetectPackageManager(dir string) string {
	if !fileExists(filepath.Join(dir, "package.json")) {
		return ""
	}
	switch {
	case fileExists(filepath.Join(dir, "pnpm-lock.yaml")):
		return "pnpm"
	case fileExists(filepath.Join(dir, "yarn.lock")):
		return "yarn"
	case fileExists(filepath.Join(dir, "package-lock.json")):
		return "npm"
	default:
		// package.json but no recognized lockfile: npm install works without one.
		return "npm"
	}
}

// DetectComposer returns "composer" when a composer.lock is present at the
// directory root, else "". Deliberately INDEPENDENT of DetectPackageManager: a
// Bedrock-style WordPress repo carries BOTH a composer.lock (PHP deps) and a JS
// lockfile, so the orchestrator runs both.
func DetectComposer(dir string) string {
	if fileExists(filepath.Join(dir, "composer.lock")) {
		return "composer"
	}
	return ""
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
