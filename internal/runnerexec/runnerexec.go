// Package runnerexec builds the hardened `docker run` invocation for a per-run
// orchestrator container (§11.1, Taso 1 MVP). Keeping the flag set in one
// constructed-and-tested place makes the isolation posture an asserted
// invariant rather than a copy-pasted shell string.
//
// Invariants (§11.1):
//   - non-root (--user 65532:65532)
//   - read-only rootfs (--read-only) + writable scratch via --tmpfs
//   - all Linux capabilities dropped (--cap-drop=ALL)
//   - no privilege escalation (--security-opt no-new-privileges)
//   - egress only through the proxy network (--network flow-egress-<runner>)
//   - resource limits (--memory / --cpus / --pids-limit)
//   - the ONLY host mount is the per-run worktree (-v <worktree>:/work)
//   - NO docker socket is ever mounted (asserted by the test)
//   - the GitHub token is NEVER placed in the container env (§11.3)
package runnerexec

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Silon-Oy/flow/internal/secrets"
)

// Spec describes a single hardened run.
type Spec struct {
	Image        string // e.g. flow-orchestrator:<ver>
	RunID        string
	WorktreePath string // host path; mounted at /work (the only host mount)
	RunnerName   string // selects the per-runner egress network
	EgressProxy  string // e.g. http://egress-proxy:3128

	// Central-service callback envs. These are how the in-container orchestrator
	// reaches flowd to fetch its run config + push telemetry; both ride out via
	// the egress proxy. CentralToken is the flowd-issued runner token (lease-
	// scoped on the server) — NOT a GitHub token, so it does NOT violate §11.3
	// (which forbids GITHUB_TOKEN/GH_TOKEN crossing the trust boundary).
	CentralURL   string
	CentralToken string

	// Claude credential file on the host, mounted READ-ONLY (Model B, §11.5).
	// The container stages it into $HOME/.claude/.credentials.json itself; it is
	// never an env var and never crosses to the central service.
	ClaudeCredHostPath string

	// Env carries lease-scoped secrets resolved from secret_ref rows whose
	// delivery='env' (§9). They become -e KEY=VALUE flags in docker run. The
	// central is the producer (POST /v1/leases/acquire); the runner just hands
	// the map to Spec without inspecting it. §11.3 defense-in-depth: forbidden
	// keys (GITHUB_TOKEN, GH_TOKEN) are silently dropped at render time so a
	// future code path that bypasses the API validator still cannot leak GH
	// credentials into the container.
	Env map[string]string

	MemoryLimit string // e.g. "2g"
	CPULimit    string // e.g. "2"
	PidsLimit   int    // e.g. 512
}

// DockerArgs returns the full argv (starting at "docker") for the hardened run.
// Returned as a slice — never a shell string — so there is no quoting/injection
// surface.
func (s Spec) DockerArgs() []string {
	mem := s.MemoryLimit
	if mem == "" {
		mem = "2g"
	}
	cpus := s.CPULimit
	if cpus == "" {
		cpus = "2"
	}
	pids := s.PidsLimit
	if pids <= 0 {
		pids = 512
	}
	proxy := s.EgressProxy
	if proxy == "" {
		proxy = "http://egress-proxy:3128"
	}

	args := []string{
		"docker", "run", "--rm",
		"--user", "65532:65532",
		"--read-only",
		"--tmpfs", "/tmp",
		"--tmpfs", "/home/node:mode=1777", // writable HOME for claude-code (R4)
		"--cap-drop=ALL",
		"--security-opt", "no-new-privileges",
		"--network", s.egressNetwork(),
		"--memory", mem,
		"--cpus", cpus,
		fmt.Sprintf("--pids-limit=%d", pids),
		// The ONLY host mount: the per-run worktree.
		"-v", s.WorktreePath + ":/work",
		// Egress goes through the proxy (allow-list + log). The container never
		// carries a GitHub credential: the GitHub write path (push/PR) runs on
		// the trusted runner host after the container exits (§11.3 model C,
		// decision 19), so HTTPS_PROXY is the only network env the container
		// sees for the GitHub path.
		"-e", "HTTPS_PROXY=" + proxy,
		"-e", "HTTP_PROXY=" + proxy,
	}

	// Central-service callback (flowd URL + runner token). This is the only path
	// by which the in-container orchestrator can fetch its run config + push
	// telemetry. The token is flowd-issued (lease-scoped), not GitHub — §11.3
	// remains satisfied (asserted by the test).
	if s.CentralURL != "" {
		args = append(args, "-e", "FLOW_CENTRAL_URL="+s.CentralURL)
	}
	if s.CentralToken != "" {
		args = append(args, "-e", "FLOW_RUNNER_TOKEN="+s.CentralToken)
	}

	// Lease-scoped secret env (§9). Sorted keys for deterministic argv (testable,
	// and avoids order-noise in audit logs). Forbidden keys are dropped silently:
	// the API validator already rejects them at write time; the second filter
	// here is defense-in-depth so a future code path that bypasses the API still
	// cannot leak GH credentials into the container.
	for _, k := range sortedKeys(s.Env) {
		if secrets.IsForbiddenEnvKey(k) {
			continue
		}
		args = append(args, "-e", k+"="+s.Env[k])
	}

	// Claude credential as a read-only file mount (Model B). The credential is a
	// FILE, never an env var, and the central service never sees it (§11.5/§12).
	if s.ClaudeCredHostPath != "" {
		args = append(args, "-v", s.ClaudeCredHostPath+":/run/claude-credentials.json:ro")
	}

	args = append(args, s.Image, "orchestrate", s.RunID)
	return args
}

func (s Spec) egressNetwork() string {
	if s.RunnerName == "" {
		return "flow-egress"
	}
	return "flow-egress-" + s.RunnerName
}

// HasForbiddenMounts reports whether any -v argument mounts the Docker socket —
// the cardinal §11.1 violation. Used by the test to assert the invariant and by
// the runner as a defensive guard before launch.
func HasForbiddenMounts(args []string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-v" && strings.Contains(args[i+1], "docker.sock") {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
