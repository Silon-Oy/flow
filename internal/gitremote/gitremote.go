// Package gitremote provides multi-remote (multi-org) helpers ported from
// the run-issues bash orchestrator (lib/git-remote.sh).
//
// One local clone can target multiple GitHub orgs at once when a project lists
// more than one git remote. Per-run identity must then be namespaced by the
// remote so Silon-Oy/...#5 and wisol-oy/...#5 do not collide on the same
// lease / branch / run identifiers.
//
// Backward compatibility: when the remote is "origin" (or empty), every
// identifier collapses to the legacy shape so existing runs are not orphaned.
// Only non-origin remotes get the <remote>- prefix.
package gitremote

import (
	"os/exec"
	"strings"
)

// ParseOwnerRepoFromRemoteURL returns "owner/repo" for a github.com remote URL,
// or empty string on a parse failure. Pure — no network, no git invocation.
//
// Handles the shapes `git remote get-url` produces in practice:
//   - git@github.com:owner/repo.git           (SSH, scp-like)
//   - git@github.com:owner/repo               (SSH, no .git)
//   - https://github.com/owner/repo.git       (HTTPS)
//   - https://github.com/owner/repo           (HTTPS, no .git)
//   - ssh://git@github.com/owner/repo(.git)   (full SSH URL)
//
// A non-github.com host returns empty so the caller can fall back / warn.
func ParseOwnerRepoFromRemoteURL(url string) string {
	if url == "" {
		return ""
	}

	stripped := url
	for _, prefix := range []string{
		"ssh://git@github.com/",
		"https://github.com/",
		"http://github.com/",
		"git@github.com:",
	} {
		if strings.HasPrefix(stripped, prefix) {
			stripped = strings.TrimPrefix(stripped, prefix)
			break
		}
	}

	// If nothing matched, this is not a github.com URL.
	if stripped == url {
		return ""
	}

	stripped = strings.TrimSuffix(stripped, ".git")
	stripped = strings.TrimSuffix(stripped, "/")

	// We expect exactly owner/repo now — reject anything else (extra path
	// segments, missing repo half) so callers see a clean empty signal.
	parts := strings.Split(stripped, "/")
	if len(parts) != 2 {
		return ""
	}
	owner, repo := parts[0], parts[1]
	if owner == "" || repo == "" {
		return ""
	}
	return owner + "/" + repo
}

// ResolveRemoteToOwnerRepo returns "owner/repo" and true if the named remote
// exists in the clone and its URL parses; returns "" and false otherwise.
// Callers use the boolean to skip a missing/un-parseable remote with a warning
// instead of crashing the whole iteration.
func ResolveRemoteToOwnerRepo(repoRoot, remote string) (string, bool) {
	cmd := exec.Command("git", "-C", repoRoot, "remote", "get-url", remote)
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	ownerRepo := ParseOwnerRepoFromRemoteURL(strings.TrimSpace(string(out)))
	if ownerRepo == "" {
		return "", false
	}
	return ownerRepo, true
}

// RemoteLabel is the canonical "namespaced issue label" used to derive lease
// keys, run ids and branches.
//
//	origin (or ""): "issue-<N>"
//	other:          "<remote>-issue-<N>"
func RemoteLabel(remote string, issueNumber int) string {
	switch remote {
	case "origin", "":
		return "issue-" + itoa(issueNumber)
	default:
		return remote + "-issue-" + itoa(issueNumber)
	}
}

// SessionSuffix is the tmux/run identity suffix. Unlike RemoteLabel, origin
// emits just "<N>" so the legacy session names are preserved exactly.
//
//	origin (or ""): "<N>"
//	other:          "<remote>-issue-<N>"
func SessionSuffix(remote string, issueNumber int) string {
	switch remote {
	case "origin", "":
		return itoa(issueNumber)
	default:
		return remote + "-issue-" + itoa(issueNumber)
	}
}

func itoa(n int) string {
	// Small dedicated helper to avoid pulling strconv into a hot, simple path.
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
